package identity

import (
	"encoding/json"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// Ownership errors must refuse before consuming the invitation, revoking any
// access/refresh credentials, or minting a replacement installation. Missing
// ownership with exactly one held dataset deterministically rejects the old
// map-selection path: that path always succeeds on this fixture.
func TestDeviceRebindingRefusesInvalidOwnershipWithoutMutation(t *testing.T) {
	for _, fault := range []string{"missing owner", "foreign owner", "wrong issuer", "ambiguous", "missing grant", "expired grant", "invalid explicit target"} {
		t.Run(fault, func(t *testing.T) {
			a, _, dataset := authFixture(t)
			issuer := Issuer{ID: "home", Template: &Template{Administrator: "person"}, Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, Cap: 10, DeviceRedemption: true}
			secret, err := a.AddIssuer(t.Context(), issuer)
			if err != nil {
				t.Fatal(err)
			}
			member := Principal{ID: "member", Kind: "person", DeviceJoined: true, InvitedBy: "home", Datasets: map[string]store.Grants{dataset: issuer.Grants}}
			a.data.Principals[member.ID] = member
			a.data.Owners = map[string]string{dataset: member.ID}
			a.data.MadeBy = map[string]string{dataset: "home"}
			a.data.Clients["phone"] = Client{ID: "phone"}
			auth, err := a.newAuthorization(t.Context(), member, "phone", dataset, store.Grants{Read: true, Write: true}, false, false)
			if err != nil {
				t.Fatal(err)
			}
			a.issue(auth, true)
			code, _, err := a.IssueRebinding("home", secret, member.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "missing owner":
				delete(a.data.Owners, dataset)
			case "foreign owner":
				a.data.Owners[dataset] = "person"
			case "wrong issuer":
				a.data.MadeBy[dataset] = "other"
			case "ambiguous":
				a.data.Owners["another"] = member.ID
				a.data.MadeBy["another"] = "home"
			case "missing grant":
				delete(member.Datasets, dataset)
			case "expired grant":
				past := time.Now().Add(-time.Hour)
				member.Datasets[dataset] = store.Grants{Read: true, ExpiresAt: &past}
			case "invalid explicit target":
				issuer.Template = nil
				issuer.Datasets = []string{dataset, "another"}
				a.data.Issuers["home"] = issuer
			}
			before, _ := json.Marshal(a.data)
			got, err := a.RedeemOnDevice(t.Context(), code, "phone", "")
			if p.ErrorCode(err) != "not_found" || got.Tokens != nil {
				t.Fatal("invalid ownership produced a successful rebinding")
			}
			after, _ := json.Marshal(a.data)
			if string(before) != string(after) {
				t.Fatal("refused rebinding changed identity state")
			}
		})
	}
}

func TestDeviceDatasetUsesOwnershipAndExplicitIssuerTarget(t *testing.T) {
	a, _, own := authFixture(t)
	full := store.Grants{Read: true, Write: true, Schema: true, Erase: true}
	issuer := Issuer{Template: &Template{}}
	a.data.Owners = map[string]string{own: "member", "borrowed": "peer"}
	a.data.MadeBy = map[string]string{own: "home", "borrowed": "home"}
	for _, order := range [][]string{{own, "borrowed", "writable"}, {"writable", "borrowed", own}} {
		member := Principal{ID: "member", Datasets: map[string]store.Grants{}}
		for _, id := range order {
			member.Datasets[id] = full
		}
		for _, policy := range []Issuer{issuer, {Datasets: []string{own}}} {
			got, err := a.deviceDataset(member, "home", policy)
			if err != nil || got != own {
				t.Fatal("selection did not preserve the issuer target")
			}
		}
	}
}
