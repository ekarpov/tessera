package security_test

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

// Administration is per dataset. A server holds a dataset for each person on
// it, so owning one must not make somebody an administrator of the rest —
// which is what deriving the right from "erase anywhere" and then listing
// every dataset the storage holds had made it.
//
// Swept over the pages rather than checked at one, because the escalation was
// available at whichever page forgot.
var namesBob = regexp.MustCompile(`(^|[^A-Za-z0-9_-])bob($|[^A-Za-z0-9_-])`)

func TestAnAdministratorSeesNothingOfAnotherDataset(t *testing.T) {
	x := tenants(t)
	typeID, object, item := x.seed(t, x.bobTok, x.bobSet, x.bobStore)
	// Only identifiers distinctive enough to mean something. A record
	// identity is a small integer and appears in any page by accident, which
	// made the first version of this check fail on its own fixture.
	_ = object
	secrets := []string{x.bobSet, x.bobStore, typeID, item}

	for _, page := range []string{
		"/admin", "/admin/datasets", "/admin/principals", "/admin/authorizations", "/admin/schema",
		"/admin?dataset=" + x.bobSet, "/admin/schema?dataset=" + x.bobSet, "/admin/datasets?dataset=" + x.bobSet,
		"/admin/principals?dataset=" + x.bobSet,
	} {
		t.Run(page, func(t *testing.T) {
			w := x.adminGet(t, page, "alice")
			if w.Code >= 500 {
				t.Fatal("administration failed rather than refusing", w.Code, w.Body.String())
			}
			for _, secret := range secrets {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatal("a page told this administrator about another dataset", secret)
				}
			}
			// The name standing alone, not inside a random token: page
			// identifiers are base64url, which contains "bob" now and again.
			if namesBob.MatchString(w.Body.String()) {
				t.Fatal("a page named an account of another dataset")
			}
		})
	}
	// And Bob sees his own, so the sweep is not passing by showing nothing.
	if body := x.adminGet(t, "/admin/datasets", "bob").Body.String(); !strings.Contains(body, x.bobSet) {
		t.Fatal("an administrator was not shown the dataset they administer")
	}
}

// Naming a dataset on a form is the escalation the listing above conceals:
// the check the form was measured against had been the storage's listing and
// not the administrator's.
func TestAnAdministratorCannotActOnAnotherDataset(t *testing.T) {
	x := tenants(t)
	if w := x.adminPost(t, "/admin/principals", "alice", url.Values{"person": {"alice"},
		"password": {password}, "dataset": {x.bobSet}, "read": {"yes"}, "write": {"yes"}, "erase": {"yes"}}); !strings.Contains(w.Header().Get("Location"), "unknown-dataset") {
		t.Fatal("an administrator added themselves to another dataset", w.Header().Get("Location"))
	}
	if x.runtime.Authority.Administers("alice")[x.bobSet] {
		t.Fatal("the escalation landed")
	}
	// Bob's own credentials are not Alice's to end. She cannot name them
	// either, which the page above already refused to tell her.
	target := ""
	for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), "bob").Authorizations {
		if auth.Principal == "bob" && !auth.Revoked {
			target = auth.ID
		}
	}
	if target == "" {
		t.Fatal("fixture should hold an authorization of bob's")
	}
	for _, action := range []string{"revoke", "withdraw", "share"} {
		w := x.adminPost(t, "/admin/authorizations", "alice", url.Values{"action": {action},
			"authorization": {target}, "types": {"security.record"}})
		if w.Code != 400 {
			t.Fatal("an administrator acted on another dataset's credentials", action, w.Code)
		}
	}
	if _, err := x.runtime.Authority.Authenticate(t.Context(), x.bobTok); err != nil {
		t.Fatal("the other tenant's credentials stopped working anyway", err)
	}
}

// An expired grant is not a grant. The engine has read it that way since
// grants could expire; administration read the flag alone, so a grant that
// had run out still opened these pages.
func TestAnExpiredGrantDoesNotAdminister(t *testing.T) {
	x := tenants(t)
	past := time.Now().Add(-time.Hour)
	if err := x.runtime.Authority.GrantDataset("alice", x.aliceSet,
		store.Grants{Read: true, Write: true, Schema: true, Erase: true, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{"/admin", "/admin/datasets", "/admin/principals", "/admin/authorizations", "/admin/schema"} {
		if w := x.adminGet(t, page, "alice"); w.Code != 401 {
			t.Fatal("an expired grant still opened administration", page, w.Code)
		}
	}
}

// Adding an existing account to a dataset needs no password — that is what
// makes one account a member of two. So nothing about an account may become
// administrable merely because it now appears on the attacker's dataset, and
// an administrator may confer only what they themselves hold.
func TestAManufacturedMembershipConfersNothing(t *testing.T) {
	x := tenants(t)
	// Alice pulls Bob onto her dataset. She may: it is the ordinary way to
	// share, and it tells her nothing she did not already know about him.
	if w := x.adminPost(t, "/admin/principals", "alice", url.Values{"person": {"bob"},
		"password": {password}, "dataset": {x.aliceSet}, "read": {"yes"}}); !strings.Contains(w.Header().Get("Location"), "added") {
		t.Fatal("an existing account could not be shared onto a dataset", w.Header().Get("Location"))
	}
	// Bob's dataset is still his, and his server-level approval is nobody's
	// here: its reach is every dataset it ever created.
	if x.runtime.Authority.Administers("alice")[x.bobSet] {
		t.Fatal("sharing an account brought their dataset with it")
	}
	for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), "alice").Authorizations {
		may, err := x.runtime.Authority.MayAdminister(t.Context(), "alice", auth.ID)
		if err != nil {
			t.Fatal(err)
		}
		if may && auth.Principal == "bob" {
			t.Fatal("a manufactured membership reached the other tenant's credentials")
		}
	}
	// And Alice, who does not hold the server right, cannot hand it out.
	if w := x.adminPost(t, "/admin/principals", "alice", url.Values{"person": {"eve"},
		"password": {password}, "dataset": {x.aliceSet}, "read": {"yes"}, "create_dataset": {"yes"}}); !strings.Contains(w.Header().Get("Location"), "added") {
		t.Fatal("the account was not added", w.Header().Get("Location"))
	}
	if x.runtime.Authority.HoldsCreation("eve") {
		t.Fatal("an administrator conferred a server right they do not hold")
	}
}

