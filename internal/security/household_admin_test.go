package security_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// A Home owner's phone reads, through the overview, when each member joined,
// since when one is paused, which issuer made them, and when the server last
// backed up — the last only as the server's administrator.
func TestTheOverviewSaysWhenAccountsJoinedAndTheLastBackup(t *testing.T) {
	x := tenants(t)
	x.household(t, "m1")
	if err := x.runtime.Authority.AddServerAdministrator("owner", password, x.aliceSet, store.Grants{Read: true, Write: true, Schema: true, Erase: true}, false); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.SuspendAccount("m1"); err != nil {
		t.Fatal(err)
	}
	type view struct {
		Accounts []struct {
			Account     string  `json:"account"`
			CreatedAt   *string `json:"created_at"`
			SuspendedAt *string `json:"suspended_at"`
			InvitedBy   string  `json:"invited_by"`
		} `json:"accounts"`
		Server *struct {
			LastBackupAt *string `json:"last_backup_at"`
		} `json:"server"`
	}
	read := func(token string) view {
		t.Helper()
		w := x.admin(t, "GET", "/overview", token, "")
		var v view
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatal("the overview was refused:", w.Code, w.Body.String())
		}
		return v
	}
	owner := x.signIn(t, "owner", x.aliceSet, true)
	v := read(owner)
	found := false
	for _, a := range v.Accounts {
		if a.Account != "m1" {
			continue
		}
		found = true
		if a.CreatedAt == nil || a.SuspendedAt == nil || a.InvitedBy != "home" {
			t.Fatal("a member's joining, pause or issuer is missing:", a)
		}
		if _, err := time.Parse(time.RFC3339, *a.SuspendedAt); err != nil {
			t.Fatal("a pause time is not RFC 3339:", *a.SuspendedAt)
		}
	}
	if !found || v.Server == nil || v.Server.LastBackupAt != nil {
		t.Fatal("the overview does not describe the member, or reports a backup never taken:", v)
	}
	if err := x.runtime.Authority.ReinstateAccount("m1"); err != nil {
		t.Fatal(err)
	}
	if err := x.runtime.Authority.RecordBackup(time.Now()); err != nil {
		t.Fatal(err)
	}
	v = read(owner)
	for _, a := range v.Accounts {
		if a.Account == "m1" && a.SuspendedAt != nil {
			t.Fatal("a reinstated member still reads as paused")
		}
	}
	if v.Server == nil || v.Server.LastBackupAt == nil {
		t.Fatal("the last backup is not reported to the server's administrator")
	}
	// Not to an administrator of a dataset alone.
	if v = read(x.signIn(t, x.alice, x.aliceSet, true)); v.Server != nil {
		t.Fatal("the server's backups were told to somebody who does not administer it")
	}
}

