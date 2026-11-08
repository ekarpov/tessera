package security_test

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Suspension stops every credential an account holds and keeps them all, so
// reinstating restores exactly what was there. Both halves are the promise:
// stopping only some credentials would leave an abusive account working from
// the ones missed, and discarding them would make it revocation by another
// name.
func TestSuspendingAnAccountStopsEveryCredentialAndReinstatingRestoresThem(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddPerson("member", password, x.aliceSet, store.Grants{Read: true, Write: true}, false); err != nil {
		t.Fatal(err)
	}
	laptop, err := x.runtime.Authority.AddAutomation(t.Context(), "member", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	phone, err := x.runtime.Authority.AddAutomation(t.Context(), "member", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	reach := func(token string) int {
		return x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", token, "", nil).Code
	}
	if reach(laptop) != 200 || reach(phone) != 200 {
		t.Fatal("the fixture's credentials do not work before suspension, so this proves nothing")
	}
	if err = x.runtime.Authority.SuspendAccount("member"); err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"laptop": laptop, "phone": phone} {
		if code := reach(token); code != 401 {
			t.Fatalf("the %s credential still works on a suspended account: %d", name, code)
		}
	}
	if _, err = x.runtime.Authority.AddAutomation(t.Context(), "member", x.aliceSet); err == nil {
		t.Fatal("a suspended account was issued a new credential")
	}
	if x.runtime.Authority.AuthenticateAdministrator("member", password) {
		t.Fatal("a suspended account signed in to administration")
	}
	if code := reach(x.aliceTok); code != 200 {
		t.Fatal("suspending one account affected another", code)
	}
	if err = x.runtime.Authority.ReinstateAccount("member"); err != nil {
		t.Fatal(err)
	}
	if reach(laptop) != 200 || reach(phone) != 200 {
		t.Fatal("reinstating did not restore the credentials the account held")
	}
	if err = x.runtime.Authority.ReinstateAccount("member"); p.ErrorCode(err) != "conflict" {
		t.Fatal("reinstating an account that is not suspended reported success:", err)
	}
}

// Deleting an account ends every credential it holds and retires its name.
//
// The name matters as much as the credentials. A receipt answers to its owning
// principal, so an account created later under the same name could ask about,
// and read, the outcomes the deleted one left in datasets that still exist.
func TestDeletingAnAccountEndsItsAccessAndRetiresItsName(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddPerson("leaver", password, x.aliceSet, store.Grants{Read: true, Write: true}, false); err != nil {
		t.Fatal(err)
	}
	token, err := x.runtime.Authority.AddAutomation(t.Context(), "leaver", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	if x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", token, "", nil).Code != 200 {
		t.Fatal("the fixture's credential does not work before deletion")
	}
	kept, err := x.runtime.Authority.DeleteAccount(t.Context(), "leaver", nil)
	if err != nil {
		t.Fatal(err)
	}
	// What the account wrote into somebody else's dataset stays there, and the
	// deletion says so rather than leaving it to be guessed.
	if len(kept) != 1 || kept[0].Dataset != x.aliceSet || kept[0].Reason != "member" {
		t.Fatal("the deletion did not report the dataset it left in place:", kept)
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", token, "", nil).Code; code != 401 {
		t.Fatal("a deleted account's credential still works", code)
	}
	if err = x.runtime.Authority.AddPerson("leaver", password, x.aliceSet, store.Grants{Read: true}, false); err == nil {
		t.Fatal("a deleted account's name was issued again")
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok, "", nil).Code; code != 200 {
		t.Fatal("deleting a member affected the dataset's owner", code)
	}
}

// Deleting the only administrator of a dataset would leave data nobody could
// administer or erase, so the deletion erases it — and only when the caller
// has named exactly that dataset. A deletion never erases what its caller did
// not see and confirm, and a dataset somebody else also administers stays.
func TestDeletingTheLastAdministratorErasesOnlyWhatWasConfirmed(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddPerson("watcher", password, x.aliceSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	watching, err := x.runtime.Authority.AddAutomation(t.Context(), "watcher", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	_, err = x.runtime.Authority.DeleteAccount(t.Context(), "alice", nil)
	var refusal *p.Error
	if !errors.As(err, &refusal) || refusal.Code != "conflict" || refusal.Details["reason"] != "erasure_required" {
		t.Fatal("the only administrator of a dataset was deleted unconfirmed:", err)
	}
	if listed, _ := refusal.Details["datasets"].([]string); len(listed) != 1 || listed[0] != x.aliceSet {
		t.Fatal("the refusal did not name the dataset the deletion erases:", refusal.Details)
	}
	for _, wrong := range [][]string{{x.bobSet}, {x.aliceSet, x.bobSet}} {
		if _, err = x.runtime.Authority.DeleteAccount(t.Context(), "alice", wrong); p.ErrorCode(err) != "conflict" {
			t.Fatal("a deletion confirming the wrong datasets went ahead:", wrong, err)
		}
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok, "", nil).Code; code != 200 {
		t.Fatal("a refused deletion still took the account's access", code)
	}
	if _, err = x.runtime.Authority.DeleteAccount(t.Context(), "alice", []string{x.aliceSet}); err != nil {
		t.Fatal("a confirmed deletion was refused:", err)
	}
	held, err := x.runtime.Engine.Provisioned(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range held {
		if d.ID == x.aliceSet {
			t.Fatal("the dataset the deleted account alone administered was not erased")
		}
	}
	// The handle that erased it holds nothing afterwards, so the emptied
	// replacement is reachable by nobody.
	eraser := store.Access{Principal: "account-deletion", Authorization: "account-deletion-" + x.aliceSet}
	if reach, err := x.runtime.Engine.Reach(t.Context(), eraser); err != nil || len(reach) != 0 {
		t.Fatal("the erasing handle still holds the replacement:", reach, err)
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", watching, "", nil).Code; code == 200 {
		t.Fatal("a reader still reaches an erased dataset")
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.bobSet+"/meta", x.bobTok, "", nil).Code; code != 200 {
		t.Fatal("deleting one account touched another's dataset", code)
	}
}

// A dataset another administrator also holds is kept, even when that other
// administrator is suspended: a suspension is reversible, and reinstating them
// must find their dataset there.
func TestDeletingAnAdministratorKeepsADatasetAnotherAdministers(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddPerson("deputy", password, x.aliceSet, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.SuspendAccount("deputy"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.runtime.Authority.DeleteAccount(t.Context(), "alice", []string{x.aliceSet}); p.ErrorCode(err) != "conflict" {
		t.Fatal("a deletion erased a dataset a suspended administrator still holds:", err)
	}
	kept, err := x.runtime.Authority.DeleteAccount(t.Context(), "alice", nil)
	if err != nil {
		t.Fatal("deleting an administrator with a deputy was refused:", err)
	}
	if len(kept) != 1 || kept[0].Dataset != x.aliceSet || kept[0].Reason != "another_administrator" {
		t.Fatal("the deletion did not say the dataset stayed with its other administrator:", kept)
	}
	if err := x.runtime.Authority.ReinstateAccount("deputy"); err != nil {
		t.Fatal(err)
	}
	deputy, err := x.runtime.Authority.AddAutomation(t.Context(), "deputy", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", deputy, "", nil).Code; code != 200 {
		t.Fatal("the reinstated administrator lost the dataset", code)
	}
}

// Standing to suspend or delete an account from the administration surface is
// administering every dataset it holds. Administering one dataset a person
// belongs to is not standing to cut them off from the others, and no
// administrator acts on their own account here, which would leave no way back.
func TestOnlyAnAdministratorOfEveryDatasetAnAccountHoldsMayActOnIt(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddPerson("member", password, x.aliceSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.AddPerson("shared", password, x.aliceSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.AddPerson("shared", password, x.bobSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	act := func(who, account, action string) string {
		w := x.adminPost(t, "/admin/principals", who, url.Values{"account": {account}, "action": {action}})
		if w.Code != 303 {
			t.Fatalf("%s %s %s answered %d", who, action, account, w.Code)
		}
		return w.Header().Get("Location")
	}
	for _, refused := range []struct{ who, account string }{
		{x.bob, "member"},   // an account in a dataset bob does not administer
		{x.alice, "shared"}, // an account that also holds bob's dataset
		{x.alice, x.alice},  // her own
	} {
		if got := act(refused.who, refused.account, "suspend"); !strings.Contains(got, "account-refused") {
			t.Fatalf("%s was allowed to suspend %s: %s", refused.who, refused.account, got)
		}
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok, "", nil).Code; code != 200 {
		t.Fatal("a refused suspension of her own account still took effect", code)
	}
	if got := act(x.alice, "member", "suspend"); !strings.Contains(got, "done=suspended") {
		t.Fatal("an administrator of every dataset the account holds could not suspend it:", got)
	}
	if !strings.Contains(x.adminGet(t, "/admin/principals", x.alice).Body.String(), "suspended") {
		t.Fatal("the accounts page does not show a suspended account as suspended")
	}
	if got := act(x.alice, "member", "delete"); !strings.Contains(got, "done=deleted") {
		t.Fatal("an administrator could not delete the account:", got)
	}
}

// A suspended service asking for a token with its own secret is told it is
// suspended; with a wrong secret it is told nothing more than any stranger.
func TestASuspendedServiceIsToldSoOnlyWithItsSecret(t *testing.T) {
	x := tenants(t)
	secret, err := x.runtime.Authority.AddService("feed", x.aliceSet, store.Grants{Read: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = x.runtime.Authority.SuspendAccount("feed"); err != nil {
		t.Fatal(err)
	}
	ask := func(secret string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(url.Values{"grant_type": {"client_credentials"}, "dataset": {x.aliceSet}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth("feed", secret)
		w := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(w, r)
		return w
	}
	if w := ask(secret); w.Code != 400 || !strings.Contains(w.Body.String(), `"account_suspended"`) {
		t.Fatal("a suspended service with its secret was not told it is suspended:", w.Code, w.Body.String())
	}
	if w := ask("not-the-secret"); w.Code != 401 || strings.Contains(w.Body.String(), "suspended") {
		t.Fatal("a wrong secret learned the service is suspended:", w.Code, w.Body.String())
	}
}
