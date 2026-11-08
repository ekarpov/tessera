package security_test

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

// listing is what GET /v1/datasets tells a token: each dataset and its access.
func (x *tenancy) listing(t *testing.T, token string) map[string]map[string]any {
	t.Helper()
	w := x.call(t, "GET", "/v1/datasets", token, "", nil)
	if w.Code != 200 {
		t.Fatal("the listing was refused:", w.Code)
	}
	var answer struct {
		Datasets []struct {
			ID     string         `json:"id"`
			Access map[string]any `json:"access"`
		} `json:"datasets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	listed := map[string]map[string]any{}
	for _, d := range answer.Datasets {
		listed[d.ID] = d.Access
	}
	return listed
}

// One sign-in reads everything shared with a person: every dataset where the
// account is a reader, within each share, and nothing it owns or writes. It
// reads and does nothing else. It grows when somebody shares and shrinks when
// a share ends, on the token already issued, and the approval says so.
func TestOneSignInReadsEverythingSharedAndOnlyThat(t *testing.T) {
	x := tenants(t)
	x.defineForm(t)
	const carol, carolPassword = "carol", "correct horse battery staple"
	when := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := x.runtime.Authority.SetReader(t.Context(), x.alice, x.aliceSet, carol, []string{question}, &when, carolPassword); err != nil {
		t.Fatal(err)
	}
	// Carol also writes Bob's dataset: that is hers to approve on its own.
	if err := x.runtime.Authority.AddPerson(carol, carolPassword, x.bobSet, store.Grants{Read: true, Write: true}, false); err != nil {
		t.Fatal(err)
	}

	// Discovery says the server offers it.
	var discovery struct {
		Capabilities []string `json:"server_capabilities"`
	}
	if w := x.call(t, "GET", "/.well-known/tessera", "", "", nil); json.Unmarshal(w.Body.Bytes(), &discovery) != nil || !strings.Contains(strings.Join(discovery.Capabilities, ","), "shared_reads") {
		t.Fatal("discovery does not advertise shared_reads:", discovery.Capabilities)
	}
	// Asked for and not ticked, nothing is approved: reading what is shared
	// is never the default.
	x.authorizeRefused(t, carol, carolPassword, url.Values{"shared_reads": {"true"}}, url.Values{"dataset": {""}})
	got := x.authorize(t, carol, carolPassword, url.Values{"shared_reads": {"true"}}, url.Values{"shared_reads": {"yes"}, "dataset": {""}, "write": {"yes"}, "erase": {"yes"}, "administer": {"yes"}})
	if !strings.Contains(got.Consent, "later, without asking you again") || !strings.Contains(got.Consent, "Shared with you now: 1.") {
		t.Fatal("the approval does not say it grows, or what it covers now")
	}
	token := got.Access

	// Malformed requests, from a client that is registered, are refused for
	// what they asked rather than for who asked.
	for _, bad := range []url.Values{{"shared_reads": {"yes"}}, {"shared_reads": {"1"}}} {
		query := url.Values{"client_id": {got.Client}, "redirect_uri": {"https://console.example/callback"}, "response_type": {"code"},
			"code_challenge_method": {"S256"}, "code_challenge": {strings.Repeat("A", 43)}, "state": {"s"}}
		for k, v := range bad {
			query[k] = v
		}
		w := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorize?"+query.Encode(), nil))
		if w.Code != 400 {
			t.Fatal("a malformed shared_reads request was answered", bad, w.Code)
		}
	}

	listed := x.listing(t, token)
	if len(listed) != 1 || listed[x.aliceSet] == nil {
		t.Fatal("a shared-reads installation reaches other than what is shared:", listed)
	}
	access := listed[x.aliceSet]
	if access["read"] != true || access["write"] != false || access["schema"] != false || access["erase"] != false || access["insert"] != false {
		t.Fatal("a shared-reads installation holds more than read, whatever its form ticked:", access)
	}
	if access["changes_at"] == nil {
		t.Fatal("a share's expiry did not reach the installation reading it")
	}
	if x.queries(t, token, question) != 200 {
		t.Fatal("a shared type could not be read")
	}
	if code := x.queries(t, token, response); code != 403 {
		t.Fatal("a type outside the share was reached:", code)
	}
	if w := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/objects/"+question, token, `{"fields":{}}`, nil); w.Code != 403 {
		t.Fatal("a shared-reads installation wrote:", w.Code)
	}
	if w := x.call(t, "GET", "/v1/datasets/"+x.bobSet+"/meta", token, "", nil); w.Code != 404 {
		t.Fatal("a shared-reads installation reached a dataset its account writes:", w.Code)
	}
	w := x.call(t, "GET", "/oauth/account", token, "", nil)
	var described struct {
		Installation map[string]bool `json:"installation"`
	}
	if json.Unmarshal(w.Body.Bytes(), &described) != nil || !described.Installation["shared_reads"] || described.Installation["administer"] {
		t.Fatal("the installation does not describe itself as reading what is shared:", w.Body.String())
	}

	// It grows, on the token already issued, when somebody shares.
	made := x.call(t, "POST", "/v1/datasets", x.bobTok, `{"display_name":"Bob's diary"}`, map[string]string{"Idempotency-Key": "7d4e28ba-2fa1-11d2-883f-0016d3cca427"})
	var created struct {
		DatasetID string `json:"dataset_id"`
	}
	if made.Code != 201 || json.Unmarshal(made.Body.Bytes(), &created) != nil {
		t.Fatal("a second dataset could not be made:", made.Code)
	}
	if err := x.runtime.Authority.SetReader(t.Context(), x.bob, created.DatasetID, carol, []string{"home.entry"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if listed = x.listing(t, token); len(listed) != 2 || listed[created.DatasetID] == nil {
		t.Fatal("a new share did not reach the installation:", listed)
	}

	// A refreshed token reads the same.
	refreshed := x.call(t, "POST", "/oauth/token", "", url.Values{"grant_type": {"refresh_token"}, "client_id": {got.Client}, "refresh_token": {got.Refresh}}.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	var tokens struct {
		Access string `json:"access_token"`
	}
	if refreshed.Code != 200 || json.Unmarshal(refreshed.Body.Bytes(), &tokens) != nil {
		t.Fatal("a shared-reads installation could not refresh:", refreshed.Code)
	}
	if listed = x.listing(t, tokens.Access); len(listed) != 2 {
		t.Fatal("a refreshed token reads other than what is shared:", listed)
	}

	// It shrinks at once when a share ends.
	if err := x.runtime.Authority.RemoveReader(t.Context(), x.alice, x.aliceSet, carol); err != nil {
		t.Fatal(err)
	}
	if listed = x.listing(t, tokens.Access); len(listed) != 1 || listed[x.aliceSet] != nil {
		t.Fatal("an ended share is still read:", listed)
	}
	if code := x.queries(t, tokens.Access, question); code != 404 {
		t.Fatal("an ended share still answers:", code)
	}

	// And revoking the installation ends all of it.
	whoever, err := x.runtime.Authority.Authenticate(t.Context(), tokens.Access)
	if err != nil {
		t.Fatal(err)
	}
	if err = x.runtime.Authority.RevokeAuthorization(t.Context(), whoever.Authorization); err != nil {
		t.Fatal(err)
	}
	if w := x.call(t, "GET", "/v1/datasets/"+created.DatasetID+"/meta", tokens.Access, "", nil); w.Code != 401 {
		t.Fatal("a revoked shared-reads installation still read:", w.Code)
	}
}

// One sign-in can hold a person's own dataset and read everything shared with
// them. Each part keeps its own rules: the dataset holds what was approved on
// it, and the shared part grows and shrinks without ever touching the
// dataset, or one the installation created.
func TestOneSignInHoldsItsOwnDatasetAndReadsWhatIsShared(t *testing.T) {
	x := tenants(t)
	x.defineForm(t)
	if err := x.runtime.Authority.SetReader(t.Context(), x.alice, x.aliceSet, x.bob, []string{question}, nil, ""); err != nil {
		t.Fatal(err)
	}
	// Not ticked: the dataset alone.
	alone := x.authorize(t, x.bob, password, url.Values{"shared_reads": {"true"}, "dataset": {x.bobSet}}, url.Values{"read": {"yes"}, "write": {"yes"}})
	if listed := x.listing(t, alone.Access); len(listed) != 1 || listed[x.bobSet] == nil {
		t.Fatal("an unticked box read what is shared:", listed)
	}
	both := x.authorize(t, x.bob, password, url.Values{"shared_reads": {"true"}, "dataset": {x.bobSet}},
		url.Values{"read": {"yes"}, "write": {"yes"}, "create_dataset": {"yes"}, "shared_reads": {"yes"}})
	if !strings.Contains(both.Consent, `name="shared_reads"`) || !strings.Contains(both.Consent, "without asking you again") {
		t.Fatal("the approval does not say it also reads what is shared, and later shares")
	}
	listed := x.listing(t, both.Access)
	if len(listed) != 2 || listed[x.bobSet]["write"] != true || listed[x.aliceSet]["read"] != true || listed[x.aliceSet]["write"] != false {
		t.Fatal("one sign-in does not hold its dataset and read what is shared:", listed)
	}
	w := x.call(t, "GET", "/oauth/account", both.Access, "", nil)
	var described struct {
		Installation map[string]bool `json:"installation"`
	}
	if json.Unmarshal(w.Body.Bytes(), &described) != nil || !described.Installation["shared_reads"] {
		t.Fatal("the installation does not say it reads what is shared:", w.Body.String())
	}
	// What the installation creates is its own, not the share's.
	made := x.call(t, "POST", "/v1/datasets", both.Access, `{"display_name":"Made here"}`, map[string]string{"Idempotency-Key": "8d4e28ba-2fa1-11d2-883f-0016d3cca427"})
	var created struct {
		DatasetID string `json:"dataset_id"`
	}
	if made.Code != 201 || json.Unmarshal(made.Body.Bytes(), &created) != nil {
		t.Fatal("the installation could not create:", made.Code)
	}
	// A share ending takes the shared dataset and nothing else.
	if err := x.runtime.Authority.RemoveReader(t.Context(), x.alice, x.aliceSet, x.bob); err != nil {
		t.Fatal(err)
	}
	listed = x.listing(t, both.Access)
	if listed[x.aliceSet] != nil || listed[x.bobSet]["write"] != true || listed[created.DatasetID] == nil {
		t.Fatal("ending a share took more than the share, or left it:", listed)
	}
}
