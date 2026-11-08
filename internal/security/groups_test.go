package security_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ekarpov/tessera/internal/identity"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// household makes a template issuer and has members redeem it, each into a
// dataset of their own that they administer, and answers each member's
// dataset.
func (x *tenancy) household(t *testing.T, members ...string) (string, map[string]string) {
	t.Helper()
	types := []p.Meta{
		{ID: "home.track", Name: "Track", Fields: []p.Field{{ID: "n", MetaID: "home.track", Type: p.IntegerType}}},
		{ID: "home.note", Name: "Note", Fields: []p.Field{{ID: "text", MetaID: "home.note", Type: p.TextType}}},
	}
	secret, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "home",
		Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: types},
		Grants:   store.Grants{Read: true, Write: true, Schema: true, Erase: true}, Cap: 10})
	if err != nil {
		t.Fatal(err)
	}
	return secret, x.join(t, secret, members...)
}

// join redeems one invitation per member and answers their datasets.
func (x *tenancy) join(t *testing.T, secret string, members ...string) map[string]string {
	t.Helper()
	for _, member := range members {
		code, status := x.invite(t, "home", secret)
		if status != 201 || x.redeem(t, code, member) != 201 {
			t.Fatal("a member could not join", member, status)
		}
	}
	datasets := map[string]string{}
	for _, principal := range x.runtime.Authority.AdminOverview(t.Context(), x.alice).Principals {
		for _, member := range members {
			if principal.ID == member {
				for dataset, held := range principal.Datasets {
					if held.Erase {
						datasets[member] = dataset
					}
				}
			}
		}
	}
	if len(datasets) != len(members) {
		t.Fatal("not every member has a dataset of their own:", datasets)
	}
	return datasets
}

// reads answers what a member's token may query of a type on a dataset.
func (x *tenancy) reads(t *testing.T, token, dataset, typeID string) int {
	t.Helper()
	return x.call(t, "POST", "/v1/datasets/"+dataset+"/objects/"+typeID+":query", token, `{"query":{}}`, nil).Code
}

