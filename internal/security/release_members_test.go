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

// Advice is scoped to the caller's installation and issuer standing, not
// merely to the account being passwordless. Issuance remains authoritative.
func TestOverviewRebindingAdviceMatchesIssuance(t *testing.T) {
	for _, scenario := range []string{"eligible", "password", "suspended", "browser", "other issuer", "dataset administrator"} {
		t.Run(scenario, func(t *testing.T) {
			x := tenants(t)
			client := x.appClient(t)
			secret := x.deviceHome(t, "home", true)
			full := store.Grants{Read: true, Write: true, Schema: true, Erase: true}
			if err := x.runtime.Authority.AddServerAdministrator("operator", password, x.aliceSet, full, false); err != nil {
				t.Fatal(err)
			}
			actor := x.signIn(t, "operator", x.aliceSet, true)
			issuer := "home"
			if scenario == "other issuer" {
				issuer = "other"
				var err error
				secret, err = x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: issuer, Datasets: []string{x.bobSet}, Grants: full, DeviceRedemption: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			code, status := x.invite(t, issuer, secret)
			if status != 201 {
				t.Fatal("fixture invitation refused", status)
			}
			var member joined
			if scenario == "browser" {
				if x.redeem(t, code, "member") != 201 {
					t.Fatal("browser join refused")
				}
			} else {
				member = x.redeemOnDevice(t, client, code, "member")
				if member.Status != 200 {
					t.Fatal("device join refused", member.Status)
				}
			}
			switch scenario {
			case "password":
				link := x.passwordLink(t, member.Access)
				if x.form(t, "/oauth/account/password", url.Values{"page": {"1"}, "link": {link}, "new_password": {password}}).Code != 200 {
					t.Fatal("password setup refused")
				}
			case "suspended":
				if err := x.runtime.Authority.SuspendAccount("member"); err != nil {
					t.Fatal(err)
				}
			case "dataset administrator":
				actor = x.signIn(t, x.alice, x.aliceSet, true)
			}
			w := x.admin(t, "GET", "/overview", actor, "")
			var overview struct {
				Accounts []struct {
					Account    string `json:"account"`
					Rebindable *bool  `json:"rebindable"`
				} `json:"accounts"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &overview) != nil {
				t.Fatal("overview refused", w.Code)
			}
			found := false
			want := scenario == "eligible"
			for _, entry := range overview.Accounts {
				if entry.Account != "member" {
					continue
				}
				found = true
				if entry.Rebindable == nil || *entry.Rebindable != want {
					t.Fatal("rebinding advice differs from caller eligibility")
				}
			}
			if !found {
				t.Fatal("member missing from overview fixture")
			}
			issued := x.admin(t, "POST", "/issuers/"+issuer+"/invitations", actor, `{"rebind":"member"}`)
			if (issued.Code == 201) != want {
				t.Fatal("advice differs from issuance", issued.Code)
			}
		})
	}
}

// A custodian-backed member administers only their own dataset, using only
// device credentials. Leaving explicitly erases it and revokes every device.
func TestDeviceMemberPowersAndConfirmedLeaving(t *testing.T) {
	for _, phase := range []string{"fresh", "rebound"} {
		t.Run(phase, func(t *testing.T) {
			x := tenants(t)
			client := x.appClient(t)
			full := store.Grants{Read: true, Write: true, Schema: true, Erase: true}
			if err := x.runtime.Authority.AddServerAdministrator("custodian", password, "", store.Grants{}, false); err != nil {
				t.Fatal(err)
			}
			owner := x.signIn(t, "custodian", "", true)
			types := []p.Meta{{ID: "house.entry", Fields: []p.Field{{ID: "n", MetaID: "house.entry", Type: p.IntegerType}}}}
			secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "house", Template: &identity.Template{Administrator: "custodian", DisplayName: "{account}", Types: types, EraseWithAccount: true}, Grants: full, Cap: 10, DeviceRedemption: true, ModerateSharing: true})
			if err != nil {
				t.Fatal(err)
			}
			join := func(name string) joined {
				t.Helper()
				code, status := x.adminInvite(t, "house", owner)
				if status != 201 {
					t.Fatal("custodian cannot invite", status)
				}
				member := x.redeemOnDevice(t, client, code, name)
				if member.Status != 200 {
					t.Fatal("device cannot join", member.Status)
				}
				return member
			}
			member, peer := join("member"), join("peer")
			check := func() {
				t.Helper()
				root := "/datasets/" + member.Dataset
				if x.call(t, "POST", "/v1/datasets/"+member.Dataset+"/objects/house.entry", member.Access, `{"fields":{"n":{"type":"integer","value":7}}}`, nil).Code != 201 {
					t.Fatal("own write refused")
				}
				for _, reader := range []string{"peer", "issuer:house"} {
					if w := x.admin(t, "PUT", root+"/readers/"+reader, member.Access, `{"types":["house.entry"]}`); w.Code != 204 {
						t.Fatal("own share refused", w.Code)
					}
					if w := x.admin(t, "DELETE", root+"/readers/"+reader, member.Access, ""); w.Code != 204 {
						t.Fatal("own unshare refused", w.Code)
					}
				}
				if x.admin(t, "PUT", root+"/readers/issuer:house", member.Access, `{"types":["house.entry"]}`).Code != 204 {
					t.Fatal("group share refused")
				}
				if x.reads(t, peer.Access, member.Dataset, "house.entry") != 200 {
					t.Fatal("group share grants no read")
				}
				if x.admin(t, "POST", "/unshare", owner, `{"types":["house.entry"],"datasets":["`+member.Dataset+`"]}`).Code != 200 {
					t.Fatal("custodian cannot stop share")
				}
				var readers struct {
					Stopped []struct {
						Type string `json:"type"`
						By   string `json:"stopped_by"`
						At   string `json:"stopped_at"`
					} `json:"stopped"`
				}
				w := x.admin(t, "GET", root+"/readers", member.Access, "")
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &readers) != nil || len(readers.Stopped) != 1 || readers.Stopped[0].Type != "house.entry" || readers.Stopped[0].By != "custodian" || readers.Stopped[0].At == "" {
					t.Fatal("own stop is not visible", w.Code)
				}
				if got := x.answer(t, "POST", "/v1/datasets/"+member.Dataset+"/objects/house.entry:query", peer.Access, `{"query":{}}`); got != (answer{404, "unavailable"}) {
					t.Fatal("stopping the final share did not conceal the dataset", got)
				}
				w = x.admin(t, "GET", "/overview", member.Access, "")
				var overview struct {
					Datasets []struct {
						ID    string       `json:"id"`
						Usage *store.Usage `json:"usage"`
					} `json:"datasets"`
				}
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &overview) != nil || len(overview.Datasets) != 1 || overview.Datasets[0].ID != member.Dataset || overview.Datasets[0].Usage == nil {
					t.Fatal("own-only usage missing", w.Code)
				}
				for _, request := range []struct{ method, path, body string }{
					{"PUT", "/readers/member", `{"types":["house.entry"]}`},
					{"DELETE", "/readers/member", ""},
					{"PUT", "/readers/issuer:house", `{"types":["house.entry"]}`},
					{"DELETE", "/readers/issuer:house", ""},
					{"GET", "/readers", ""},
				} {
					if w := x.admin(t, request.method, "/datasets/"+peer.Dataset+request.path, member.Access, request.body); w.Code != 403 {
						t.Fatal("foreign administration not refused", request.method, request.path, w.Code)
					}
				}
				if x.reads(t, owner, member.Dataset, "house.entry") < 400 {
					t.Fatal("custodian reads private records")
				}
				if x.listing(t, member.Access)[member.Dataset]["erase"] != false {
					t.Fatal("device carries erase")
				}
			}
			check()
			if phase == "rebound" {
				old := member
				code, status := x.rebindInvitation(t, "house", secret, "member")
				if status != 201 {
					t.Fatal("rebinding refused", status)
				}
				member = x.redeemOnDevice(t, client, code, "")
				if member.Status != 200 || member.Dataset != old.Dataset {
					t.Fatal("rebind changed own dataset")
				}
				if x.call(t, "GET", "/v1/datasets", old.Access, "", nil).Code != 401 {
					t.Fatal("old device survived rebind")
				}
				check()
			}
			w := x.call(t, "POST", "/oauth/device-codes", member.Access, "", nil)
			var next struct {
				Code string `json:"code"`
			}
			if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &next) != nil {
				t.Fatal("second device code refused")
			}
			second := x.redeemOnDevice(t, client, next.Code, "")
			if second.Status != 200 {
				t.Fatal("second device refused")
			}
			deletion := func(erase ...string) *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "/oauth/account/delete", strings.NewReader(url.Values{"erase": erase}.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("Authorization", "Bearer "+member.Access)
				rec := httptest.NewRecorder()
				x.runtime.Handler.ServeHTTP(rec, r)
				return rec
			}
			w = deletion()
			var refusal struct {
				Error    string   `json:"error"`
				Datasets []string `json:"datasets"`
			}
			if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Error != "erasure_required" || len(refusal.Datasets) != 1 || refusal.Datasets[0] != member.Dataset {
				t.Fatal("leaving did not require exact own erasure", w.Code)
			}
			if w = deletion(peer.Dataset); w.Code != 409 {
				t.Fatal("wrong erasure confirmation accepted", w.Code)
			}
			if w = deletion(member.Dataset); w.Code != 200 {
				t.Fatal("confirmed leaving refused", w.Code)
			}
			for _, token := range []string{member.Access, second.Access} {
				if x.call(t, "GET", "/v1/datasets", token, "", nil).Code != 401 {
					t.Fatal("device survived leaving")
				}
			}
			held, err := x.runtime.Engine.Provisioned(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, dataset := range held {
				if dataset.ID == member.Dataset {
					t.Fatal("left member dataset survived")
				}
			}
			w = x.admin(t, "GET", "/overview", owner, "")
			var overview struct {
				Accounts []struct {
					Account string `json:"account"`
				} `json:"accounts"`
				Datasets []struct {
					ID    string       `json:"id"`
					Usage *store.Usage `json:"usage"`
				} `json:"datasets"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &overview) != nil {
				t.Fatal("custodian totals refused")
			}
			total := false
			for _, d := range overview.Datasets {
				if d.ID == peer.Dataset && d.Usage != nil {
					total = true
				}
			}
			if !total {
				t.Fatal("custodian cannot see surviving totals")
			}
			for _, a := range overview.Accounts {
				if a.Account == "member" {
					t.Fatal("left account survived")
				}
			}
			if x.admin(t, "POST", "/accounts/peer/delete", owner, `{"erase":["`+peer.Dataset+`"]}`).Code != 200 {
				t.Fatal("custodian cannot remove member")
			}
			if _, status := x.adminInvite(t, "house", owner); status != 201 {
				t.Fatal("custodian cannot invite after member leaves", status)
			}
		})
	}
}
