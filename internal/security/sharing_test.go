package security_test

import (
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// A share limits the guest, not one of the guest's sign-ins. Review finding 29.
//
// Reproduced by a consumer on the running server: an administrator shared two
// types' worth of a dataset with a family member, the family member's session
// could read only those, and the family member's next sign-in — a fresh token,
// or an app approved on another device — read the whole dataset. The share had
// been set on the one authorization it named, and every later one was minted
// from the account with a full view.
//
// The steps below are the reporter's, in their order: the second sign-in after
// the share is step 6, and it is the one that matters.
func TestAShareLimitsTheGuestNotOneSignIn(t *testing.T) {
	x := tenants(t)
	visible, hidden := "security.weight", "security.pressure"
	for _, typeID := range []string{visible, hidden} {
		if w := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok,
			`{"type":{"id":"`+typeID+`","name":"Record","fields":[{"id":"n","meta_id":"`+typeID+`","type":"integer"}]}}`, nil); w.Code != 201 {
			t.Fatal("could not define", typeID, w.Code, w.Body.String())
		}
		if w := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/objects/"+typeID, x.aliceTok,
			`{"fields":{"n":{"type":"integer","value":1}}}`, nil); w.Code != 201 {
			t.Fatal("could not write", typeID, w.Code)
		}
	}
	reads := func(token, typeID string) int {
		return x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/objects/"+typeID+":query", token, `{"query":{"fields":["n"]}}`, nil).Code
	}

	if err := x.runtime.Authority.AddPerson("family", password, x.aliceSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	// Two sign-ins before the share: two devices. Narrowing one and not the
	// other would leave the guest reading everything from the second.
	first, err := x.runtime.Authority.AddAutomation(t.Context(), "family", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	second, err := x.runtime.Authority.AddAutomation(t.Context(), "family", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	if reads(first, hidden) != 200 {
		t.Fatal("before the share a reader should read every type; the fixture proves nothing otherwise")
	}
	named := ""
	for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), "alice").Authorizations {
		if auth.Principal == "family" {
			named = auth.ID
			break
		}
	}
	if err = x.runtime.Authority.GrantRead(t.Context(), named, []string{visible}, nil); err != nil {
		t.Fatal(err)
	}

	// Every sign-in the guest already had.
	for i, token := range []string{first, second} {
		if code := reads(token, visible); code != 200 {
			t.Fatalf("existing sign-in %d lost the shared type: %d", i+1, code)
		}
		if code := reads(token, hidden); code < 400 {
			t.Fatalf("existing sign-in %d still reads a type that was not shared: %d", i+1, code)
		}
	}
	// And the one that used to read everything: a sign-in minted afterwards.
	later, err := x.runtime.Authority.AddAutomation(t.Context(), "family", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	if code := reads(later, visible); code != 200 {
		t.Fatal("a sign-in after the share cannot read the shared type", code)
	}
	if code := reads(later, hidden); code < 400 {
		t.Fatal("a sign-in after the share reads a type that was never shared — finding 29", code)
	}
	if code := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/objects/"+visible, later,
		`{"fields":{"n":{"type":"integer","value":2}}}`, nil).Code; code < 400 {
		t.Fatal("a guest's later sign-in can write", code)
	}
}

// Sharing narrows a guest. Applied to an account that writes to the dataset it
// would now narrow the whole account — making a writer read-only everywhere,
// or an administrator unable to administer — so it is refused.
func TestSharingIsRefusedForAnAccountThatWrites(t *testing.T) {
	x := tenants(t)
	named := ""
	for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), "alice").Authorizations {
		if auth.Principal == "alice" {
			named = auth.ID
			break
		}
	}
	if named == "" {
		t.Fatal("fixture should offer alice's own installation")
	}
	err := x.runtime.Authority.GrantRead(t.Context(), named, []string{"security.anything"}, nil)
	if p.ErrorCode(err) != "conflict" {
		t.Fatal("sharing an administrator's own sign-in was not refused:", err)
	}
	if code := x.call(t, "GET", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok, "", nil).Code; code != 200 {
		t.Fatal("a refused share still narrowed the administrator", code)
	}
}
