package security_test

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// appClient registers the public client an app signs in as.
func (x *tenancy) appClient(t *testing.T) string {
	t.Helper()
	w := x.call(t, "POST", "/oauth/register", "", `{"client_name":"Home","redirect_uris":["https://home.example/callback"]}`, nil)
	var client struct {
		ID string `json:"client_id"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &client) != nil {
		t.Fatal("could not register a client", w.Code)
	}
	return client.ID
}

// deviceHome configures a household issuer whose invitations an app may
// redeem on a device, administered by alice.
func (x *tenancy) deviceHome(t *testing.T, id string, device bool) string {
	t.Helper()
	types := []p.Meta{{ID: "home.track", Name: "Track", Fields: []p.Field{{ID: "n", MetaID: "home.track", Type: p.IntegerType}}}}
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: id,
		Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: types},
		Grants:   store.Grants{Read: true, Write: true, Schema: true, Erase: true}, Cap: 10, DeviceRedemption: device})
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

type joined struct {
	Status  int
	Error   string `json:"error"`
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Account string `json:"account"`
	Dataset string `json:"dataset_id"`
}

func (x *tenancy) redeemOnDevice(t *testing.T, client, code, account string) joined {
	t.Helper()
	w := x.form(t, "/oauth/token", url.Values{"grant_type": {identity.InvitationGrant}, "client_id": {client}, "code": {code}, "account": {account}})
	var answer joined
	_ = json.Unmarshal(w.Body.Bytes(), &answer)
	answer.Status = w.Code
	return answer
}

func (x *tenancy) rebindInvitation(t *testing.T, issuer, secret, account string) (string, int) {
	t.Helper()
	r := httptest.NewRequest("POST", "/oauth/invitations", strings.NewReader(url.Values{"rebind": {account}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(issuer, secret)
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, r)
	var answer struct {
		Invitation string `json:"invitation"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &answer)
	return answer.Invitation, w.Code
}

// An app joins a household on a device: no password, no browser. The preview
// says what joining gives, the code is spent once, the installation reaches
// the member's own dataset and what is shared with them, and an issuer that
// did not opt in refuses it.
func TestAnAppJoinsAHomeWithoutABrowser(t *testing.T) {
	x := tenants(t)
	x.runtime.Authority.SetDisplayName("Example Home")
	client := x.appClient(t)
	secret := x.deviceHome(t, "home", true)
	code, status := x.invite(t, "home", secret)
	if status != 201 {
		t.Fatal(status)
	}
	preview := x.call(t, "GET", "/oauth/invitation/preview?code="+url.QueryEscape(code), "", "", nil)
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), `"display_name":"Example Home"`) || strings.Contains(preview.Body.String(), `"erase":true`) {
		t.Fatal("the preview does not say what joining gives", preview.Code, preview.Body.String())
	}
	got := x.redeemOnDevice(t, client, code, "sara")
	if got.Status != 200 || got.Account != "sara" || got.Dataset == "" || got.Access == "" || got.Refresh == "" {
		t.Fatal("the app could not join", got)
	}
	if !x.listsDataset(t, x.call(t, "GET", "/v1/datasets", got.Access, "", nil).Body.Bytes(), got.Dataset) {
		t.Fatal("the installation does not reach the member's dataset")
	}
	who := x.whoami(t, got.Access)
	if who["has_password"] != false || who["installation"].(map[string]any)["shared_reads"] != true {
		t.Fatal("the account is not described as joined on a device", who)
	}
	// The member administers the dataset made for them, as a browser sign-in
	// approving administration would: they share it with their household.
	if who["installation"].(map[string]any)["administer"] != true || !strings.Contains(preview.Body.String(), `"administer":true`) {
		t.Fatal("a member who joined on a device cannot administer their own dataset", who, preview.Body.String())
	}
	if w := x.admin(t, "GET", "/datasets/"+got.Dataset+"/readers", got.Access, ""); w.Code != 200 {
		t.Fatal("a member who joined on a device cannot see who reads their dataset", w.Code, w.Body.String())
	}
	if w := x.admin(t, "GET", "/datasets/"+x.aliceSet+"/readers", got.Access, ""); w.Code != 403 {
		t.Fatal("a member administered a dataset that is not theirs", w.Code)
	}
	if again := x.redeemOnDevice(t, client, code, "sara2"); again.Status != 400 || again.Error != "invalid_grant" {
		t.Fatal("an invitation was redeemed twice", again)
	}
	// No password means no password sign-in, whatever is typed.
	if w := x.deleteOwn(t, "sara", ""); w.Code != 401 {
		t.Fatal("a passwordless account was reached with an empty password", w.Code)
	}
	// An issuer that did not opt in keeps the browser flow.
	plain := x.deviceHome(t, "studyish", false)
	other, _ := x.invite(t, "studyish", plain)
	if w := x.call(t, "GET", "/oauth/invitation/preview?code="+url.QueryEscape(other), "", "", nil); w.Code != 404 {
		t.Fatal("an issuer that did not opt in was previewed for a device", w.Code)
	}
	if refused := x.redeemOnDevice(t, client, other, "tom"); refused.Status != 400 {
		t.Fatal("an issuer that did not opt in was redeemed on a device", refused)
	}
	// Only a registered public client redeems.
	fresh, _ := x.invite(t, "home", secret)
	if refused := x.redeemOnDevice(t, "no-such-client", fresh, "ann"); refused.Status != 401 {
		t.Fatal("an unknown client redeemed an invitation", refused)
	}
}

