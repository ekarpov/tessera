package security_test

import (
	"strings"
	"testing"
)

// The three-answer rule, swept over the protocol surface rather than checked
// at one route. A dataset the caller may not use must answer exactly as an
// identifier naming nothing does; anything else tells them it exists.
//
// Written as a sweep because the finding this replaces was one route that had
// been missed while the rule held everywhere else. A route added later and
// left out fails here without anybody having to remember.
func TestAForeignDatasetAnswersAsAnAbsentOneOnEveryRoute(t *testing.T) {
	x := tenants(t)
	typeID, object, _ := x.seed(t, x.bobTok, x.bobSet, x.bobStore)

	for _, route := range []struct {
		name, method, path, body string
	}{
		{"describe the dataset", "GET", "/v1/datasets/%s/meta", ""},
		{"describe one type", "GET", "/v1/datasets/%s/meta/" + typeID, ""},
		{"list child types", "GET", "/v1/datasets/%s/meta/" + typeID + "/children", ""},
		{"read one record", "GET", "/v1/datasets/%s/objects/" + typeID + "/" + object, ""},
		{"query records", "POST", "/v1/datasets/%s/objects/" + typeID + ":query", `{"query":{}}`},
		{"count records", "POST", "/v1/datasets/%s/objects/" + typeID + ":count", `{}`},
		{"begin a page set", "POST", "/v1/datasets/%s/objects/" + typeID + ":page", `{"query":{},"page_size":2}`},
		{"define a type", "POST", "/v1/datasets/%s/meta", `{"type":{"id":"security.intruder","name":"X","fields":[]}}`},
		{"insert a record", "POST", "/v1/datasets/%s/objects/" + typeID, `{"fields":{"n":{"type":"integer","value":1}}}`},
		{"change a record", "PATCH", "/v1/datasets/%s/objects/" + typeID + "/" + object, `{"fields":{"n":{"type":"integer","value":2}}}`},
		{"delete a record", "DELETE", "/v1/datasets/%s/objects/" + typeID + "/" + object, ""},
	} {
		t.Run(route.name, func(t *testing.T) {
			foreign := x.answer(t, route.method, strings.Replace(route.path, "%s", x.bobSet, 1), x.aliceTok, route.body)
			absent := x.answer(t, route.method, strings.Replace(route.path, "%s", absentDataset, 1), x.aliceTok, route.body)
			if foreign != absent {
				t.Fatal("a dataset this caller may not use is distinguishable from one that does not exist", foreign, absent)
			}
			if foreign.Status < 400 {
				t.Fatal("a dataset this caller may not use answered successfully", foreign)
			}
		})
	}
}

// The same rule for content. A store belonging to a dataset the caller cannot
// reach must answer as a store identifier naming nothing — the finding here
// was that one said permission_denied and the other said unavailable, so the
// pair reported whether the store existed.
func TestAForeignContentStoreAnswersAsAnAbsentOne(t *testing.T) {
	x := tenants(t)
	_, _, item := x.seed(t, x.bobTok, x.bobSet, x.bobStore)

	for _, route := range []struct {
		name, method, path, body string
	}{
		{"inspect an item", "GET", "/v1/stores/%s/items/" + item + "?representation=original", ""},
		{"resolve an item", "POST", "/v1/stores/%s/items/" + item + ":resolve", `{"representation":"original"}`},
		{"delete an item", "DELETE", "/v1/stores/%s/items/" + item, ""},
		{"begin an import", "POST", "/v1/stores/%s/imports", `{"operation_id":"0c5e2d914b7300000000000000000000","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":10}`},
	} {
		t.Run(route.name, func(t *testing.T) {
			foreign := x.answer(t, route.method, strings.Replace(route.path, "%s", x.bobStore, 1), x.aliceTok, route.body)
			absent := x.answer(t, route.method, strings.Replace(route.path, "%s", absentStore, 1), x.aliceTok, route.body)
			if foreign != absent {
				t.Fatal("a content store this caller may not use is distinguishable from one that does not exist", foreign, absent)
			}
			if foreign.Status < 400 {
				t.Fatal("a content store this caller may not use answered successfully", foreign)
			}
		})
	}
}

// What a caller is told exists. Neither listing may mention the other tenant.
func TestNeitherListingMentionsTheOtherTenant(t *testing.T) {
	x := tenants(t)
	x.seed(t, x.bobTok, x.bobSet, x.bobStore)
	for _, path := range []string{"/v1/datasets", "/v1/stores"} {
		body := x.call(t, "GET", path, x.aliceTok, "", nil).Body.String()
		if strings.Contains(body, x.bobSet) || strings.Contains(body, x.bobStore) {
			t.Fatal("a listing named the other tenant", path, body)
		}
		if !strings.Contains(body, x.aliceSet) && !strings.Contains(body, x.aliceStore) {
			t.Fatal("a listing named nothing of the caller's own", path, body)
		}
	}
}
