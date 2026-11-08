package security_test

import (
	"encoding/json"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// adminInvite issues an invitation through the administration API on token's
// installation, answering the code and the status.
func (x *tenancy) adminInvite(t *testing.T, issuer, token string) (string, int) {
	t.Helper()
	w := x.admin(t, "POST", "/issuers/"+issuer+"/invitations", token, "")
	var answer struct {
		Invitation string `json:"invitation"`
	}
	if w.Code == 201 && (json.Unmarshal(w.Body.Bytes(), &answer) != nil || answer.Invitation == "") {
		t.Fatal("an issued invitation carries no code", w.Body.String())
	}
	return answer.Invitation, w.Code
}

// An administration tool that is an application issues invitations on the
// administrator's own sign-in, so no issuer secret lives on a phone. It takes
// what configuring the issuer takes, the issuer's cap and disabling still
// apply, and what an installation issued stops being redeemable when the
// installation is revoked.
func TestAnAdministratorIssuesInvitationsOnTheirOwnSignIn(t *testing.T) {
	x := tenants(t)
	const typeID = "home.track"
	types := []p.Meta{{ID: typeID, Name: "Track", Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}}
	if _, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "home",
		Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: types},
		Grants:   store.Grants{Read: true, Write: true}, Cap: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "household", Datasets: []string{x.aliceSet}, Grants: store.Grants{Read: true}}); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.AddServerAdministrator("steward", password, x.aliceSet, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	operator := x.signIn(t, "operator", "", true)

	// Not a server administrator; and a server administrator's sign-in not
	// approved to administer.
	if _, status := x.adminInvite(t, "home", x.signIn(t, x.alice, x.aliceSet, true)); status != 403 {
		t.Fatal("a dataset owner who does not administer the server issued an invitation:", status)
	}
	notAdministering := x.signIn(t, "steward", x.aliceSet, false)
	if _, status := x.adminInvite(t, "home", notAdministering); status != 403 {
		t.Fatal("a sign-in not approved to administer issued an invitation:", status)
	}
	// The authority holds the rule itself, not only the route in front of it:
	// an installation not approved to administer is refused there too.
	plain, err := x.runtime.Authority.Authenticate(t.Context(), notAdministering)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = x.runtime.Authority.IssueInvitationAs(plain, "home", ""); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("an installation not approved to administer issued an invitation:", err)
	}
	// An issuer into existing data takes administering it.
	if _, status := x.adminInvite(t, "household", operator); status != 403 {
		t.Fatal("a server administrator without data invited people into a dataset:", status)
	}
	code, status := x.adminInvite(t, "household", x.signIn(t, "steward", x.aliceSet, true))
	if status != 201 || x.redeem(t, code, "relative") != 201 {
		t.Fatal("the dataset's administrator could not invite into it:", status)
	}
	if _, status = x.adminInvite(t, "nobody", operator); status != 404 {
		t.Fatal("an unknown issuer was answered", status)
	}

	// A template issuer reaches no data that exists, and its invitation works
	// as one issued with its secret does.
	code, status = x.adminInvite(t, "home", operator)
	if status != 201 || x.redeem(t, code, "member-1") != 201 {
		t.Fatal("a server administrator could not invite from a template:", status)
	}

	// What an installation issued ends with it.
	pending, status := x.adminInvite(t, "home", operator)
	if status != 201 {
		t.Fatal(status)
	}
	access, err := x.runtime.Authority.Authenticate(t.Context(), operator)
	if err != nil {
		t.Fatal(err)
	}
	if err = x.runtime.Authority.RevokeAuthorization(t.Context(), access.Authorization); err != nil {
		t.Fatal(err)
	}
	if got := x.redeem(t, pending, "member-2"); got != 404 {
		t.Fatal("an invitation outlived the installation that issued it:", got)
	}

	// The cap counts every invitation issued, whichever way it was issued.
	again := x.signIn(t, "operator", "", true)
	if _, status = x.adminInvite(t, "home", again); status != 201 {
		t.Fatal("the third invitation within the cap was refused:", status)
	}
	if _, status = x.adminInvite(t, "home", again); status != 409 {
		t.Fatal("an administrator issued past the issuer's cap:", status)
	}
	disabled := true
	raised := 10
	if _, err = x.runtime.Authority.ChangeIssuer("home", identity.IssuerChange{Cap: &raised, Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, status = x.adminInvite(t, "home", again); status != 409 {
		t.Fatal("an administrator issued from a disabled issuer:", status)
	}
}
