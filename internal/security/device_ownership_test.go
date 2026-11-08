package security_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	"github.com/ekarpov/tessera/internal/standalone"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// The expected primary is learned independently from the owner's grants,
// never from listing membership or a display name. All members deliberately
// have the same display name and type identifiers.
func TestDeviceJoiningAndRebindingKeepOwnedDatasetWithGroupShares(t *testing.T) {
	for _, shares := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(shares), func(t *testing.T) {
			x := tenants(t)
			client := x.appClient(t)
			types := []p.Meta{}
			for _, id := range []string{"home.track", "home.private"} {
				types = append(types, p.Meta{ID: id, Name: "Same", Fields: []p.Field{{ID: "n", MetaID: id, Type: p.IntegerType}}})
			}
			secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "home", Template: &identity.Template{Administrator: x.alice, DisplayName: "Same", Types: types}, Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, Cap: 20, DeviceRedemption: true})
			if err != nil {
				t.Fatal(err)
			}
			join := func(name string) joined {
				code, status := x.invite(t, "home", secret)
				if status != 201 {
					t.Fatal("invitation refused", status)
				}
				got := x.redeemOnDevice(t, client, code, name)
				if got.Status != 200 {
					t.Fatal("join refused", got.Status)
				}
				expected := ""
				for _, principal := range x.runtime.Authority.AdminOverview(t.Context(), x.alice).Principals {
					if principal.ID == name {
						for id, grants := range principal.Datasets {
							if grants.Erase {
								if expected != "" {
									t.Fatal("ambiguous fixture")
								}
								expected = id
							}
						}
					}
				}
				if expected == "" || got.Dataset != expected {
					t.Fatal("primary differs from independently established own dataset")
				}
				return got
			}
			// A control joins before any shares, then is rebound after they arrive.
			control := join("control")
			peers := []joined{}
			for i := 0; i < shares; i++ {
				peer := join(fmt.Sprintf("peer%d", i))
				peers = append(peers, peer)
				for _, typ := range []string{"home.track", "home.private"} {
					if w := x.call(t, "POST", "/v1/datasets/"+peer.Dataset+"/objects/"+typ, peer.Access, `{"fields":{"n":{"type":"integer","value":3}}}`, nil); w.Code != 201 {
						t.Fatal("peer fixture write refused", w.Code)
					}
				}
				if w := x.admin(t, "PUT", "/datasets/"+peer.Dataset+"/readers/issuer:home", peer.Access, `{"types":["home.track"]}`); w.Code != 204 {
					t.Fatal("share refused", w.Code)
				}
			}
			check := func(member joined, schema string) {
				root := "/v1/datasets/" + member.Dataset
				if w := x.call(t, "POST", root+"/objects/home.track", member.Access, `{"fields":{"n":{"type":"integer","value":7}}}`, nil); w.Code != 201 {
					t.Fatal("own write refused", w.Code)
				}
				if x.reads(t, member.Access, member.Dataset, "home.track") != 200 {
					t.Fatal("own read refused")
				}
				if w := x.call(t, "POST", root+"/meta", member.Access, `{"type":{"id":"`+schema+`","fields":[]}}`, nil); w.Code != 201 {
					t.Fatal("own schema refused", w.Code)
				}
				overview := x.admin(t, "GET", "/overview", member.Access, "")
				var administered struct {
					Datasets []struct {
						ID    string       `json:"id"`
						Usage *store.Usage `json:"usage"`
					} `json:"datasets"`
				}
				if overview.Code != 200 || json.Unmarshal(overview.Body.Bytes(), &administered) != nil || len(administered.Datasets) != 1 || administered.Datasets[0].ID != member.Dataset || administered.Datasets[0].Usage == nil {
					t.Fatal("own-only administration or usage missing", overview.Code)
				}
				if w := x.admin(t, "PUT", "/datasets/"+member.Dataset+"/readers/issuer:home", member.Access, `{"types":["home.track"]}`); w.Code != 204 {
					t.Fatal("own sharing refused", w.Code)
				}
				if w := x.admin(t, "DELETE", "/datasets/"+member.Dataset+"/readers/issuer:home", member.Access, ""); w.Code != 204 {
					t.Fatal("own unsharing refused", w.Code)
				}
				listing := x.listing(t, member.Access)
				if listing[member.Dataset]["erase"] != false {
					t.Fatal("device carries erase or lacks own dataset")
				}
				for _, peer := range peers {
					if x.reads(t, member.Access, peer.Dataset, "home.track") != 200 || x.reads(t, member.Access, peer.Dataset, "home.private") < 400 {
						t.Fatal("borrowed view is incorrect")
					}
					if w := x.call(t, "POST", "/v1/datasets/"+peer.Dataset+"/objects/home.track", member.Access, `{"fields":{"n":{"type":"integer","value":9}}}`, nil); w.Code < 400 {
						t.Fatal("foreign write succeeded")
					}
					readback := x.call(t, "POST", "/v1/datasets/"+peer.Dataset+"/objects/home.track:query", member.Access, `{"query":{}}`, nil)
					var records p.RecordSet
					if readback.Code != 200 || json.Unmarshal(readback.Body.Bytes(), &records) != nil || len(records.Records) != 1 {
						t.Fatal("borrowed records changed after refused write")
					}
					if w := x.call(t, "POST", "/v1/datasets/"+peer.Dataset+"/meta", member.Access, `{"type":{"id":"home.foreign","fields":[]}}`, nil); w.Code < 400 {
						t.Fatal("foreign schema succeeded")
					}
					if w := x.admin(t, "PUT", "/datasets/"+peer.Dataset+"/readers/issuer:home", member.Access, `{"types":["home.private"]}`); w.Code < 400 {
						t.Fatal("foreign sharing succeeded")
					}
				}
				if x.reads(t, x.aliceTok, member.Dataset, "home.track") < 400 || x.reads(t, member.Access, x.bobSet, "home.track") < 400 {
					t.Fatal("unshared dataset exposed")
				}
			}
			check(control, "home.extra")
			// Fresh joining with existing group shares.
			fresh := join("fresh")
			check(fresh, "home.extra")
			config := x.runtime.Config
			if err := x.runtime.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := standalone.Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			x.runtime = reopened
			t.Cleanup(func() { reopened.Close() })
			code, status := x.rebindInvitation(t, "home", secret, "control")
			if status != 201 {
				t.Fatal("rebinding invitation refused", status)
			}
			rebound := x.redeemOnDevice(t, client, code, "")
			if rebound.Status != 200 || rebound.Dataset != control.Dataset {
				t.Fatal("rebind changed own dataset")
			}
			if x.reads(t, rebound.Access, rebound.Dataset, "home.track") != 200 {
				t.Fatal("rebind lost own records")
			}
			w := x.call(t, "POST", "/v1/datasets/"+rebound.Dataset+"/objects/home.track:query", rebound.Access, `{"query":{}}`, nil)
			var answer p.RecordSet
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil || len(answer.Records) != 1 {
				t.Fatal("own record did not survive rebinding")
			}
			check(rebound, "home.rebound")
			if w := x.call(t, "GET", "/v1/datasets", control.Access, "", nil); w.Code != 401 {
				t.Fatal("old access survived")
			}
			if w := x.form(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {control.Refresh}}); w.Code != 400 {
				t.Fatal("old refresh survived")
			}
			if again := x.redeemOnDevice(t, client, code, ""); again.Status != 400 {
				t.Fatal("rebinding invitation reused")
			}
			// Share changes never reselect the persisted primary, including for a
			// second device minted from the replacement installation.
			for _, peer := range peers {
				if w := x.admin(t, "DELETE", "/datasets/"+peer.Dataset+"/readers/issuer:home", peer.Access, ""); w.Code != 204 {
					t.Fatal("unshare refused")
				}
				if x.reads(t, rebound.Access, peer.Dataset, "home.track") < 400 {
					t.Fatal("unshare left access")
				}
			}
			w = x.call(t, "POST", "/oauth/device-codes", rebound.Access, "", nil)
			var device struct {
				Code string `json:"code"`
			}
			if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &device) != nil {
				t.Fatal("device code refused")
			}
			next := x.redeemOnDevice(t, client, device.Code, "")
			if next.Status != 200 || next.Dataset != control.Dataset {
				t.Fatal("share change or device code changed primary")
			}
		})
	}
}

func TestDeviceExistingStoreIssuerKeepsItsExplicitTarget(t *testing.T) {
	x := tenants(t)
	client := x.appClient(t)
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "fixed", Datasets: []string{x.aliceSet}, Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, DeviceRedemption: true})
	if err != nil {
		t.Fatal(err)
	}
	code, status := x.invite(t, "fixed", secret)
	if status != 201 {
		t.Fatal(status)
	}
	first := x.redeemOnDevice(t, client, code, "member")
	if first.Status != 200 || first.Dataset != x.aliceSet {
		t.Fatal("explicit joining target changed")
	}
	code, status = x.rebindInvitation(t, "fixed", secret, "member")
	if status != 201 {
		t.Fatal(status)
	}
	replacement := x.redeemOnDevice(t, client, code, "")
	if replacement.Status != 200 || replacement.Dataset != x.aliceSet {
		t.Fatal("explicit rebinding target changed")
	}
	if listed := x.listing(t, replacement.Access); listed[x.aliceSet]["write"] != true || listed[x.aliceSet]["erase"] != false {
		t.Fatal("issuer rights changed or erase approved")
	}
}