// An administrator ends one waiting invitation and leaves the rest; only one
// who may issue from that issuer may, and an unknown code says nothing.
func TestAnAdministratorEndsOneInvitation(t *testing.T) {
	x := tenants(t)
	x.household(t)
	if err := x.runtime.Authority.AddServerAdministrator("operator", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	operator := x.signIn(t, "operator", "", true)
	ended, _ := x.adminInvite(t, "home", operator)
	kept, _ := x.adminInvite(t, "home", operator)
	if w := x.admin(t, "DELETE", "/invitations", x.signIn(t, x.alice, x.aliceSet, true), `{"invitation":"`+ended+`"}`); w.Code != 403 {
		t.Fatal("somebody who may not issue from the issuer ended its invitation:", w.Code)
	}
	if w := x.admin(t, "DELETE", "/invitations", operator, `{"invitation":"`+ended+`"}`); w.Code != 204 {
		t.Fatal("the invitation could not be ended:", w.Code, w.Body.String())
	}
	if got := x.redeem(t, ended, "late"); got != 404 {
		t.Fatal("an ended invitation was redeemed:", got)
	}
	if got := x.redeem(t, kept, "prompt"); got != 201 {
		t.Fatal("ending one invitation ended another:", got)
	}
	if w := x.admin(t, "DELETE", "/invitations", operator, `{"invitation":"never-issued"}`); w.Code != 404 {
		t.Fatal("an unknown code was answered other than not found:", w.Code)
	}
}

// Stopping a Track being shared, for the whole household, deletes nothing:
// every share of the type ends, group and named, and each member keeps their
// records; a dry run says who would be affected first.
func TestAnOwnerStopsATypeBeingSharedWithoutDeletingAnything(t *testing.T) {
	x := tenants(t)
	_, homes := x.household(t, "m1", "m2")
	for _, member := range []string{"m1", "m2"} {
		if err := x.runtime.Authority.SetGroupReader(t.Context(), member, homes[member], "home", []string{"home.track", "home.note"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// And a share naming somebody, of the same type.
	if err := x.runtime.Authority.SetReader(t.Context(), x.alice, homes["m1"], x.bob, []string{"home.track"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	m1, err := x.runtime.Authority.AddAutomation(t.Context(), "m1", homes["m1"])
	if err != nil {
		t.Fatal(err)
	}
	if w := x.call(t, "POST", "/v1/datasets/"+homes["m1"]+"/objects/home.track", m1, `{"fields":{"n":{"type":"integer","value":7}}}`, nil); w.Code != 201 {
		t.Fatal(w.Code)
	}
	owner := x.signIn(t, x.alice, x.aliceSet, true)
	var answer struct {
		Datasets []struct {
			Dataset string   `json:"dataset"`
			Groups  []string `json:"groups"`
			Readers []string `json:"readers"`
		} `json:"datasets"`
	}
	w := x.admin(t, "POST", "/unshare", owner, `{"types":["home.track"],"dry_run":true}`)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil || len(answer.Datasets) != 2 {
		t.Fatal("the dry run does not name both datasets:", w.Code, w.Body.String())
	}
	m2, err := x.runtime.Authority.AddAutomation(t.Context(), "m2", homes["m1"])
	if err != nil {
		t.Fatal(err)
	}
	if x.reads(t, m2, homes["m1"], "home.track") != 200 {
		t.Fatal("a dry run changed who reads")
	}
	if w = x.admin(t, "POST", "/unshare", owner, `{"types":["home.track"]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if code := x.reads(t, m2, homes["m1"], "home.track"); code != 403 {
		t.Fatal("a member still reads a type the household stopped sharing:", code)
	}
	if x.reads(t, m2, homes["m1"], "home.note") != 200 {
		t.Fatal("stopping one type stopped another")
	}
	if _, err = x.runtime.Authority.AddAutomation(t.Context(), x.bob, homes["m1"]); err == nil {
		t.Fatal("a share naming somebody survived the type it named being unshared")
	}
	// Nothing was deleted: the member's own records are theirs.
	w = x.call(t, "POST", "/v1/datasets/"+homes["m1"]+"/objects/home.track:query", m1, `{"query":{}}`, nil)
	var set p.RecordSet
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &set) != nil || len(set.Records) != 1 {
		t.Fatal("stopping sharing deleted the member's own records:", w.Code)
	}
	// Somebody who administers nothing there changes nothing.
	if w = x.admin(t, "POST", "/unshare", x.signIn(t, x.bob, x.bobSet, true), `{"types":["home.track"]}`); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil || len(answer.Datasets) != 0 {
		t.Fatal("somebody who administers none of it changed its shares:", w.Body.String())
	}
}

// On a Home whose issuer moderates sharing, the server's owner can stop one
// member sharing one type — from that member's dataset alone — and the member
// is told who stopped it and when. The owner reads nothing and deletes
// nothing; the member may share again. None of it reaches a dataset another
// issuer made, and nobody but the server's administrator gets it.
func TestTheOwnerOfAModeratedHomeStopsOneMembersShare(t *testing.T) {
	x := tenants(t)
	types := []p.Meta{
		{ID: "home.track", Name: "Track", Fields: []p.Field{{ID: "n", MetaID: "home.track", Type: p.IntegerType}}},
		{ID: "home.note", Name: "Note", Fields: []p.Field{{ID: "text", MetaID: "home.note", Type: p.TextType}}},
	}
	// The custodian holds the members' datasets and nobody signs in as it.
	// A person must be named on some dataset to exist; it holds nothing there.
	if err := x.runtime.Authority.AddPerson("custodian", password, x.bobSet, store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	for _, issuer := range []identity.Issuer{
		{ID: "home", ModerateSharing: true, Template: &identity.Template{Administrator: "custodian", DisplayName: "{account}", Types: types},
			Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, Cap: 10},
		{ID: "elsewhere", Template: &identity.Template{Administrator: "custodian", DisplayName: "{account}", Types: types},
			Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, Cap: 10},
	} {
		if _, err := x.runtime.Authority.AddIssuer(t.Context(), issuer); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.runtime.Authority.AddServerAdministrator("owner", password, "", store.Grants{}, false); err != nil {
		t.Fatal(err)
	}
	owner := x.signIn(t, "owner", "", true)
	datasetOf := map[string]string{}
	for _, join := range []struct{ member, issuer string }{{"ben", "home"}, {"sara", "home"}, {"other", "elsewhere"}} {
		code, status := x.adminInvite(t, join.issuer, owner)
		if status != 201 || x.redeem(t, code, join.member) != 201 {
			t.Fatal("a member could not join", join)
		}
		for _, principal := range x.runtime.Authority.AdminOverview(t.Context(), "custodian").Principals {
			if principal.ID == join.member {
				for dataset := range principal.Datasets {
					datasetOf[join.member] = dataset
				}
			}
		}
	}
	for _, member := range []string{"ben", "sara"} {
		if err := x.runtime.Authority.SetGroupReader(t.Context(), member, datasetOf[member], "home", []string{"home.track", "home.note"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	token := func(member, dataset string) string {
		t.Helper()
		tok, err := x.runtime.Authority.AddAutomation(t.Context(), member, dataset)
		if err != nil {
			t.Fatal(member, err)
		}
		return tok
	}
	saraReadsBen := token("sara", datasetOf["ben"])
	benReadsSara := token("ben", datasetOf["sara"])

	// Somebody who is not the server's administrator cannot, even one who
	// administers a dataset; nor can the owner reach another issuer's.
	if w := x.admin(t, "POST", "/unshare", x.signIn(t, "sara", datasetOf["sara"], true), `{"types":["home.track"],"datasets":["`+datasetOf["ben"]+`"]}`); w.Code != 403 {
		t.Fatal("a member stopped another member's share:", w.Code)
	}
	if w := x.admin(t, "POST", "/unshare", owner, `{"types":["home.track"],"datasets":["`+datasetOf["other"]+`"]}`); w.Code != 403 {
		t.Fatal("the owner stopped a share on a dataset an unmoderated issuer made:", w.Code)
	}

	// The owner stops Ben's Track, and only Ben's.
	if w := x.admin(t, "POST", "/unshare", owner, `{"types":["home.track"],"datasets":["`+datasetOf["ben"]+`"]}`); w.Code != 200 {
		t.Fatal("the owner could not stop a member's share:", w.Code, w.Body.String())
	}
	if code := x.reads(t, saraReadsBen, datasetOf["ben"], "home.track"); code != 403 {
		t.Fatal("a stopped share is still read:", code)
	}
	if x.reads(t, saraReadsBen, datasetOf["ben"], "home.note") != 200 || x.reads(t, benReadsSara, datasetOf["sara"], "home.track") != 200 {
		t.Fatal("stopping one member's Track stopped more than that")
	}
	// The owner reads none of it.
	if _, err := x.runtime.Authority.AddAutomation(t.Context(), "owner", datasetOf["ben"]); err == nil {
		t.Fatal("moderating a dataset let the owner reach it")
	}

	// Ben is told who stopped it and when.
	ben := x.signIn(t, "ben", datasetOf["ben"], true)
	var listed struct {
		Stopped []struct {
			Type      string `json:"type"`
			StoppedBy string `json:"stopped_by"`
			StoppedAt string `json:"stopped_at"`
		} `json:"stopped"`
	}
	w := x.admin(t, "GET", "/datasets/"+datasetOf["ben"]+"/readers", ben, "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Stopped) != 1 ||
		listed.Stopped[0].Type != "home.track" || listed.Stopped[0].StoppedBy != "owner" || listed.Stopped[0].StoppedAt == "" {
		t.Fatal("the member is not told who stopped their share:", w.Code, w.Body.String())
	}

	// Ben shares it again; the note clears, and Sara reads it again.
	if err := x.runtime.Authority.SetGroupReader(t.Context(), "ben", datasetOf["ben"], "home", []string{"home.track", "home.note"}, nil); err != nil {
		t.Fatal(err)
	}
	if x.reads(t, token("sara", datasetOf["ben"]), datasetOf["ben"], "home.track") != 200 {
		t.Fatal("a member could not share again what was stopped")
	}
	w = x.admin(t, "GET", "/datasets/"+datasetOf["ben"]+"/readers", ben, "")
	if json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Stopped) != 0 {
		t.Fatal("sharing again did not clear the record of the stop:", w.Body.String())
	}
}