// A second device joins from the first, by a single-use code carrying the
// first installation's approval and nothing more.
func TestAnotherDeviceJoinsFromTheFirst(t *testing.T) {
	x := tenants(t)
	client := x.appClient(t)
	secret := x.deviceHome(t, "home", true)
	code, _ := x.invite(t, "home", secret)
	first := x.redeemOnDevice(t, client, code, "sara")
	w := x.call(t, "POST", "/oauth/device-codes", first.Access, "", nil)
	var issued struct {
		Code string `json:"code"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &issued) != nil || issued.Code == "" {
		t.Fatal("no device code", w.Code, w.Body.String())
	}
	second := x.redeemOnDevice(t, client, issued.Code, "")
	if second.Status != 200 || second.Account != "sara" || second.Dataset != first.Dataset {
		t.Fatal("the second device did not join the same account", second)
	}
	if again := x.redeemOnDevice(t, client, issued.Code, ""); again.Status != 400 {
		t.Fatal("a device code was spent twice", again)
	}
	if w := x.call(t, "POST", "/oauth/device-codes", "", "", nil); w.Code != 401 {
		t.Fatal("a device code was issued without a token", w.Code)
	}
}

// After a lost phone, the issuer reconnects an account it made on a device to
// a new one, and every old installation stops. It cannot reach an account
// with a password, one another issuer made, or one made in a browser.
func TestTheIssuerRebindsALostPhone(t *testing.T) {
	x := tenants(t)
	client := x.appClient(t)
	secret := x.deviceHome(t, "home", true)
	code, _ := x.invite(t, "home", secret)
	lost := x.redeemOnDevice(t, client, code, "sara")
	rebinding, status := x.rebindInvitation(t, "home", secret, "sara")
	if status != 201 {
		t.Fatal("no rebinding", status)
	}
	found := x.redeemOnDevice(t, client, rebinding, "")
	if found.Status != 200 || found.Account != "sara" || found.Dataset != lost.Dataset {
		t.Fatal("the rebinding did not reconnect the account", found)
	}
	if w := x.call(t, "GET", "/v1/datasets", lost.Access, "", nil); w.Code != 401 {
		t.Fatal("the lost phone's token still works", w.Code)
	}
	if w := x.form(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {lost.Refresh}, "client_id": {client}}); w.Code != 400 {
		t.Fatal("the lost phone could still renew", w.Code)
	}
	if who := x.whoami(t, found.Access); who["rebound"] == nil {
		t.Fatal("the account is not told it was rebound", who)
	}
	// Not an account made in a browser, nor one another issuer made.
	if _, status = x.rebindInvitation(t, "home", secret, x.alice); status != 403 {
		t.Fatal("an issuer rebound an account it did not make on a device", status)
	}
	other := x.deviceHome(t, "other", true)
	if _, status = x.rebindInvitation(t, "other", other, "sara"); status != 403 {
		t.Fatal("an issuer rebound another issuer's member", status)
	}
	// Not once the person has a password.
	link := x.passwordLink(t, found.Access)
	if w := x.form(t, "/oauth/account/password", url.Values{"page": {"1"}, "link": {link}, "new_password": {password}}); w.Code != 200 {
		t.Fatal("the password link did not set a password", w.Code)
	}
	if _, status = x.rebindInvitation(t, "home", secret, "sara"); status != 403 {
		t.Fatal("an issuer rebound an account with a password", status)
	}
}

func (x *tenancy) passwordLink(t *testing.T, token string) string {
	t.Helper()
	w := x.call(t, "POST", "/oauth/account/password-link", token, "", nil)
	var answer struct {
		URL string `json:"url"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &answer) != nil {
		t.Fatal("no password link", w.Code, w.Body.String())
	}
	parsed, err := url.Parse(answer.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query().Get("link")
}

// A passwordless account chooses its first password on this server's page,
// once; an account with a password cannot have it replaced this way.
func TestAPasswordLinkSetsTheFirstPasswordOnly(t *testing.T) {
	x := tenants(t)
	client := x.appClient(t)
	secret := x.deviceHome(t, "home", true)
	code, _ := x.invite(t, "home", secret)
	member := x.redeemOnDevice(t, client, code, "sara")
	link := x.passwordLink(t, member.Access)
	page := x.call(t, "GET", "/oauth/account/password?link="+url.QueryEscape(link), "", "", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `data-account="sara"`) || !strings.Contains(page.Body.String(), `name="new_password"`) {
		t.Fatal("the link does not open the first-password page", page.Code)
	}
	set := x.form(t, "/oauth/account/password", url.Values{"page": {"1"}, "link": {link}, "new_password": {password}})
	if set.Code != 200 || !strings.Contains(set.Body.String(), "data-recovery-code") {
		t.Fatal("the first password was not set with its recovery code", set.Code)
	}
	if again := x.form(t, "/oauth/account/password", url.Values{"page": {"1"}, "link": {link}, "new_password": {"another-password-1"}}); again.Code != 404 {
		t.Fatal("a password link was spent twice", again.Code)
	}
	if w := x.call(t, "GET", "/v1/datasets", member.Access, "", nil); w.Code != 200 {
		t.Fatal("setting a password signed the device out", w.Code)
	}
	if w := x.call(t, "POST", "/oauth/account/password-link", member.Access, "", nil); w.Code != 403 {
		t.Fatal("an account with a password was given a link to replace it", w.Code)
	}
	if w := x.call(t, "POST", "/oauth/account/password-link", x.aliceTok, "", nil); w.Code != 403 {
		t.Fatal("an installation replaced its person's password", w.Code)
	}
}

// A passwordless account deletes itself with its device's installation; one
// with a password still needs the password.
func TestAPasswordlessAccountDeletesItselfWithItsDevice(t *testing.T) {
	x := tenants(t)
	client := x.appClient(t)
	secret := x.deviceHome(t, "home", true)
	code, _ := x.invite(t, "home", secret)
	member := x.redeemOnDevice(t, client, code, "sara")
	deletion := func(token string, erase ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/oauth/account/delete", strings.NewReader(url.Values{"erase": erase}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(w, r)
		return w
	}
	if w := deletion(x.aliceTok); w.Code != 403 {
		t.Fatal("an installation deleted an account that has a password", w.Code)
	}
	// Alice administers the dataset the template made, so nothing is the
	// member's alone to erase.
	if w := deletion(member.Access); w.Code != 200 {
		t.Fatal("a passwordless account could not delete itself", w.Code, w.Body.String())
	}
	if w := x.call(t, "GET", "/v1/datasets", member.Access, "", nil); w.Code != 401 {
		t.Fatal("a deleted account's device still works", w.Code)
	}
}

// The server's owner signs in from the app with a code made on the host,
// which can also sign every other device of the account out in the same step.
func TestTheOwnersFirstDeviceComesFromTheHost(t *testing.T) {
	x := tenants(t)
	client := x.appClient(t)
	// Alice administers a dataset and holds no server right, so her code
	// names the dataset; one naming none would carry nothing.
	if _, _, err := x.runtime.Authority.AddHostDeviceCode(t.Context(), x.alice, "", false); p.ErrorCode(err) != "invalid_field" {
		t.Fatal("a host code carrying nothing was made", err)
	}
	code, _, err := x.runtime.Authority.AddHostDeviceCode(t.Context(), x.alice, x.aliceSet, true)
	if err != nil {
		t.Fatal(err)
	}
	if w := x.call(t, "GET", "/v1/datasets", x.aliceTok, "", nil); w.Code != 401 {
		t.Fatal("revoking the account's installations left one working", w.Code)
	}
	owner := x.redeemOnDevice(t, client, code, "")
	if owner.Status != 200 || owner.Account != x.alice {
		t.Fatal("the owner's device did not join", owner)
	}
	if who := x.whoami(t, owner.Access); who["installation"].(map[string]any)["administer"] != true {
		t.Fatal("the owner's device does not administer", who)
	}
	if again := x.redeemOnDevice(t, client, code, ""); again.Status != 400 {
		t.Fatal("a host code was spent twice", again)
	}
	// Bob may create datasets and holds none: his code carries that right.
	creator, _, err := x.runtime.Authority.AddHostDeviceCode(t.Context(), x.bob, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if bob := x.redeemOnDevice(t, client, creator, ""); bob.Status != 200 || x.whoami(t, bob.Access)["installation"].(map[string]any)["create_dataset"] != true {
		t.Fatal("a creator's device does not carry creation", bob)
	}
	if _, _, err = x.runtime.Authority.AddHostDeviceCode(t.Context(), "nobody", "", false); p.ErrorCode(err) != "not_found" {
		t.Fatal("a host code was made for nobody", err)
	}
}

// A member lists and removes their own devices, and an administrator of the
// account lists and removes any of them. Removal ends access at the next
// request, nothing renews it, and no record of it stays.
func TestDevicesAreRemovedByTheirPersonOrAnAdministrator(t *testing.T) {
	x := tenants(t)
	client := x.appClient(t)
	secret := x.deviceHome(t, "home", true)
	code, _ := x.invite(t, "home", secret)
	phone := x.redeemOnDevice(t, client, code, "sara")
	w := x.call(t, "POST", "/oauth/device-codes", phone.Access, "", nil)
	var issued struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &issued)
	tablet := x.redeemOnDevice(t, client, issued.Code, "")
	third := x.call(t, "POST", "/oauth/device-codes", phone.Access, "", nil)
	_ = json.Unmarshal(third.Body.Bytes(), &issued)
	laptop := x.redeemOnDevice(t, client, issued.Code, "")
	list := func(token string) []map[string]any {
		var answer struct {
			Installations []map[string]any `json:"installations"`
		}
		w := x.call(t, "GET", "/oauth/account/installations", token, "", nil)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil {
			t.Fatal("no installation list", w.Code)
		}
		return answer.Installations
	}
	mine := list(phone.Access)
	if len(mine) != 3 {
		t.Fatal("the member does not see their three devices", mine)
	}
	idOf := func(token string) string {
		for _, i := range list(token) {
			if i["current"] == true {
				return i["id"].(string)
			}
		}
		t.Fatal("no current installation")
		return ""
	}
	tabletID, laptopID := idOf(tablet.Access), idOf(laptop.Access)
	// Another person's installation is not theirs to remove.
	other, _ := x.invite(t, "home", secret)
	stranger := x.redeemOnDevice(t, client, other, "tom")
	if w := x.call(t, "DELETE", "/oauth/account/installations/"+tabletID, stranger.Access, "", nil); w.Code != 404 {
		t.Fatal("a member removed somebody else's device", w.Code)
	}
	if w := x.call(t, "DELETE", "/oauth/account/installations/"+tabletID, phone.Access, "", nil); w.Code != 204 {
		t.Fatal("a member could not remove their own device", w.Code)
	}
	if w := x.call(t, "GET", "/v1/datasets", tablet.Access, "", nil); w.Code != 401 {
		t.Fatal("a removed device still reads", w.Code)
	}
	if w := x.form(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tablet.Refresh}, "client_id": {client}}); w.Code != 400 {
		t.Fatal("a removed device renewed", w.Code)
	}
	if len(list(phone.Access)) != 2 {
		t.Fatal("a removed device is still listed")
	}
	// The administrator of the member's dataset removes the laptop.
	admin := x.signIn(t, x.alice, x.aliceSet, true)
	listing := x.admin(t, "GET", "/accounts/sara/installations", admin, "")
	if listing.Code != 200 || !strings.Contains(listing.Body.String(), laptopID) || strings.Contains(listing.Body.String(), tabletID) {
		t.Fatal("the administrator does not see the account's live devices", listing.Code, listing.Body.String())
	}
	if w := x.admin(t, "DELETE", "/accounts/sara/installations/"+laptopID, admin, ""); w.Code != 204 {
		t.Fatal("the administrator could not remove a device", w.Code, w.Body.String())
	}
	if w := x.call(t, "GET", "/v1/datasets", laptop.Access, "", nil); w.Code != 401 {
		t.Fatal("a device the administrator removed still reads", w.Code)
	}
	// Somebody who does not administer the account cannot.
	bob := x.signIn(t, x.bob, x.bobSet, true)
	if w := x.admin(t, "GET", "/accounts/sara/installations", bob, ""); w.Code != 403 {
		t.Fatal("a stranger listed an account's devices", w.Code)
	}
}
