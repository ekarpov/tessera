package security_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// An issuer outlives its first policy decisions: a study grows past its cap,
// closes, or loses its portal's secret. Each is the server administrator's to
// change, and what the issuer hands out is not.
func TestTheServerAdministratorManagesAnIssuersLifetime(t *testing.T) {
	x := tenants(t)
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	operator := x.signIn(t, "operator", "", true)
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "study",
		Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: []p.Meta{}},
		Grants:   store.Grants{Read: true, Write: true}, Cap: 1})
	if err != nil {
		t.Fatal(err)
	}
	outstanding, _, status := x.inviteNaming(t, "study", secret, "")
	if status != 201 {
		t.Fatal(status)
	}
	if _, _, status = x.inviteNaming(t, "study", secret, ""); status < 400 {
		t.Fatal("an issuer at its cap issued another invitation")
	}

	w := x.admin(t, "GET", "/issuers", operator, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"issued":1`) || strings.Contains(w.Body.String(), `"secret"`) {
		t.Fatal("the issuers were not listed, or their secret was:", w.Code, w.Body.String())
	}
	// Raised, it issues again.
	if w = x.admin(t, "PATCH", "/issuers/study", operator, `{"cap":3}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"cap":3`) {
		t.Fatal("the cap could not be raised:", w.Code, w.Body.String())
	}
	if _, _, status = x.inviteNaming(t, "study", secret, ""); status != 201 {
		t.Fatal("a raised cap did not let the issuer issue:", status)
	}
	// Zero would read as unbounded; a stopped issuer is a disabled one.
	if w = x.admin(t, "PATCH", "/issuers/study", operator, `{"cap":0}`); w.Code != 400 {
		t.Fatal("a cap of zero was accepted:", w.Code)
	}

	// Disabled, it issues nothing and what it issued cannot be redeemed.
	if w = x.admin(t, "PATCH", "/issuers/study", operator, `{"disabled":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, _, status = x.inviteNaming(t, "study", secret, ""); status < 400 {
		t.Fatal("a disabled issuer issued an invitation")
	}
	if got := x.redeem(t, outstanding, "late"); got < 400 {
		t.Fatal("an invitation from a disabled issuer was redeemed:", got)
	}
	if w = x.admin(t, "PATCH", "/issuers/study", operator, `{"disabled":false}`); w.Code != 200 {
		t.Fatal(w.Code)
	}

	// Rotated, the old secret stops and the new one works; invitations already
	// out are the invitees', and still redeem.
	w = x.admin(t, "POST", "/issuers/study/secret", operator, "")
	var rotated struct {
		Secret string `json:"secret"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &rotated); err != nil || w.Code != 200 || rotated.Secret == "" || rotated.Secret == secret {
		t.Fatal("the secret could not be rotated:", w.Code)
	}
	if _, _, status = x.inviteNaming(t, "study", secret, ""); status < 400 {
		t.Fatal("the old secret still issued an invitation")
	}
	if _, _, status = x.inviteNaming(t, "study", rotated.Secret, ""); status != 201 {
		t.Fatal("the new secret did not issue:", status)
	}
	if got := x.redeem(t, outstanding, "punctual"); got != 201 {
		t.Fatal("an invitation issued before the rotation stopped working:", got)
	}

	// None of it is anybody else's to do, a dataset's administrator included.
	alice := x.signIn(t, x.alice, x.aliceSet, true)
	for _, attempt := range [][3]string{{"GET", "/issuers", ""}, {"PATCH", "/issuers/study", `{"cap":100}`}, {"POST", "/issuers/study/secret", ""}} {
		if w = x.admin(t, attempt[0], attempt[1], alice, attempt[2]); w.Code != 403 {
			t.Fatal("a dataset administrator managed an issuer:", attempt, w.Code)
		}
	}
	if w = x.admin(t, "PATCH", "/issuers/nobody", operator, `{"cap":2}`); w.Code != 404 {
		t.Fatal("changing an issuer that does not exist was not not_found:", w.Code)
	}
}
