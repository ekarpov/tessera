package security_test

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/pkg/store"
)

// deleteOwn posts the self-service deletion form.
func (x *tenancy) deleteOwn(t *testing.T, account, secret string, erase ...string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"account": {account}, "password": {secret}, "erase": erase}
	r := httptest.NewRequest("POST", "/oauth/account/delete", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	return w
}

// A person deletes their own account with their own password. The datasets it
// erases are listed only to somebody who has proved that password, and are
// erased only once named back.
func TestAPersonDeletesTheirOwnAccountWithTheirPassword(t *testing.T) {
	x := tenants(t)
	if w := x.deleteOwn(t, "alice", "not the password"); w.Code != 401 || strings.Contains(w.Body.String(), x.aliceSet) {
		t.Fatal("a wrong password was answered with anything but a refusal:", w.Code, w.Body.String())
	}
	w := x.deleteOwn(t, "alice", password)
	var refused struct {
		Error    string   `json:"error"`
		Datasets []string `json:"datasets"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &refused)
	if w.Code != 409 || refused.Error != "erasure_required" || len(refused.Datasets) != 1 || refused.Datasets[0] != x.aliceSet {
		t.Fatal("an unconfirmed deletion was not refused with the datasets it erases:", w.Code, w.Body.String())
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok, "", nil).Code; code != 200 {
		t.Fatal("a refused deletion took the account's access", code)
	}
	if w = x.deleteOwn(t, "alice", password, x.aliceSet); w.Code != 200 || !strings.Contains(w.Body.String(), `"kept":[]`) {
		t.Fatal("a confirmed deletion was refused:", w.Code, w.Body.String())
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok, "", nil).Code; code != 401 {
		t.Fatal("a deleted account's credential still works", code)
	}
	if w = x.deleteOwn(t, "alice", password); w.Code != 401 {
		t.Fatal("a deleted account answered its old password:", w.Code)
	}
	// A service account has no password to prove, and a suspended person is
	// not the one who decides here.
	if err := x.runtime.Authority.SuspendAccount("bob"); err != nil {
		t.Fatal(err)
	}
	if w = x.deleteOwn(t, "bob", password, x.bobSet); w.Code != 401 {
		t.Fatal("a suspended account deleted itself:", w.Code)
	}
}

// The last server administrator cannot delete themselves: nothing could
// administer the server afterwards.
func TestTheLastServerAdministratorCannotDeleteThemselves(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	if w := x.deleteOwn(t, "operator", password); w.Code != 409 || !strings.Contains(w.Body.String(), "last_server_administrator") {
		t.Fatal("the last server administrator deleted themselves:", w.Code, w.Body.String())
	}
	if err := x.runtime.Authority.AddServerAdministrator("second", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	if w := x.deleteOwn(t, "operator", password); w.Code != 200 {
		t.Fatal("a server administrator with a colleague could not leave:", w.Code, w.Body.String())
	}
}

// A server administrator holding no data sees every account and can suspend
// or delete it, without seeing what datasets it holds or what it installed.
func TestAServerAdministratorWithoutDataManagesEveryAccount(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	operator := x.signIn(t, "operator", "", true)
	w := x.admin(t, "GET", "/overview", operator, "")
	var overview struct {
		Accounts []struct {
			Account  string                  `json:"account"`
			Datasets map[string]store.Grants `json:"datasets"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &overview); err != nil || w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	listed := map[string]bool{}
	for _, account := range overview.Accounts {
		listed[account.Account] = true
		if len(account.Datasets) != 0 {
			t.Fatal("the overview showed a dataset outside the administrator's scope:", account)
		}
	}
	if !listed["alice"] || !listed["bob"] {
		t.Fatal("the server administrator was not shown every account:", w.Body.String())
	}
	if strings.Contains(w.Body.String(), x.aliceSet) || strings.Contains(w.Body.String(), x.bobSet) {
		t.Fatal("the overview named a dataset the server administrator does not hold")
	}
	if w = x.admin(t, "POST", "/accounts/alice/suspend", operator, ""); w.Code != 204 {
		t.Fatal("the server administrator could not suspend an account:", w.Code, w.Body.String())
	}
	if w = x.admin(t, "POST", "/accounts/bob/delete", operator, ""); w.Code != 409 || !strings.Contains(w.Body.String(), x.bobSet) {
		t.Fatal("deleting the last administrator of a dataset was not refused with what it erases:", w.Code, w.Body.String())
	}
	if w = x.admin(t, "POST", "/accounts/bob/delete", operator, `{"erase":["`+x.bobSet+`"]}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"erased":["`+x.bobSet+`"]`) {
		t.Fatal("a confirmed deletion was refused:", w.Code, w.Body.String())
	}
}

// On the administration pages, deleting the last administrator of a dataset
// names what it would erase and goes ahead only when that is entered — as on
// the command line and the API, rather than a refusal that says nothing.
func TestTheAdministrationPagesConfirmAnErasingDeletion(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	w := x.adminPost(t, "/admin/principals", "operator", url.Values{"account": {"bob"}, "action": {"delete"}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "enter exactly: "+x.bobSet) {
		t.Fatal("the page did not name what deleting the account would erase:", w.Code)
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.bobSet+"/meta", x.bobTok, "", nil).Code; code != 200 {
		t.Fatal("an unconfirmed deletion took effect:", code)
	}
	w = x.adminPost(t, "/admin/principals", "operator", url.Values{"account": {"bob"}, "action": {"delete"}, "erase": {x.bobSet}})
	if w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "done=deleted") {
		t.Fatal("the confirmed deletion did not go ahead:", w.Code, w.Header().Get("Location"))
	}
	held, err := x.runtime.Engine.Provisioned(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range held {
		if d.ID == x.bobSet {
			t.Fatal("the confirmed deletion did not erase the dataset")
		}
	}
}
