package security_test

import (
	"encoding/json"
	"net/url"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const (
	response = "form.response"
	question = "form.question"
	followUp = "form.response.follow_up"
)

// defineForm gives alice's dataset a question type, a response type and a
// subtype of the response, and one question.
func (x *tenancy) defineForm(t *testing.T) {
	t.Helper()
	for _, meta := range []string{
		`{"type":{"id":"form.question","name":"Question","fields":[{"id":"text","meta_id":"form.question","type":"text"}]}}`,
		`{"type":{"id":"form.response","name":"Response","fields":[{"id":"answer","meta_id":"form.response","type":"text"}]}}`,
		`{"type":{"id":"form.response.follow_up","parent_id":"form.response","name":"Follow-up","fields":[]}}`,
	} {
		if w := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok, meta, nil); w.Code != 201 {
			t.Fatal("the form could not be defined:", w.Code, w.Body.String())
		}
	}
	if w := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/objects/"+question, x.aliceTok, `{"fields":{"text":{"type":"text","value":"How are you?"}}}`, nil); w.Code != 201 {
		t.Fatal("the question could not be written:", w.Code)
	}
}

// contribute adds a response as token and answers the status and the identity.
func (x *tenancy) contribute(t *testing.T, token, typeID string) (int, string) {
	t.Helper()
	w := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/objects/"+typeID, token, `{"fields":{"answer":{"type":"text","value":"fine"}}}`, nil)
	var added struct {
		Objects []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &added)
	if len(added.Objects) == 1 {
		return w.Code, added.Objects[0].ID
	}
	return w.Code, ""
}

func (x *tenancy) queries(t *testing.T, token, typeID string) int {
	t.Helper()
	return x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/objects/"+typeID+":query", token, `{"query":{}}`, nil).Code
}

// A device reporting into somebody's dataset adds, and has no hold on what it
// added: it cannot read it, change it or delete it, reaches no other type, and
// is told as much by its listing.
func TestAServiceContributesAndHoldsNothing(t *testing.T) {
	x := tenants(t)
	x.defineForm(t)
	secret, err := x.runtime.Authority.AddService("sensor", "", store.Grants{})
	if err != nil {
		t.Fatal(err)
	}
	if err = x.runtime.Authority.SetContributor(t.Context(), x.alice, x.aliceSet, "sensor", []string{response}, nil, ""); err != nil {
		t.Fatal(err)
	}
	token, status := x.serviceToken(t, "sensor", secret, x.aliceSet)
	if status != 200 {
		t.Fatal("a contributing service could not get a token:", status)
	}
	code, id := x.contribute(t, token, response)
	if code != 201 || id == "" {
		t.Fatal("a contributing service could not add:", code)
	}
	path := "/v1/datasets/" + x.aliceSet + "/objects/" + response + "/" + id
	for _, refused := range []struct{ method, path, body string }{
		{"GET", path, ""},
		{"PATCH", path, `{"fields":{"answer":{"type":"text","value":"changed"}}}`},
		{"DELETE", path, ""},
		{"POST", "/v1/datasets/" + x.aliceSet + "/objects/" + response + ":query", `{"query":{}}`},
		{"POST", "/v1/datasets/" + x.aliceSet + "/objects/" + question + ":query", `{"query":{}}`},
		{"POST", "/v1/datasets/" + x.aliceSet + "/objects/" + followUp, `{"fields":{}}`},
		{"POST", "/v1/datasets/" + x.aliceSet + "/objects/" + question, `{"fields":{}}`},
	} {
		if w := x.call(t, refused.method, refused.path, token, refused.body, nil); w.Code != 403 {
			t.Fatal("a contributor reached beyond adding:", refused.method, refused.path, w.Code)
		}
	}
	if seen := x.typesSeen(t, x.aliceSet, token); len(seen) != 1 || !seen[response] {
		t.Fatal("a contributor was shown the schema of types it cannot add to:", seen)
	}
	if w := x.call(t, "GET", path, x.aliceTok, "", nil); w.Code != 200 {
		t.Fatal("the owner does not hold what was contributed:", w.Code)
	}
	// Nor does it reach anybody else's dataset.
	if _, status = x.serviceToken(t, "sensor", secret, x.bobSet); status == 200 {
		t.Fatal("a contributor got a token for a dataset it does not contribute to")
	}
}

