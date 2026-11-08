// Package security_test holds the regression suite for the isolation and
// availability properties this deployment is required to keep. Every case
// here answers to a finding that was once real: the file is the record of
// what went wrong, so that a change which undoes one is a failure with a name
// rather than a rediscovery.
//
// It drives the composed server over HTTP and the administration surface over
// its own forms, because that is where the properties are promised. The unit
// tests beside each fix check the same rules against the code that implements
// them; a regression has to get past both.
//
// Several cases are sweeps rather than single reproductions — every protocol
// route, every administration page — because more than one finding here was a
// rule that held everywhere except the one place nobody had checked. A route
// added later and left out of the rule fails without anybody remembering to
// come back here.
//
// What is kept:
//
//   - A dataset a caller may not use answers exactly as one that does not
//     exist, on every route, and so does a content store.
//   - Neither listing mentions the other tenant.
//   - Administration is per dataset: an administrator of one learns nothing
//     of another, acts on nothing of another, and confers nothing they do not
//     hold themselves. An expired grant administers nothing.
//   - A membership an administrator can arrange gives them nothing, and a
//     server-level approval is administrable by nobody here.
//   - Withdrawal takes an installation's access off everything it can reach,
//     and needs standing over all of it.
//   - A share of one dataset is not a key to an item referenced only from
//     another.
//   - A service token is bound to the dataset its request named, and
//     revocation ends credentials everywhere.
//   - Work an unauthenticated caller can start does not stop the server
//     serving anybody else, and one address cannot spend the shared rate
//     budget.
//   - An operator upgrading keeps the administrator of their own server.
package security_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/standalone"
	"github.com/ekarpov/tessera/pkg/store"
)

const password = "correct horse battery staple"

// absent identifiers: well formed, and naming nothing at all. The three-answer
// rule turns on a caller being unable to tell these from identifiers that name
// somebody else's.
const (
	absentDataset = "s-0123456789abcdef0123456789abcdef"
	absentStore   = "c-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	absentObject  = "o-0123456789abcdef0123456789abcdef"
)

// tenancy is two people on one server, each with a dataset of their own. It is
// the shape every isolation question here is asked in: what may Alice, who
// administers her own dataset and nothing else, learn or do about Bob's.
type tenancy struct {
	runtime          *standalone.Runtime
	alice, bob       string
	aliceTok, bobTok string
	aliceSet, bobSet string
	aliceStore       string
	bobStore         string
}

func tenants(t *testing.T) *tenancy {
	t.Helper()
	config := standalone.Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite",
		EncryptedStorage: true, Content: true, DatasetCreation: true}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := standalone.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	held, err := r.Engine.Provisioned(t.Context())
	if err != nil || len(held) != 1 {
		t.Fatal("fixture should start with one dataset", err)
	}
	tenancy := &tenancy{runtime: r, alice: "alice", bob: "bob", aliceSet: held[0].ID}

	// Alice owns the dataset the server started with. Bob's is made the way a
	// second one is really made: by somebody who holds the right to create.
	if err = r.Authority.AddPerson("alice", password, tenancy.aliceSet,
		store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	if err = r.Authority.AddPerson("bob", password, "", store.Grants{}, true); err != nil {
		t.Fatal(err)
	}
	if tenancy.aliceTok, err = r.Authority.AddAutomation(t.Context(), "alice", tenancy.aliceSet); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := r.Authority.AddAutomation(t.Context(), "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	made := tenancy.call(t, "POST", "/v1/datasets", bootstrap, `{"display_name":"Bob"}`,
		map[string]string{"Idempotency-Key": "1b4e28ba-2fa1-11d2-883f-0016d3cca427"})
	if made.Code != 201 {
		t.Fatal("fixture could not create the second dataset", made.Code, made.Body.String())
	}
	var created struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(made.Body.Bytes(), &created) != nil || created.DatasetID == "" {
		t.Fatal("no identity in the creation response", made.Body.String())
	}
	tenancy.bobSet = created.DatasetID
	if tenancy.bobTok, err = r.Authority.AddAutomation(t.Context(), "bob", tenancy.bobSet); err != nil {
		t.Fatal(err)
	}
	tenancy.aliceStore = tenancy.contentStore(t, tenancy.aliceTok, tenancy.aliceSet)
	tenancy.bobStore = tenancy.contentStore(t, tenancy.bobTok, tenancy.bobSet)
	return tenancy
}

