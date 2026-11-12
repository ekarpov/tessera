package security_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/pkg/store"
)

// A legacy attachment row belongs to the dataset that wrote it. A reader
// sharing a type in the item's dataset must not inherit access from a
// matching type in a different dataset.
func TestAShareOfOneDatasetIsNotAKeyToAnothersReferences(t *testing.T) {
	x := tenants(t)
	typeID, _, _ := x.seed(t, x.aliceTok, x.aliceSet, x.aliceStore)
	// A second item in Alice's store, referenced by nothing in her dataset.
	loose := x.upload(t, x.aliceTok, x.aliceStore, "cccccccccccccccccccccccccccccccc")

	// Carol has write access to the item store and creates the second dataset.
	// The current protocol still refuses a write crossing that boundary.
	if err := x.runtime.Authority.AddPerson("carol", password, x.aliceSet,
		store.Grants{Read: true, Write: true, Schema: true}, true); err != nil {
		t.Fatal(err)
	}
	carol, err := x.runtime.Authority.AddAutomation(t.Context(), "carol", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	made := x.call(t, "POST", "/v1/datasets", carol, `{"display_name":"Carol"}`,
		map[string]string{"Idempotency-Key": "3f2504e0-4f89-11d3-9a0c-0305e82c3301"})
	if made.Code != 201 {
		t.Fatal("carol could not create a dataset", made.Code, made.Body.String())
	}
	var created struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(made.Body.Bytes(), &created) != nil || created.DatasetID == "" {
		t.Fatal("no identity in the creation response", made.Body.String())
	}

	// The same type identifier as Alice's, in Carol's new dataset, holding
	// the reference. A type identifier is a name and two datasets may use
	// the same one, which is what made a share of Alice's type match a row
	// written by somebody else's.
	if w := x.call(t, "POST", "/v1/datasets/"+created.DatasetID+"/meta", carol,
		`{"type":{"id":"`+typeID+`","name":"Record","fields":[{"id":"photo","meta_id":"`+typeID+`","type":"media"}]}}`, nil); w.Code != 201 {
		t.Fatal("could not define the crossing type", w.Code, w.Body.String())
	}
	records := "/v1/datasets/" + created.DatasetID + "/objects/" + typeID
	if w := x.call(t, "POST", records, carol,
		`{"fields":{"photo":{"type":"media","value":"`+x.aliceStore+`//`+loose+`"}}}`, nil); w.Code != 403 {
		t.Fatal("cross-dataset write was not refused", w.Code)
	}
	createdRecord := x.call(t, "POST", records, carol, `{"fields":{}}`, nil)
	var inserted struct{ Objects []struct{ ID string } }
	if createdRecord.Code != 201 || json.Unmarshal(createdRecord.Body.Bytes(), &inserted) != nil || len(inserted.Objects) != 1 {
		t.Fatal("could not create the legacy fixture record")
	}
	access, err := x.runtime.Authority.Authenticate(t.Context(), carol)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the old attachment index entry that the visibility query reads.
	if err = x.runtime.Engine.WithTx(t.Context(), access, created.DatasetID, func(_ store.Tx, tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO "tessera_attachments" ("dataset_id","object_id","type_id","field_id","store_id","item_id")
			VALUES ($1,$2,$3,'photo',$4,$5)`, created.DatasetID, inserted.Objects[0].ID, typeID, x.aliceStore, loose)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// A guest sharing that type of Alice's, and holding nothing of Carol's.
	if err = x.runtime.Authority.AddPerson("guest", password, x.aliceSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	guest, err := x.runtime.Authority.AddAutomation(t.Context(), "guest", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	shared := ""
	for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), "alice").Authorizations {
		if auth.Principal == "guest" {
			shared = auth.ID
		}
	}
	if shared == "" {
		t.Fatal("fixture should offer the guest's installation")
	}
	if err = x.runtime.Authority.GrantRead(t.Context(), shared, []string{typeID}, nil); err != nil {
		t.Fatal(err)
	}
	if w := x.call(t, "GET", "/v1/datasets/"+created.DatasetID+"/meta", guest, "", nil); w.Code < 400 {
		t.Fatal("the crossing dataset is not out of the guest's reach", w.Code)
	}

	for _, route := range []struct{ method, path, body string }{
		{"GET", "/v1/stores/" + x.aliceStore + "/items/" + loose + "?representation=original", ""},
		{"POST", "/v1/stores/" + x.aliceStore + "/items/" + loose + ":resolve", `{"representation":"original"}`},
	} {
		reached := x.answer(t, route.method, route.path, guest, route.body)
		absent := x.answer(t, route.method, strings.Replace(route.path, loose, "c-"+strings.Repeat("b", 43), 1), guest, route.body)
		if reached.Status < 400 {
			t.Fatal("a share of one dataset reached an item held only by another's record", route.path, reached)
		}
		if reached != absent {
			t.Fatal("an item referenced only from another dataset is distinguishable from one that does not exist", reached, absent)
		}
	}
}

// upload puts bytes in a store and commits them, without attaching the item
// to any record.
func (x *tenancy) upload(t *testing.T, token, storeID, operation string) string {
	t.Helper()
	begin := x.call(t, "POST", "/v1/stores/"+storeID+"/imports", token,
		`{"operation_id":"`+operation+`","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":10}`, nil)
	if begin.Code != 201 && begin.Code != 200 {
		t.Fatal("could not begin an import", begin.Code, begin.Body.String())
	}
	var opened struct {
		UploadID string `json:"upload_id"`
		URL      string `json:"url"`
	}
	if json.Unmarshal(begin.Body.Bytes(), &opened) != nil || opened.URL == "" {
		t.Fatal("no transfer url", begin.Body.String())
	}
	transfer := httptest.NewRequest("PATCH", strings.TrimPrefix(opened.URL, "https://tessera.example"), bytes.NewReader([]byte("key")))
	transfer.Header.Set("Content-Range", "bytes 0-2/3")
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, transfer)
	if w.Code >= 400 {
		t.Fatal("could not send bytes", w.Code, w.Body.String())
	}
	commit := x.call(t, "POST", "/v1/stores/"+storeID+"/imports/"+opened.UploadID+":commit", token, "", nil)
	if commit.Code >= 400 {
		t.Fatal("could not commit an import", commit.Code, commit.Body.String())
	}
	var committed struct {
		ItemID string `json:"item_id"`
	}
	if json.Unmarshal(commit.Body.Bytes(), &committed) != nil || committed.ItemID == "" {
		t.Fatal("no item identity", commit.Body.String())
	}
	return committed.ItemID
}
