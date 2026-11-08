package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Finding 29's step 6 over OAuth consent rather than an operator-minted token,
// because an application approved on a second phone is the ordinary way a
// guest gets another sign-in. The reporter asked for this path specifically:
// consent goes through the same minting function as a token, and that function
// is where the full view came from.
func TestAnApplicationApprovedAfterAShareInheritsIt(t *testing.T) {
	a, engine, dataset := authFixture(t)
	const visible, hidden = "test.weight", "test.pressure"
	owner := store.Access{Principal: "synthetic-owner", Authorization: "synthetic-owner"}
	if err := engine.SetAuthorization(t.Context(), dataset, owner, store.Authorization{
		Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		for _, typeID := range []string{visible, hidden} {
			if _, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddPerson("family", "synthetic-family-password", dataset, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	firstDevice, err := a.AddAutomation(t.Context(), "family", dataset)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := a.Authenticate(t.Context(), firstDevice)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.GrantRead(t.Context(), shared.Authorization, []string{visible}, nil); err != nil {
		t.Fatal(err)
	}

	// The second phone: the guest signs in and approves an application.
	a.data.Clients["phone"] = Client{ID: "phone", Name: "Second phone", Redirects: []string{"https://phone.example/callback"}}
	verifier := strings.Repeat("v", 43)
	hash := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {"phone"}, "redirect_uri": {"https://phone.example/callback"}, "response_type": {"code"},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "state": {"s"}, "dataset": {dataset}}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorize?"+query.Encode(), nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var id string
	var f flow
	for id, f = range a.flows {
		break
	}
	w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {f.CSRF}, "username": {"family"},
		"password": {"synthetic-family-password"}, "decision": {"continue"}}, w.Result().Cookies()[0])
	if w.Code != 200 {
		t.Fatal("the guest could not sign in", w.Code)
	}
	w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "decision": {"allow"}, "read": {"yes"}},
		w.Result().Cookies()[0])
	if w.Code != 303 {
		t.Fatal("the guest could not approve", w.Code, w.Body.String())
	}
	redirect, _ := url.Parse(w.Header().Get("Location"))
	w = formCall(a, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"phone"},
		"redirect_uri": {"https://phone.example/callback"}, "code": {redirect.Query().Get("code")}, "code_verifier": {verifier}}, nil)
	var tokens map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tokens) != nil {
		t.Fatal("no token for the second phone", w.Code)
	}
	phone, err := a.Authenticate(t.Context(), tokens["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if phone.Authorization == shared.Authorization {
		t.Fatal("the second phone reused the shared authorization, so this proves nothing about a new one")
	}

	readable := func(typeID string) error {
		return engine.Transact(t.Context(), phone, dataset, func(tx store.Tx) error {
			_, e := tx.Query(typeID, p.Query{}, 10)
			return e
		})
	}
	if err = readable(visible); err != nil {
		t.Fatal("the second phone cannot read the shared type", err)
	}
	if err = readable(hidden); err == nil {
		t.Fatal("an application approved after the share reads a type that was never shared — finding 29")
	}
}