func (x *tenancy) contentStore(t *testing.T, token, dataset string) string {
	t.Helper()
	w := x.call(t, "GET", "/v1/datasets", token, "", nil)
	var listing struct {
		Datasets []struct {
			ID             string `json:"id"`
			ContentStoreID string `json:"content_store_id"`
		} `json:"datasets"`
	}
	if json.Unmarshal(w.Body.Bytes(), &listing) != nil {
		t.Fatal("dataset listing unreadable", w.Body.String())
	}
	for _, entry := range listing.Datasets {
		if entry.ID == dataset {
			return entry.ContentStoreID
		}
	}
	t.Fatal("no content store for", dataset, w.Body.String())
	return ""
}

func (x *tenancy) call(t *testing.T, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, reader)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	return w
}

// answer is a response reduced to what a caller can actually distinguish.
type answer struct {
	Status int
	Code   string
}

func (x *tenancy) answer(t *testing.T, method, path, token, body string) answer {
	t.Helper()
	w := x.call(t, method, path, token, body, nil)
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &envelope)
	return answer{Status: w.Code, Code: envelope.Error.Code}
}

func (x *tenancy) adminGet(t *testing.T, path, who string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.SetBasicAuth(who, password)
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	return w
}

func (x *tenancy) adminPost(t *testing.T, path, who string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form.Set("csrf", x.csrf(t, who))
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(who, password)
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	return w
}

func (x *tenancy) csrf(t *testing.T, who string) string {
	t.Helper()
	body := x.adminGet(t, "/admin/authorizations", who).Body.String()
	_, after, found := strings.Cut(body, `name="csrf" value="`)
	if !found {
		t.Fatal("no CSRF token on the authorizations page for", who)
	}
	token, _, _ := strings.Cut(after, `"`)
	return token
}

// seed puts a type, a record and an attachment into a dataset, so that the
// isolation questions have something real to be about.
func (x *tenancy) seed(t *testing.T, token, dataset, storeID string) (typeID, object, item string) {
	t.Helper()
	typeID = "security.record"
	if w := x.call(t, "POST", "/v1/datasets/"+dataset+"/meta", token,
		`{"type":{"id":"`+typeID+`","name":"Record","fields":[{"id":"photo","meta_id":"`+typeID+`","type":"media"},{"id":"n","meta_id":"`+typeID+`","type":"integer"}]}}`, nil); w.Code != 201 {
		t.Fatal("could not define a type", w.Code, w.Body.String())
	}
	begin := x.call(t, "POST", "/v1/stores/"+storeID+"/imports", token,
		`{"operation_id":"`+dataset[2:34]+`","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":10}`, nil)
	if begin.Code != 201 && begin.Code != 200 {
		t.Fatal("could not begin an import", begin.Code, begin.Body.String())
	}
	var opened struct {
		UploadID string `json:"upload_id"`
		URL      string `json:"url"`
	}
	if json.Unmarshal(begin.Body.Bytes(), &opened) != nil || opened.URL == "" {
		t.Fatal("no transfer url", begin.Body.String())
	}
	transfer := httptest.NewRequest("PATCH", strings.TrimPrefix(opened.URL, "https://tessera.example"), bytes.NewReader([]byte("key")))
	transfer.Header.Set("Content-Range", "bytes 0-2/3")
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, transfer)
	if w.Code != 204 && w.Code != 200 {
		t.Fatal("could not send bytes", w.Code, w.Body.String())
	}
	commit := x.call(t, "POST", "/v1/stores/"+storeID+"/imports/"+opened.UploadID+":commit", token, "", nil)
	if commit.Code != 200 && commit.Code != 201 {
		t.Fatal("could not commit an import", commit.Code, commit.Body.String())
	}
	var committed struct {
		ItemID string `json:"item_id"`
	}
	if json.Unmarshal(commit.Body.Bytes(), &committed) != nil || committed.ItemID == "" {
		t.Fatal("no item identity", commit.Body.String())
	}
	item = committed.ItemID
	insert := x.call(t, "POST", "/v1/datasets/"+dataset+"/objects/"+typeID, token,
		`{"fields":{"photo":{"type":"media","value":"`+storeID+`//`+item+`"},"n":{"type":"integer","value":1}}}`, nil)
	if insert.Code != 201 {
		t.Fatal("could not insert a record", insert.Code, insert.Body.String())
	}
	var written struct {
		Objects []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	if json.Unmarshal(insert.Body.Bytes(), &written) != nil || len(written.Objects) != 1 {
		t.Fatal("no record identity", insert.Body.String())
	}
	return typeID, written.Objects[0].ID, item
}