// Withdrawal takes an installation's access off everything it can reach,
// because creating a dataset is how an installation grants itself access to
// one. Standing to withdraw therefore has to cover the whole reach.
func TestWithdrawalTakesTheWholeReachAndNeedsStandingOverIt(t *testing.T) {
	x := tenants(t)
	// Bob's installation reaches his dataset, and it created that dataset, so
	// the bootstrap approval reaches it too.
	bobs := ""
	for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), "bob").Authorizations {
		if auth.Principal == "bob" && auth.Dataset == x.bobSet {
			bobs = auth.ID
		}
	}
	if bobs == "" {
		t.Fatal("fixture should offer bob his own installation")
	}
	access := store.Access{Principal: "bob", Authorization: bobs}
	if _, err := x.runtime.Engine.Info(t.Context(), access, x.bobSet); err != nil {
		t.Fatal("fixture installation cannot read its own dataset", err)
	}
	if err := x.runtime.Authority.WithdrawGrants(t.Context(), bobs); err != nil {
		t.Fatal(err)
	}
	if _, err := x.runtime.Engine.Info(t.Context(), access, x.bobSet); err == nil {
		t.Fatal("withdrawal left the installation its access")
	}
	for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), "bob").Authorizations {
		if auth.ID == bobs && auth.CreateDataset {
			t.Fatal("a withdrawn installation kept the right to make itself a new dataset")
		}
	}
}

// An approval carrying a server right and no dataset belongs to the server
// rather than to any dataset, so nobody who administers a dataset may act on
// it. That left it administrable by nobody at all, which was honest and a
// hole: a credential that can create datasets on this server could not be
// ended from the surface that exists to end credentials.
//
// The right to administer the server is now held explicitly. Not derived —
// from holding erase somewhere, from administering every dataset, from being
// the only account. Each of those grows silently as the server does, and two
// of them were this week's escalations.
func TestOnlyTheServersAdministratorActsOnAServerLevelApproval(t *testing.T) {
	x := tenants(t)
	// Bob holds the server right to create and has a bootstrap approval that
	// carries it and names no dataset.
	bootstrap := ""
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := x.runtime.Authority.AddAutomation(t.Context(), "bob", ""); err != nil {
		t.Fatal(err)
	}
	for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), "operator").Authorizations {
		if auth.Principal == "bob" && auth.Dataset == "" && auth.CreateDataset {
			bootstrap = auth.ID
		}
	}
	if bootstrap == "" {
		t.Fatal("the server's administrator was not shown a server-level approval")
	}

	// Alice administers a dataset and not the server. It is not hers to see
	// or to end, and Bob owning a dataset does not make it his either.
	for _, who := range []string{"alice", "bob"} {
		for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), who).Authorizations {
			if auth.ID == bootstrap {
				t.Fatal("a server-level approval was shown to somebody who does not administer the server", who)
			}
		}
		may, err := x.runtime.Authority.MayAdminister(t.Context(), who, bootstrap)
		if err != nil {
			t.Fatal(err)
		}
		if may {
			t.Fatal("a dataset administrator could act on a server-level approval", who)
		}
	}

	// The server's administrator can, and it works through the form.
	may, err := x.runtime.Authority.MayAdminister(t.Context(), "operator", bootstrap)
	if err != nil || !may {
		t.Fatal("the server's administrator could not act on a server-level approval", err)
	}
	if w := x.adminPost(t, "/admin/authorizations", "operator", url.Values{"action": {"revoke"},
		"authorization": {bootstrap}}); w.Code != 303 {
		t.Fatal("the server's administrator could not revoke a server-level credential", w.Code, w.Body.String())
	}
	for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), "operator").Authorizations {
		if auth.ID == bootstrap && !auth.Revoked {
			t.Fatal("the credential was not revoked")
		}
	}
	// And an account holding only this right reaches the pages at all, which
	// requiring a dataset would have prevented.
	if w := x.adminGet(t, "/admin/authorizations", "operator"); w.Code != 200 {
		t.Fatal("the server's administrator could not open administration", w.Code)
	}
}