// A household member shares chosen types with everyone the household's
// issuer made — members who join later included — without naming anybody.
// Only a member of that issuer's household who administers the dataset may,
// a share naming a member wins over the group's, and ending the group ends
// what it gave.
func TestAGroupReadsWhatAMemberSharesWithTheHousehold(t *testing.T) {
	x := tenants(t)
	secret, homes := x.household(t, "m1", "m2")
	d1 := homes["m1"]
	token := func(member, dataset string) string {
		t.Helper()
		tok, err := x.runtime.Authority.AddAutomation(t.Context(), member, dataset)
		if err != nil {
			t.Fatal(member, "holds nothing on", dataset, err)
		}
		return tok
	}

	// Who may name the group: an administrator of the dataset the issuer made.
	if err := x.runtime.Authority.SetGroupReader(t.Context(), "m1", d1, "other", []string{"home.track"}, nil); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("a member named a group other than their own:", err)
	}
	if err := x.runtime.Authority.SetGroupReader(t.Context(), "m2", d1, "home", []string{"home.track"}, nil); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("a member named a group on somebody else's dataset:", err)
	}
	if err := x.runtime.Authority.SetGroupReader(t.Context(), x.alice, d1, "home", []string{"home.track"}, nil); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("an administrator the issuer did not make named its group:", err)
	}
	when := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := x.runtime.Authority.SetGroupReader(t.Context(), "m1", d1, "home", []string{"home.track"}, &when); err != nil {
		t.Fatal(err)
	}

	m2 := token("m2", d1)
	if x.reads(t, m2, d1, "home.track") != 200 {
		t.Fatal("a member of the group cannot read what was shared with it")
	}
	if code := x.reads(t, m2, d1, "home.note"); code != 403 {
		t.Fatal("a group reader reached a type outside the share:", code)
	}
	if w := x.call(t, "POST", "/v1/datasets/"+d1+"/objects/home.track", m2, `{"fields":{"n":{"type":"integer","value":1}}}`, nil); w.Code != 403 {
		t.Fatal("a group reader wrote:", w.Code)
	}
	listed := x.listing(t, m2)
	if listed[d1]["changes_at"] == nil {
		t.Fatal("the group's expiry did not reach its reader")
	}
	// The dataset's own account writes it, and is no reader of it.
	if listed := x.listing(t, token("m1", d1)); listed[d1]["write"] != true {
		t.Fatal("the dataset's own account was made a reader of it:", listed)
	}

	// A member who joins later reads it at once.
	x.join(t, secret, "m3")
	if x.reads(t, token("m3", d1), d1, "home.track") != 200 {
		t.Fatal("a member who joined later does not read what was shared with the group")
	}

	// The administrator sees who reads, and through what.
	owner := x.signIn(t, "m1", d1, true)
	w := x.admin(t, "GET", "/datasets/"+d1+"/readers", owner, "")
	var answer struct {
		Readers []struct {
			Account string `json:"account"`
			Group   string `json:"group"`
		} `json:"readers"`
		Groups []struct {
			Issuer  string   `json:"issuer"`
			Members []string `json:"members"`
		} `json:"groups"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &answer) != nil || len(answer.Groups) != 1 || answer.Groups[0].Issuer != "home" ||
		len(answer.Groups[0].Members) != 2 || len(answer.Readers) != 2 || answer.Readers[0].Group != "issuer:home" {
		t.Fatal("the readers listing does not show the group and who it reaches:", w.Code, w.Body.String())
	}
	// A group reader is removed by changing the group, not one by one.
	if w = x.admin(t, "DELETE", "/datasets/"+d1+"/readers/m2", owner, ""); w.Code != 409 {
		t.Fatal("a group reader was removed on their own, until the group applied again:", w.Code)
	}

	// A share naming a member wins over the group's; ending it falls back.
	if err := x.runtime.Authority.SetReader(t.Context(), "m1", d1, "m2", []string{"home.note"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if x.reads(t, m2, d1, "home.note") != 200 || x.reads(t, m2, d1, "home.track") != 403 {
		t.Fatal("a share naming the member did not take precedence over the group's")
	}
	// Including when the group is set again afterwards.
	if err := x.runtime.Authority.SetGroupReader(t.Context(), "m1", d1, "home", []string{"home.track"}, &when); err != nil {
		t.Fatal(err)
	}
	if x.reads(t, m2, d1, "home.note") != 200 || x.reads(t, m2, d1, "home.track") != 403 {
		t.Fatal("setting the group again overrode a share naming the member")
	}
	if err := x.runtime.Authority.RemoveReader(t.Context(), "m1", d1, "m2"); err != nil {
		t.Fatal(err)
	}
	if x.reads(t, token("m2", d1), d1, "home.track") != 200 {
		t.Fatal("ending a member's own share did not return them to the group's")
	}

	// Ending the group ends what it gave, through the API as well.
	if w = x.admin(t, "DELETE", "/datasets/"+d1+"/readers/issuer:home", owner, ""); w.Code != 204 {
		t.Fatal("the group could not be removed:", w.Code)
	}
	if code := x.reads(t, m2, d1, "home.track"); code != 404 && code != 401 {
		t.Fatal("a group reader still reads after the group ended:", code)
	}
	if _, err := x.runtime.Authority.AddAutomation(t.Context(), "m3", d1); err == nil {
		t.Fatal("a member still holds a dataset whose group grant ended")
	}
	// And the API sets one too.
	if w = x.admin(t, "PUT", "/datasets/"+d1+"/readers/issuer:home", owner, `{"types":["home.note"]}`); w.Code != 204 {
		t.Fatal("the group could not be set through the API:", w.Code, w.Body.String())
	}
	if x.reads(t, token("m3", d1), d1, "home.note") != 200 {
		t.Fatal("a group set through the API does not read")
	}
	// A member reading everything shared with them reads what reaches them
	// through the group, and stops when the group ends.
	shared := x.authorize(t, "m3", password, url.Values{"shared_reads": {"true"}}, url.Values{"shared_reads": {"yes"}, "dataset": {""}})
	if listed := x.listing(t, shared.Access); listed[d1] == nil {
		t.Fatal("reading everything shared does not reach a group share:", listed)
	}
	if w = x.admin(t, "DELETE", "/datasets/"+d1+"/readers/issuer:home", owner, ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if listed := x.listing(t, shared.Access); listed[d1] != nil {
		t.Fatal("reading everything shared kept a group share that ended:", listed)
	}
}

// A member choosing where to sign in can tell their own dataset from those
// shared with them: names, their own first and marked, the same order every
// time; and a share is left out where reading everything shared is offered,
// since that covers it.
func TestTheSignInChooserTellsAPersonsOwnFromShared(t *testing.T) {
	x := tenants(t)
	_, homes := x.household(t, "m1", "m2")
	if err := x.runtime.Authority.SetGroupReader(t.Context(), "m1", homes["m1"], "home", []string{"home.track"}, nil); err != nil {
		t.Fatal(err)
	}
	options := func(consent string) []string {
		var out []string
		rest := consent
		for {
			_, after, found := strings.Cut(rest, "<option ")
			if !found {
				return out
			}
			label, tail, _ := strings.Cut(after, "</option>")
			_, text, _ := strings.Cut(label, ">")
			out = append(out, text)
			rest = tail
		}
	}
	plain := options(x.authorize(t, "m2", password, url.Values{}, url.Values{"dataset": {homes["m2"]}}).Consent)
	if len(plain) != 2 || plain[0] != "m2 — yours" || plain[1] != "m1 — shared with you" {
		t.Fatal("the chooser does not name the datasets, own first:", plain)
	}
	shared := options(x.authorize(t, "m2", password, url.Values{"shared_reads": {"true"}}, url.Values{"dataset": {homes["m2"]}}).Consent)
	if len(shared) != 2 || !strings.HasPrefix(shared[0], "None") || shared[1] != "m2 — yours" {
		t.Fatal("the chooser offers a share beside reading everything shared:", shared)
	}
}

// A person who only adds records to a dataset can sign in to it, through the
// chooser or named, and add.
func TestAPersonWhoOnlyContributesCanSignIn(t *testing.T) {
	x := tenants(t)
	x.defineForm(t)
	if err := x.runtime.Authority.SetContributor(t.Context(), x.alice, x.aliceSet, "dora", []string{response}, nil, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []url.Values{{}, {"dataset": {x.aliceSet}}} {
		got := x.authorize(t, "dora", "correct horse battery staple", extra, url.Values{"dataset": {x.aliceSet}, "insert": {"yes"}})
		if code, _ := x.contribute(t, got.Access, response); code != 201 {
			t.Fatal("a person who only contributes could not add after signing in:", extra, code)
		}
	}
}
