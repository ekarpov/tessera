package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// occupy takes every derivation slot until the returned function is called.
func occupy(a *Authority) func() {
	for i := 0; i < cap(a.derivations); i++ {
		a.derivations <- struct{}{}
	}
	return func() {
		for i := 0; i < cap(a.derivations); i++ {
			<-a.derivations
		}
	}
}

// With every slot taken, a derivation waits its turn and is then refused as
// busy — not answered as a wrong password, and not run beside the others.
func TestADerivationWaitsForASlotAndIsRefusedAsBusy(t *testing.T) {
	a, _, _ := authFixture(t)
	previous := derivationWait
	derivationWait = 50 * time.Millisecond
	t.Cleanup(func() { derivationWait = previous })
	release := occupy(a)
	if _, err := a.derive(t.Context(), "correct horse battery staple", "salt"); p.ErrorCode(err) != "rate_limited" {
		t.Fatal("a derivation ran with every slot taken:", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.derive(ctx, "correct horse battery staple", "salt"); !errors.Is(err, context.Canceled) {
		t.Fatal("a cancelled request still waited for a slot:", err)
	}
	release()
	if derived, err := a.derive(t.Context(), "correct horse battery staple", "salt"); err != nil || derived != password("correct horse battery staple", "salt", a.iterations) {
		t.Fatal("a free slot did not derive:", err)
	}
	if len(a.derivations) != 0 {
		t.Fatal("a derivation kept its slot after finishing")
	}
}

// Every path that derives a password does it without holding the lock that
// every bearer-token check takes. Each is held waiting for a slot here; if it
// held the lock while waiting, Authenticate would wait with it.
func TestNoDerivationHoldsTheLock(t *testing.T) {
	a, _, dataset := authFixture(t)
	if err := a.AddPerson("existing", "correct horse battery staple", dataset, store.Grants{Read: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	token, err := a.AddAutomation(t.Context(), "existing", dataset)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := a.AddIssuer(t.Context(), Issuer{ID: "portal", Datasets: []string{dataset}, Grants: store.Grants{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	code, _, _, err := a.IssueInvitation("portal", secret, "")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]func(){
		"adding a person": func() {
			_ = a.AddPerson("added", "correct horse battery staple", dataset, store.Grants{Read: true}, false)
		},
		"resetting a password": func() { _ = a.ResetPassword("existing", "another horse battery staple", "admin") },
		"redeeming an invitation": func() {
			_, _ = a.RedeemInvitation(context.Background(), code, "invitee", "correct horse battery staple")
		},
		"adding a reader": func() {
			_ = a.SetReader(context.Background(), "existing", dataset, "reader", []string{"t.a"}, nil, "correct horse battery staple")
		},
		"administrator sign-in": func() { a.AuthenticateAdministrator("existing", "correct horse battery staple") },
		"deleting one's own account": func() {
			r := httptest.NewRequest("POST", "/oauth/account/delete", strings.NewReader("account=existing&password=wrong+password+here"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			a.deleteOwnAccount(httptest.NewRecorder(), r)
		},
	}
	for name, path := range paths {
		release := occupy(a)
		done := make(chan struct{})
		go func() { path(); close(done) }()
		time.Sleep(50 * time.Millisecond)
		checked := make(chan error, 1)
		go func() { _, err := a.Authenticate(context.Background(), token); checked <- err }()
		select {
		case err := <-checked:
			if err != nil {
				t.Fatal(name, "broke authentication:", err)
			}
		case <-time.After(2 * time.Second):
			release()
			<-done
			t.Fatal(name, "held the lock while it waited to derive")
		}
		release()
		<-done
	}
}

// With the table of counted addresses full, new addresses share one window,
// and the addresses already counted keep their own: a flood of new ones does
// not take sign-in away from anybody who was already using it.
func TestAFullRateTableKeepsTheCallersItAlreadyCounts(t *testing.T) {
	a, _, _ := authFixture(t)
	previous := maxRateWindows
	maxRateWindows = 2
	t.Cleanup(func() { maxRateWindows = previous })
	ask := func(address string) bool {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = address + ":1"
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.allowed(r, "probe", 3)
	}
	if !ask("192.0.2.1") || !ask("192.0.2.2") {
		t.Fatal("the first addresses were refused")
	}
	// New addresses now share the overflow window, four times the limit.
	admitted := 0
	for i := 0; i < 20; i++ {
		if ask("198.51.100." + strconv.Itoa(10+i)) {
			admitted++
		}
	}
	if admitted != 12 {
		t.Fatal("new addresses past the bound were not counted together:", admitted)
	}
	if !ask("192.0.2.1") {
		t.Fatal("an address already counted lost its own budget to the flood")
	}
}

// A full registry makes room by removing self-registered clients that nobody
// signed in with and that have had an hour to be used. A client somebody
// signed in with, one registered recently, and one the operator provisioned
// all stay.
func TestAFullRegistryMakesRoomFromUnusedRegistrations(t *testing.T) {
	a, _, dataset := authFixture(t)
	stale := time.Now().Add(-2 * unusedClientLifetime).Unix()
	for i := 0; len(a.data.Clients) < maxClients-3; i++ {
		id := "unused-" + strconv.Itoa(i)
		a.data.Clients[id] = Client{ID: id, Redirects: []string{"https://app.example/cb"}, Registered: stale}
	}
	a.data.Clients["signed-in"] = Client{ID: "signed-in", Redirects: []string{"https://app.example/cb"}, Registered: stale}
	a.data.Authorizations["auth"] = Authorization{ID: "auth", Principal: "person", Client: "signed-in", Dataset: dataset, Grants: store.Grants{Read: true}}
	a.data.Clients["recent"] = Client{ID: "recent", Redirects: []string{"https://app.example/cb"}, Registered: time.Now().Unix()}
	if _, err := a.AddService("operator-service", dataset, store.Grants{Read: true}); err != nil {
		t.Fatal(err)
	}
	register := func() int {
		r := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(`{"client_name":"New","redirect_uris":["https://new.example/cb"]}`))
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if got := register(); got != 201 {
		t.Fatal("a full registry of unused registrations refused a new application:", got)
	}
	for _, kept := range []string{"signed-in", "recent", "operator-service"} {
		if _, ok := a.data.Clients[kept]; !ok {
			t.Fatal("making room removed a client in use:", kept)
		}
	}
	if _, ok := a.data.Clients["unused-0"]; ok {
		t.Fatal("an unused, expired registration survived")
	}
}

// An expired invitation can never be redeemed and reserves no name, so it
// does not stay in the registry for ever.
func TestExpiredInvitationsAreDropped(t *testing.T) {
	a, _, dataset := authFixture(t)
	secret, err := a.AddIssuer(t.Context(), Issuer{ID: "open", Datasets: []string{dataset}, Grants: store.Grants{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, _, err = a.IssueInvitation("open", secret, ""); err != nil {
			t.Fatal(err)
		}
	}
	for key, pending := range a.data.Invitations {
		pending.Expires = time.Now().Add(-time.Minute).Unix()
		a.data.Invitations[key] = pending
	}
	if _, _, _, err = a.IssueInvitation("open", secret, ""); err != nil {
		t.Fatal(err)
	}
	if len(a.data.Invitations) != 1 {
		t.Fatal("expired invitations were kept:", len(a.data.Invitations))
	}
}

// A summary of issuers is for showing, and carries no secret, not even its
// digest.
func TestIssuerSummariesCarryNoSecret(t *testing.T) {
	a, _, dataset := authFixture(t)
	if _, err := a.AddIssuer(t.Context(), Issuer{ID: "open", Datasets: []string{dataset}, Grants: store.Grants{Read: true}}); err != nil {
		t.Fatal(err)
	}
	for _, issuer := range a.IssuerSummaries() {
		if issuer.Secret != "" {
			t.Fatal("an issuer summary carried its secret")
		}
	}
}

// One person approving again and again signs out their own oldest
// installations; nobody else is refused because of them.
func TestAPersonsInstallationsAreBoundedByTheirOwnOldest(t *testing.T) {
	a, _, dataset := authFixture(t)
	first, err := a.AddAutomation(t.Context(), "person", dataset)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for i := 0; i < maxPersonAuthorizations; i++ {
		if last, err = a.AddAutomation(t.Context(), "person", dataset); err != nil {
			t.Fatal("an installation was refused instead of making room:", i, err)
		}
	}
	if _, err = a.Authenticate(t.Context(), first); err == nil {
		t.Fatal("the oldest installation was not signed out to make room")
	}
	if _, err = a.Authenticate(t.Context(), last); err != nil {
		t.Fatal("the newest installation does not work:", err)
	}
	live := 0
	for _, auth := range a.data.Authorizations {
		if auth.Principal == "person" && !auth.Revoked {
			live++
		}
	}
	if live != maxPersonAuthorizations {
		t.Fatal("a person holds more live installations than the bound:", live)
	}
}

// A registry full of revoked authorizations makes room by dropping them; a
// long-running server does not end up refusing every sign-in.
func TestRevokedAuthorizationsMakeRoomWhenTheRegistryIsFull(t *testing.T) {
	a, _, dataset := authFixture(t)
	for i := 0; len(a.data.Authorizations) < maxAuthorizations; i++ {
		id := "revoked-" + strconv.Itoa(i)
		a.data.Authorizations[id] = Authorization{ID: id, Principal: "gone", Client: "old", Revoked: true}
	}
	token, err := a.AddAutomation(t.Context(), "person", dataset)
	if err != nil {
		t.Fatal("a full registry of revoked authorizations refused a new one:", err)
	}
	if _, err = a.Authenticate(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if _, kept := a.data.Authorizations["revoked-0"]; kept {
		t.Fatal("revoked authorizations were not dropped to make room")
	}
}

// A service holds one authorization per dataset it reaches and is not signed
// out of the oldest: a reader of many datasets needs all of them.
func TestAServicesAuthorizationsAreNotEvicted(t *testing.T) {
	a, _, dataset := authFixture(t)
	if _, err := a.AddService("reader", dataset, store.Grants{Read: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPersonAuthorizations; i++ {
		id := "held-" + strconv.Itoa(i)
		a.data.Authorizations[id] = Authorization{ID: id, Principal: "reader", Client: "reader", Dataset: "s-other-" + strconv.Itoa(i), Grants: store.Grants{Read: true}}
	}
	a.mu.Lock()
	_, err := a.newAuthorization(t.Context(), a.data.Principals["reader"], "reader", dataset, store.Grants{Read: true}, false, false)
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range a.data.Authorizations {
		if auth.Principal == "reader" && auth.Revoked {
			t.Fatal("a service's authorization was evicted")
		}
	}
}

// A sign-in that proved a password must not finish once that password is
// gone — replaced by the person, by an administrator, or by recovery, which
// is exactly when somebody else may have held the old one.
func TestAPendingSignInEndsWhenThePasswordChanges(t *testing.T) {
	for _, change := range []string{"recovery", "own change", "administrator"} {
		t.Run(change, func(t *testing.T) {
			a, _, dataset := authFixture(t)
			recovery, err := a.IssueRecoveryCode(t.Context(), "person", "synthetic-password-only")
			if err != nil {
				t.Fatal(err)
			}
			a.data.Clients["client"] = Client{ID: "client", Redirects: []string{"https://client.example/callback"}}
			verifier := strings.Repeat("x", 43)
			hash := sha256.Sum256([]byte(verifier))
			query := url.Values{"client_id": {"client"}, "redirect_uri": {"https://client.example/callback"}, "response_type": {"code"},
				"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "state": {"s"}, "dataset": {dataset}}
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorize?"+query.Encode(), nil))
			var id string
			var f flow
			for id, f = range a.flows {
				break
			}
			w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {f.CSRF}, "username": {"person"}, "password": {"synthetic-password-only"}, "decision": {"continue"}}, w.Result().Cookies()[0])
			if w.Code != 200 {
				t.Fatal("sign-in failed", w.Code)
			}
			cookie := w.Result().Cookies()[0]
			signedIn := a.flows[id]
			switch change {
			case "recovery":
				_, err = a.Recover(t.Context(), "person", recovery, "replacement-password-only")
			case "own change":
				err = a.ChangeOwnPassword(t.Context(), "person", "synthetic-password-only", "replacement-password-only")
			default:
				err = a.ResetPassword("person", "replacement-password-only", "admin")
			}
			if err != nil {
				t.Fatal(err)
			}
			w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {signedIn.CSRF}, "decision": {"allow"}, "read": {"yes"}}, cookie)
			if w.Code == 303 {
				t.Fatal("a sign-in made with the old password finished after", change)
			}
		})
	}
}

// A password proved while it was being replaced does not count. Each check
// derives with the lock released; the test holds it there, replaces the
// password as a recovery would, and lets it finish. Every path must refuse:
// the old password is gone, whatever the derivation says about the copy it
// started from.
func TestAPasswordReplacedDuringItsCheckDoesNotCount(t *testing.T) {
	const old = "synthetic-password-only"
	paths := map[string]func(a *Authority, dataset string) bool{
		"sign-in": func(a *Authority, dataset string) bool {
			a.data.Clients["client"] = Client{ID: "client", Redirects: []string{"https://client.example/callback"}}
			hash := sha256.Sum256([]byte(strings.Repeat("x", 43)))
			query := url.Values{"client_id": {"client"}, "redirect_uri": {"https://client.example/callback"}, "response_type": {"code"},
				"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "state": {"s"}, "dataset": {dataset}}
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorize?"+query.Encode(), nil))
			var id string
			var f flow
			for id, f = range a.flows {
				break
			}
			return formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {f.CSRF}, "username": {"person"}, "password": {old}, "decision": {"continue"}}, w.Result().Cookies()[0]).Code == 200
		},
		"administration": func(a *Authority, _ string) bool { return a.AuthenticateAdministrator("person", old) },
		"recovery code": func(a *Authority, _ string) bool {
			_, err := a.IssueRecoveryCode(context.Background(), "person", old)
			return err == nil
		},
		"own change": func(a *Authority, _ string) bool {
			return a.ChangeOwnPassword(context.Background(), "person", old, "a-password-of-their-choosing") == nil
		},
		"deletion": func(a *Authority, _ string) bool {
			r := httptest.NewRequest("POST", "/oauth/account/delete", strings.NewReader(url.Values{"account": {"person"}, "password": {old}, "erase": {""}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			return w.Code == 200 || w.Code == 409
		},
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			a, _, dataset := authFixture(t)
			release := occupy(a)
			done := make(chan bool)
			go func() { done <- path(a, dataset) }()
			time.Sleep(50 * time.Millisecond)
			// Replaced as a recovery replaces it, while the check waits.
			a.mu.Lock()
			person := a.data.Principals["person"]
			person.Salt, person.Password = "replaced", password("a-new-password-entirely", "replaced", a.iterations)
			a.data.Principals["person"] = person
			a.mu.Unlock()
			release()
			if <-done {
				t.Fatal(name, "accepted a password replaced while it was being checked")
			}
		})
	}
}
