package security_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
	"github.com/ekarpov/tessera/internal/standalone"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// This is a template participant signing in with write alone, and a template
// service reading that participant. The study administrator holds full grants,
// but cannot restore a participant's withdrawn share even with write approval.
func TestFrozenTemplateReaderWithdrawalAndReconsent(t *testing.T) {
	x := tenants(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	secret, err := x.runtime.Authority.AddService("analysis", "", store.Grants{})
	must(err)
	const typ = "study.measurement"
	scale := 2
	types := []p.Meta{{ID: typ, Fields: []p.Field{{ID: "n", MetaID: typ, Type: p.IntegerType}, {ID: "x", MetaID: typ, Type: p.FixedType, Scale: &scale}, {ID: "ref", MetaID: typ, Type: p.ReferenceType}, {ID: "photo", MetaID: typ, Type: p.MediaType}}}}
	issuer, err := x.runtime.Authority.AddIssuer(t.Context(), identity.Issuer{ID: "study", Template: &identity.Template{Administrator: x.alice, DisplayName: "{account}", Types: types, Readers: []identity.TemplateReader{{Account: "analysis"}}}, Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, Cap: 5})
	must(err)
	code, _, _ := x.inviteNaming(t, "study", issuer, "")
	if x.redeem(t, code, "participant") != 201 {
		t.Fatal("redemption failed")
	}
	dataset := ""
	datasets, err := x.runtime.Engine.Provisioned(t.Context())
	must(err)
	for _, d := range datasets {
		if d.DisplayName == "participant" {
			dataset = d.ID
		}
	}
	if dataset == "" {
		t.Fatal("missing participant dataset")
	}
	writer := x.signInGrants(t, "participant", dataset, url.Values{"write": {"yes"}})
	reading, status := x.serviceToken(t, "analysis", secret, dataset)
	if status != 200 {
		t.Fatal(status)
	}
	root := "/v1/datasets/" + dataset
	content := x.contentStore(t, writer, dataset)
	preItem := x.upload(t, writer, content, strings.Repeat("a", 32))
	postItem := x.upload(t, writer, content, strings.Repeat("b", 32))
	insert := func(n int, item string) string {
		t.Helper()
		body := fmt.Sprintf(`{"fields":{"n":{"type":"integer","value":%d},"x":{"type":"fixed","value":"%d.25"},"ref":{"type":"reference","value":{"dataset_id":"%s","meta_id":"%s","object_id":"target-%d"}},"photo":{"type":"media","value":"%s//%s"}}}`, n, n, dataset, typ, n, content, item)
		w := x.call(t, "POST", root+"/objects/"+typ, writer, body, nil)
		var result struct {
			Objects []struct {
				ID string `json:"id"`
			} `json:"objects"`
		}
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Objects) != 1 {
			t.Fatal("insert failed", w.Code, w.Body.String())
		}
		return result.Objects[0].ID
	}
	pre := []string{}
	for i := 1; i <= 11; i++ {
		pre = append(pre, insert(i, preItem))
	}
	version := func() string {
		w := x.call(t, "GET", "/v1/datasets", reading, "", nil)
		var listing struct {
			Datasets []struct {
				ID   string `json:"id"`
				View string `json:"view_version"`
			} `json:"datasets"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listing) != nil {
			t.Fatal("missing view version", w.Code, w.Body.String())
		}
		for _, d := range listing.Datasets {
			if d.ID == dataset && d.View != "" {
				return d.View
			}
		}
		t.Fatal("missing dataset view version", w.Body.String())
		return ""
	}
	begin := func(query string) string {
		w := x.call(t, "POST", root+"/objects/"+typ+":page", reading, `{"query":`+query+`,"page_size":2}`, nil)
		var page store.PageStart
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Cursor == "" {
			t.Fatal("page start failed", w.Code, w.Body.String())
		}
		return page.Cursor
	}
	oldView := version()
	oldCursor := begin(`{}`)
	oldSnapshot := begin(`{"result_sort":[{"field_id":"x","ascending":true}]}`)
	freeze := func(token, route, action string, want int) {
		t.Helper()
		w := x.call(t, "POST", route+"/readers/analysis:"+action, token, "", nil)
		if w.Code != want {
			t.Fatalf("%s: got %d want %d: %s", action, w.Code, want, w.Body.String())
		}
	}
	// Neither a read approval nor another dataset's write approval has standing.
	readonly := x.signIn(t, "participant", dataset, false)
	freeze(readonly, root, "freeze", 403)
	freeze(x.bobTok, root, "freeze", 403)
	freeze(writer, "/admin/api/v1/datasets/"+dataset, "freeze", 403)
	freeze(writer, root, "freeze", 204)
	frozenView := version()
	if oldView == frozenView {
		t.Fatal("freeze did not change view_version")
	}
	for _, cursor := range []string{oldCursor, oldSnapshot} {
		w := x.call(t, "POST", root+"/pages/"+cursor+":next", reading, "", nil)
		if w.Code != 410 || !strings.Contains(w.Body.String(), "view_changed") {
			t.Fatal("old cursor survived freeze", w.Code, w.Body.String())
		}
	}
	post := []string{}
	for i := 100; i < 109; i++ {
		post = append(post, insert(i, postItem))
	}
	for _, id := range pre {
		if w := x.call(t, "GET", root+"/objects/"+typ+"/"+id, reading, "", nil); w.Code != 200 {
			t.Fatal("pre-freeze record hidden", id, w.Code, w.Body.String())
		}
	}
	for _, id := range append(post, "never-minted") {
		if w := x.call(t, "GET", root+"/objects/"+typ+"/"+id, reading, "", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "unavailable") {
			t.Fatal("post-freeze record disclosed", id, w.Code, w.Body.String())
		}
	}
	for _, route := range []struct {
		suffix, body string
		count        int
	}{
		{":query", `{"query":{}}`, 11},
		{":query", `{"query":{"result_sort":[{"field_id":"n","ascending":false}]}}`, 11},
		{":count", `{}`, 11},
		{":aggregate", `{"field":"n"}`, 11},
		{":distinct-references", `{"field":"ref","maximum_count":20}`, 11},
	} {
		w := x.call(t, "POST", root+"/objects/"+typ+route.suffix, reading, route.body, nil)
		if w.Code != 200 {
			t.Fatal(route.suffix, w.Code, w.Body.String())
		}
		var answer struct {
			Records    []p.Record `json:"records"`
			Count      int        `json:"count"`
			Aggregates []struct {
				Rows int `json:"row_count"`
				Sum  int `json:"sum"`
			} `json:"aggregates"`
			References []p.Reference `json:"references"`
		}
		must(json.Unmarshal(w.Body.Bytes(), &answer))
		switch route.suffix {
		case ":query":
			if len(answer.Records) != 11 {
				t.Fatal("query included hidden records", w.Body.String())
			}
		case ":count":
			if answer.Count != 11 {
				t.Fatal("count included hidden records", w.Body.String())
			}
		case ":aggregate":
			if len(answer.Aggregates) != 1 || answer.Aggregates[0].Rows != 11 || answer.Aggregates[0].Sum != 66 {
				t.Fatal("aggregate included hidden records", w.Body.String())
			}
		case ":distinct-references":
			if len(answer.References) != 11 {
				t.Fatal("distinct references included hidden records", w.Body.String())
			}
		}
	}
	for _, query := range []string{`{}`, `{"result_sort":[{"field_id":"n","ascending":false}]}`, `{"result_sort":[{"field_id":"x","ascending":true}]}`} {
		cursor := begin(query)
		seen := map[string]bool{}
		for cursor != "" {
			w := x.call(t, "POST", root+"/pages/"+cursor+":next", reading, "", nil)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var page struct {
				Records []p.Record `json:"records"`
				Next    *string    `json:"next_cursor"`
			}
			must(json.Unmarshal(w.Body.Bytes(), &page))
			for _, r := range page.Records {
				if seen[r.ID.ID] {
					t.Fatal("duplicate record")
				}
				seen[r.ID.ID] = true
			}
			cursor = ""
			if page.Next != nil {
				cursor = *page.Next
			}
		}
		if len(seen) != 11 {
			t.Fatal("pages included hidden records", query, seen)
		}
		for _, id := range pre {
			if !seen[id] {
				t.Fatal("page lost a pre-freeze record", id)
			}
		}
	}
	if !x.typesSeen(t, dataset, reading)[typ] {
		t.Fatal("freeze hid schema")
	}
	for _, item := range []string{preItem, postItem} {
		want := 200
		if item == postItem {
			want = 404
		}
		w := x.call(t, "POST", "/v1/stores/"+content+"/items/"+item+":resolve", reading, `{"representation":"original"}`, nil)
		if w.Code != want || (want == 404 && !strings.Contains(w.Body.String(), "unavailable")) {
			t.Fatal("content visibility", w.Code, w.Body.String())
		}
	}
	for _, route := range []string{root, "/admin/api/v1/datasets/" + dataset} {
		token := writer
		if strings.HasPrefix(route, "/admin/") {
			token = x.signIn(t, x.alice, dataset, true)
		}
		w := x.call(t, "GET", route+"/readers", token, "", nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"frozen_by":"participant"`) || !strings.Contains(w.Body.String(), `"frozen_at":`) {
			t.Fatal("missing freeze attribution", w.Code, w.Body.String())
		}
	}
	// The registry and engine must preserve the cut across a restart.
	must(x.runtime.Close())
	reopened, err := standalone.Open(t.Context(), x.runtime.Config)
	must(err)
	x.runtime = reopened
	t.Cleanup(func() { reopened.Close() })
	if w := x.call(t, "GET", root+"/objects/"+typ+"/"+post[0], reading, "", nil); w.Code != 404 {
		t.Fatal("restart lost the freeze", w.Code)
	}
	researcher := x.signInGrants(t, x.alice, dataset, url.Values{"read": {"yes"}, "write": {"yes"}})
	freeze(researcher, root, "unfreeze", 403)
	freeze(researcher, "/admin/api/v1/datasets/"+dataset, "unfreeze", 403)
	must(x.runtime.Authority.GrantDataset(x.bob, dataset, store.Grants{Read: true, Write: true}))
	otherWriter := x.signInGrants(t, x.bob, dataset, url.Values{"write": {"yes"}})
	freeze(otherWriter, root, "unfreeze", 403)
	freeze(otherWriter, "/admin/api/v1/datasets/"+dataset, "unfreeze", 403)
	administrator := x.signInGrants(t, x.alice, dataset, url.Values{"read": {"yes"}, "write": {"yes"}, "administer": {"yes"}})
	freeze(administrator, "/admin/api/v1/datasets/"+dataset, "unfreeze", 403)
	freeze(administrator, root, "unfreeze", 403)
	// A second freeze must preserve the original cut, and future tokens inherit it.
	freeze(administrator, "/admin/api/v1/datasets/"+dataset, "freeze", 204)
	again, status := x.serviceToken(t, "analysis", secret, dataset)
	if status != 200 {
		t.Fatal(status)
	}
	if w := x.call(t, "GET", root+"/objects/"+typ+"/"+post[0], again, "", nil); w.Code != 404 {
		t.Fatal("new token lost the freeze")
	}
	if w := x.call(t, "PATCH", root+"/objects/"+typ+"/"+pre[0], writer, `{"fields":{"n":{"type":"integer","value":42}}}`, nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := x.call(t, "GET", root+"/objects/"+typ+"/"+pre[0], reading, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"value":42`) {
		t.Fatal("edit hidden", w.Code, w.Body.String())
	}
	if w := x.call(t, "DELETE", root+"/objects/"+typ+"/"+pre[1], writer, "", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	deletedPost := post[len(post)-1]
	if w := x.call(t, "DELETE", root+"/objects/"+typ+"/"+deletedPost, writer, "", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	post = post[:len(post)-1]
	for _, id := range []string{pre[1], post[0], deletedPost, "unminted"} {
		want := "unavailable"
		if id == pre[1] {
			want = "not_found"
		}
		w := x.call(t, "GET", root+"/objects/"+typ+"/"+id, reading, "", nil)
		if w.Code != 404 || !strings.Contains(w.Body.String(), `"code":"`+want+`"`) {
			t.Fatal("frozen missing record", id, want, w.Code, w.Body.String())
		}
	}
	frozenCursor := begin(`{}`)
	freeze(writer, root, "unfreeze", 204)
	if version() == frozenView {
		t.Fatal("unfreeze did not change view_version")
	}
	if w := x.call(t, "POST", root+"/pages/"+frozenCursor+":next", reading, "", nil); w.Code != 410 || !strings.Contains(w.Body.String(), "view_changed") {
		t.Fatal("cursor survived unfreeze", w.Code, w.Body.String())
	}
	for _, id := range post {
		if w := x.call(t, "GET", root+"/objects/"+typ+"/"+id, reading, "", nil); w.Code != 200 {
			t.Fatal("unfreeze lost records", id, w.Code)
		}
	}
	freeze(writer, root, "unfreeze", 409)
	if w := x.call(t, "POST", root+"/readers/nobody:freeze", writer, "", nil); w.Code != 404 {
		t.Fatal("missing reader", w.Code)
	}
}

// Existing and future group members, including a shared-reads installation,
// inherit the same cut. Ending and recreating the grant must not restore it.
func TestFrozenGroupReachesFutureMembersAndSharedReads(t *testing.T) {
	x := tenants(t)
	secret, homes := x.household(t, "m1", "m2")
	dataset := homes["m1"]
	root := "/v1/datasets/" + dataset
	owner := x.signInGrants(t, "m1", dataset, url.Values{"write": {"yes"}})
	admin := x.signIn(t, "m1", dataset, true)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	insert := func() string {
		t.Helper()
		w := x.call(t, "POST", root+"/objects/home.track", owner, `{"fields":{"n":{"type":"integer","value":1}}}`, nil)
		var reply struct {
			Objects []struct {
				ID string `json:"id"`
			} `json:"objects"`
		}
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &reply) != nil || len(reply.Objects) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		return reply.Objects[0].ID
	}
	before := insert()
	if w := x.admin(t, "PUT", "/datasets/"+dataset+"/readers/issuer:home", admin, `{"types":["home.track"]}`); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	reader := x.signIn(t, "m2", dataset, false)
	shared := x.authorize(t, "m2", password, url.Values{"shared_reads": {"true"}}, url.Values{"shared_reads": {"yes"}, "dataset": {""}}).Access
	if w := x.call(t, "POST", root+"/readers/issuer:home:freeze", owner, "", nil); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	after := insert()
	x.join(t, secret, "m3")
	future := x.signIn(t, "m3", dataset, false)
	check := func(token string) {
		t.Helper()
		for _, id := range []string{before, after} {
			want := 200
			if id == after {
				want = 404
			}
			w := x.call(t, "GET", root+"/objects/home.track/"+id, token, "", nil)
			if w.Code != want || (want == 404 && !strings.Contains(w.Body.String(), "unavailable")) {
				t.Fatal("group cut lost", w.Code, w.Body.String())
			}
		}
	}
	for _, token := range []string{reader, shared, future} {
		check(token)
	}
	if w := x.call(t, "GET", root+"/readers/issuer:home", owner, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"frozen_by":"m1"`) {
		t.Fatal("group attribution missing", w.Code, w.Body.String())
	}
	if w := x.call(t, "POST", root+"/readers/m2:freeze", owner, "", nil); w.Code != 409 {
		t.Fatal("group member changed independently", w.Code)
	}
	if w := x.admin(t, "DELETE", "/datasets/"+dataset+"/readers/issuer:home", admin, ""); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := x.admin(t, "PUT", "/datasets/"+dataset+"/readers/issuer:home", admin, `{"types":["home.track","home.note"]}`); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Reopening also proves that the retained group cut survives revocation.
	must(x.runtime.Close())
	reopened, err := standalone.Open(t.Context(), x.runtime.Config)
	must(err)
	x.runtime = reopened
	t.Cleanup(func() { reopened.Close() })
	reader = x.signIn(t, "m2", dataset, false)
	future = x.signIn(t, "m3", dataset, false)
	for _, token := range []string{reader, shared, future} {
		check(token)
	}
	if w := x.call(t, "POST", root+"/readers/issuer:home:unfreeze", owner, "", nil); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, token := range []string{reader, shared, future} {
		if w := x.call(t, "GET", root+"/objects/home.track/"+after, token, "", nil); w.Code != 200 {
			t.Fatal("group unfreeze failed", w.Code, w.Body.String())
		}
	}
	// A fresh administrative freeze is allowed, using the administration scope.
	if w := x.admin(t, "POST", "/datasets/"+dataset+"/readers/issuer:home:freeze", admin, ""); w.Code != 204 {
		t.Fatal("administrator freeze failed", w.Code, w.Body.String())
	}
	last := insert()
	if w := x.call(t, "GET", root+"/objects/home.track/"+last, reader, "", nil); w.Code != 404 {
		t.Fatal("administrator freeze did not narrow the view", w.Code)
	}
	must(x.runtime.Authority.RemoveGroupReader(t.Context(), "m1", dataset, "home"))
}

// Removing a named share can expose its group fallback, but cannot erase the
// named share's withdrawal and thereby let an administrator restore reading.
func TestFrozenNamedShareSurvivesGroupFallback(t *testing.T) {
	x := tenants(t)
	_, homes := x.household(t, "m1", "m2")
	dataset := homes["m1"]
	root := "/v1/datasets/" + dataset
	owner := x.signInGrants(t, "m1", dataset, url.Values{"write": {"yes"}})
	admin := x.signIn(t, "m1", dataset, true)
	for _, account := range []string{"issuer:home", "m2"} {
		if w := x.admin(t, "PUT", "/datasets/"+dataset+"/readers/"+account, admin, `{"types":["home.track"]}`); w.Code != 204 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := x.call(t, "POST", root+"/readers/m2:freeze", owner, "", nil); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := x.call(t, "POST", root+"/objects/home.track", owner, `{"fields":{}}`, nil); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := x.admin(t, "DELETE", "/datasets/"+dataset+"/readers/m2", admin, ""); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	reading := x.signIn(t, "m2", dataset, false)
	w := x.call(t, "POST", root+"/objects/home.track:count", reading, `{}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"count":0`) {
		t.Fatal("group fallback restored a frozen named share", w.Code, w.Body.String())
	}
}

// A dataset created through the ordinary API records its creating account,
// even when another account later receives identical full grants.
func TestFrozenReaderOwnerFromDatasetCreation(t *testing.T) {
	x := tenants(t)
	_, err := x.runtime.Authority.AddService("analysis", "", store.Grants{})
	if err != nil {
		t.Fatal(err)
	}
	if err = x.runtime.Authority.GrantDataset("analysis", x.bobSet, store.Grants{Read: true}); err != nil {
		t.Fatal(err)
	}
	if err = x.runtime.Authority.GrantDataset(x.alice, x.bobSet, store.Grants{Read: true, Write: true, Schema: true, Erase: true}); err != nil {
		t.Fatal(err)
	}
	owner := x.signInGrants(t, x.bob, x.bobSet, url.Values{"write": {"yes"}})
	other := x.signInGrants(t, x.alice, x.bobSet, url.Values{"read": {"yes"}, "write": {"yes"}, "administer": {"yes"}})
	root := "/v1/datasets/" + x.bobSet
	for _, c := range []struct {
		token, route, action string
		want                 int
	}{
		{owner, root, "freeze", 204},
		{other, root, "unfreeze", 403},
		{other, "/admin/api/v1/datasets/" + x.bobSet, "unfreeze", 403},
		{owner, root, "unfreeze", 204},
	} {
		w := x.call(t, "POST", c.route+"/readers/analysis:"+c.action, c.token, "", nil)
		if w.Code != c.want {
			t.Fatal(c.action, w.Code, w.Body.String())
		}
	}
}