// Reading and adding are separate standings on one account, and taking one
// away leaves the other — on the installation already signed in, not only on
// the next one.
func TestContributingAndReadingAreHeldApart(t *testing.T) {
	x := tenants(t)
	x.defineForm(t)
	if err := x.runtime.Authority.SetReader(t.Context(), x.alice, x.aliceSet, "carol", []string{question}, nil, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.SetContributor(t.Context(), x.alice, x.aliceSet, "carol", []string{response}, nil, ""); err != nil {
		t.Fatal(err)
	}
	token := x.signInApproving(t, "carol", "correct horse battery staple", x.aliceSet, url.Values{"read": {"yes"}, "insert": {"yes"}})
	if x.queries(t, token, question) != 200 {
		t.Fatal("a reader who contributes could not read what it was shown")
	}
	if code, _ := x.contribute(t, token, response); code != 201 {
		t.Fatal("a reader who contributes could not add:", code)
	}
	if x.queries(t, token, response) != 403 {
		t.Fatal("contributing to a type let the contributor read it")
	}
	// Reading something does not make a representation of what it added
	// readable: the row is handed back only to a caller whose view holds it.
	w := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/objects/"+response, token, `{"fields":{"answer":{"type":"text","value":"fine"}}}`, map[string]string{"Prefer": "return=representation"})
	if w.Code != 201 || w.Header().Get("Preference-Applied") != "" || json.Valid(w.Body.Bytes()) && x.carries(t, w.Body.Bytes(), "record_set") {
		t.Fatal("a reader who contributes was handed a row it cannot read:", w.Code, w.Header().Get("Preference-Applied"))
	}

	if err := x.runtime.Authority.RemoveReader(t.Context(), x.alice, x.aliceSet, "carol"); err != nil {
		t.Fatal(err)
	}
	if x.queries(t, token, question) != 403 {
		t.Fatal("removing a reader who contributes left them reading")
	}
	if code, _ := x.contribute(t, token, response); code != 201 {
		t.Fatal("removing a reader who contributes stopped them adding:", code)
	}

	if err := x.runtime.Authority.SetReader(t.Context(), x.alice, x.aliceSet, "carol", []string{question}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.RemoveContributor(t.Context(), x.alice, x.aliceSet, "carol"); err != nil {
		t.Fatal(err)
	}
	if code, _ := x.contribute(t, token, response); code != 403 {
		t.Fatal("removing a contributor who reads left them adding:", code)
	}
	if x.queries(t, token, question) != 200 {
		t.Fatal("removing a contributor who reads stopped them reading")
	}
	if err := x.runtime.Authority.RemoveReader(t.Context(), x.alice, x.aliceSet, "carol"); err != nil {
		t.Fatal(err)
	}
	if w := x.call(t, "GET", "/v1/datasets", token, "", nil); w.Code == 200 && x.listsDataset(t, w.Body.Bytes(), x.aliceSet) {
		t.Fatal("an account with no standing left still lists the dataset")
	}
}

// An installation holds what it was approved for and no more, whatever the
// account comes to hold: one approved to read does not start adding because
// its account became a contributor.
func TestAnInstallationAddsOnlyWhenItWasApprovedTo(t *testing.T) {
	x := tenants(t)
	x.defineForm(t)
	if err := x.runtime.Authority.SetReader(t.Context(), x.alice, x.aliceSet, "carol", []string{question}, nil, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	readOnly := x.signInApproving(t, "carol", "correct horse battery staple", x.aliceSet, url.Values{"read": {"yes"}})
	either := x.signInApproving(t, "carol", "correct horse battery staple", x.aliceSet, url.Values{"read": {"yes"}, "insert": {"yes"}})
	if err := x.runtime.Authority.SetContributor(t.Context(), x.alice, x.aliceSet, "carol", []string{response}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if code, _ := x.contribute(t, readOnly, response); code != 403 {
		t.Fatal("an installation approved to read began adding:", code)
	}
	if code, _ := x.contribute(t, either, response); code != 201 {
		t.Fatal("an installation approved to add could not, once its account contributed:", code)
	}
	// And one approved to read after the account contributes is no different.
	later := x.signInApproving(t, "carol", "correct horse battery staple", x.aliceSet, url.Values{"read": {"yes"}})
	if code, _ := x.contribute(t, later, response); code != 403 {
		t.Fatal("an installation approved only to read was given adding:", code)
	}
}

func (x *tenancy) carries(t *testing.T, body []byte, member string) bool {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		t.Fatal(err)
	}
	_, present := members[member]
	return present
}

// Naming a contributor lets somebody write into a dataset, so it takes
// administering that dataset — not administering the server, and not holding
// some other dataset.
func TestOnlyADatasetsAdministratorNamesItsContributors(t *testing.T) {
	x := tenants(t)
	x.defineForm(t)
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	operator := x.signIn(t, "operator", "", true)
	if w := x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/contributors/operator", operator, `{"types":["form.response"]}`); w.Code != 403 {
		t.Fatal("a server administrator without data made themselves a contributor:", w.Code)
	}
	if w := x.admin(t, "GET", "/datasets/"+x.aliceSet+"/contributors", operator, ""); w.Code != 403 {
		t.Fatal("a server administrator without data listed a dataset's contributors:", w.Code)
	}
	if err := x.runtime.Authority.SetContributor(t.Context(), x.bob, x.aliceSet, "bob", []string{response}, nil, ""); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("somebody else's owner named a contributor:", err)
	}
	if err := x.runtime.Authority.SetContributor(t.Context(), x.alice, x.aliceSet, x.alice, []string{response}, nil, ""); p.ErrorCode(err) != "conflict" {
		t.Fatal("a writer was narrowed to a contributor:", err)
	}
	owner := x.signIn(t, x.alice, x.aliceSet, true)
	if w := x.admin(t, "PUT", "/datasets/"+x.aliceSet+"/contributors/respondent", owner, `{"types":["form.response"],"password":"correct horse battery staple"}`); w.Code != 204 {
		t.Fatal("the dataset's administrator could not name a contributor:", w.Code, w.Body.String())
	}
	w := x.admin(t, "GET", "/datasets/"+x.aliceSet+"/contributors/respondent", owner, "")
	var named struct {
		Account string   `json:"account"`
		Types   []string `json:"types"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &named) != nil || named.Account != "respondent" || len(named.Types) != 1 || named.Types[0] != response {
		t.Fatal("a contributor is not reported as named:", w.Code, w.Body.String())
	}
	if w = x.admin(t, "DELETE", "/datasets/"+x.aliceSet+"/contributors/respondent", owner, ""); w.Code != 204 {
		t.Fatal("the dataset's administrator could not remove a contributor:", w.Code)
	}
	if w = x.admin(t, "GET", "/datasets/"+x.aliceSet+"/contributors/respondent", owner, ""); w.Code != 404 {
		t.Fatal("a removed contributor is still reported:", w.Code)
	}
}

func (x *tenancy) listsDataset(t *testing.T, body []byte, dataset string) bool {
	t.Helper()
	var listing struct {
		Datasets []struct {
			ID string `json:"id"`
		} `json:"datasets"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatal(err)
	}
	for _, d := range listing.Datasets {
		if d.ID == dataset {
			return true
		}
	}
	return false
}
