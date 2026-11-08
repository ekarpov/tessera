package security_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ekarpov/tessera/pkg/store"
)

// A service acting as itself names its dataset on every request, which is the
// argued reason not to infer it from a storage that currently holds one.
// Reuse keyed on the credential alone served the second request the first
// request's authorization, so the requirement held only until a token had
// been issued.
func TestAServiceTokenIsBoundToTheDatasetItNames(t *testing.T) {
	x := tenants(t)
	secret, err := x.runtime.Authority.AddService("synthetic-service", x.aliceSet, store.Grants{Read: true, Write: true, Schema: true})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth("synthetic-service", secret)
		w := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(w, r)
		return w
	}
	first := ask("grant_type=client_credentials&dataset=" + x.aliceSet)
	if first.Code != 200 {
		t.Fatal("a service was refused the dataset it holds", first.Code, first.Body.String())
	}
	if w := ask("grant_type=client_credentials"); w.Code != 400 {
		t.Fatal("a request naming no dataset was served the previous request's authorization", w.Code)
	}
	if w := ask("grant_type=client_credentials&dataset=" + x.bobSet); w.Code != 400 {
		t.Fatal("a request naming another tenant's dataset was served one it holds", w.Code)
	}
	// And the token it did get reaches only what it named.
	var issued struct {
		Token string `json:"access_token"`
	}
	if json.Unmarshal(first.Body.Bytes(), &issued) != nil || issued.Token == "" {
		t.Fatal("no token issued", first.Body.String())
	}
	foreign := x.answer(t, "GET", "/v1/datasets/"+x.bobSet+"/meta", issued.Token, "")
	absent := x.answer(t, "GET", "/v1/datasets/"+absentDataset+"/meta", issued.Token, "")
	if foreign != absent {
		t.Fatal("a service token distinguished another tenant's dataset from one that does not exist", foreign, absent)
	}
}

// Revoking credentials must end them everywhere, including on datasets they
// created, which the registry never hears about.
func TestRevocationEndsCredentialsEverywhere(t *testing.T) {
	x := tenants(t)
	bobs := ""
	for _, auth := range x.runtime.Authority.AdminOverview(t.Context(), "bob").Authorizations {
		if auth.Dataset == x.bobSet {
			bobs = auth.ID
		}
	}
	if bobs == "" {
		t.Fatal("fixture should offer bob his own installation")
	}
	if w := x.call(t, "GET", "/v1/datasets/"+x.bobSet+"/meta", x.bobTok, "", nil); w.Code != 200 {
		t.Fatal("fixture token cannot read its own dataset", w.Code)
	}
	if err := x.runtime.Authority.RevokeAuthorization(t.Context(), bobs); err != nil {
		t.Fatal(err)
	}
	if w := x.call(t, "GET", "/v1/datasets/"+x.bobSet+"/meta", x.bobTok, "", nil); w.Code < 400 {
		t.Fatal("a revoked token still read its dataset", w.Code)
	}
	if w := x.call(t, "GET", "/v1/datasets", x.bobTok, "", nil); w.Code < 400 {
		t.Fatal("a revoked token still listed datasets", w.Code, w.Body.String())
	}
}

// Creating a dataset records its creator as holding it, because otherwise
// nobody would. The notice that does it runs on a replay too, so that
// bookkeeping which failed once is retried by the retry the caller was going
// to make — and writing the grants unconditionally turned that replay into a
// way to undo an administrator.
//
// Reported by review: create, have your grants reduced, replay the creation,
// hold it fully again.
func TestReplayingACreationDoesNotRestoreWhatAnAdministratorTookAway(t *testing.T) {
	x := tenants(t)
	const key = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	bootstrap, err := x.runtime.Authority.AddAutomation(t.Context(), "bob", "")
	if err != nil {
		t.Fatal(err)
	}
	made := x.call(t, "POST", "/v1/datasets", bootstrap, `{"display_name":"Replayed"}`,
		map[string]string{"Idempotency-Key": key})
	if made.Code != 201 {
		t.Fatal("creation was refused", made.Code, made.Body.String())
	}
	var created struct {
		DatasetID string `json:"dataset_id"`
	}
	if json.Unmarshal(made.Body.Bytes(), &created) != nil || created.DatasetID == "" {
		t.Fatal("no identity in the creation response", made.Body.String())
	}
	if !x.runtime.Authority.Administers("bob")[created.DatasetID] {
		t.Fatal("the creator was not recorded as holding what they made")
	}

	// An administrator of that dataset reduces the account to reading.
	if err = x.runtime.Authority.GrantDataset("bob", created.DatasetID, store.Grants{Read: true}); err != nil {
		t.Fatal(err)
	}
	if x.runtime.Authority.Administers("bob")[created.DatasetID] {
		t.Fatal("reducing the account left it administering")
	}

	// The same request again. It must answer with the identity it made and
	// change nothing else.
	replay := x.call(t, "POST", "/v1/datasets", bootstrap, `{"display_name":"Replayed"}`,
		map[string]string{"Idempotency-Key": key})
	if replay.Code != 201 || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("the replay did not answer as a replay", replay.Code, replay.Body.String())
	}
	if !strings.Contains(replay.Body.String(), created.DatasetID) {
		t.Fatal("the replay answered with a different identity", replay.Body.String())
	}
	if x.runtime.Authority.Administers("bob")[created.DatasetID] {
		t.Fatal("replaying a creation restored privileges an administrator had taken away")
	}
	// Removed entirely, not merely reduced: a replay must not put it back.
	if err = x.runtime.Authority.GrantDataset("bob", created.DatasetID, store.Grants{}); err != nil {
		t.Fatal(err)
	}
	if replayed := x.call(t, "POST", "/v1/datasets", bootstrap, `{"display_name":"Replayed"}`,
		map[string]string{"Idempotency-Key": key}); replayed.Code != 201 {
		t.Fatal("the replay stopped working", replayed.Code, replayed.Body.String())
	}
	held := x.runtime.Authority.AdminOverview(t.Context(), "bob")
	_ = held
	if x.runtime.Authority.Administers("bob")[created.DatasetID] {
		t.Fatal("replaying a creation re-added an account an administrator had removed")
	}
}

// An insert names what it stored and carries no fields, so a client that wants
// the row reads it back: two round trips for the most latency-visible thing a
// person does. Asking for the representation is one header, and what comes
// back is what a read would have returned — produced by the same path, so the
// two cannot contradict each other.
//
// Requested by a client whose record editor pays that second call on every
// write. Their point about the concrete type is why the row is read back by
// its own type rather than by the one the request named: a read of the
// requested type could be a projection of a different object.
func TestAnInsertCanCarryTheRowItStored(t *testing.T) {
	x := tenants(t)
	typeID, _, _ := x.seed(t, x.aliceTok, x.aliceSet, x.aliceStore)
	body := `{"fields":{"n":{"type":"integer","value":7}}}`
	path := "/v1/datasets/" + x.aliceSet + "/objects/" + typeID

	// Without the header: identities, and nothing that says otherwise.
	plain := x.call(t, "POST", path, x.aliceTok, body, nil)
	if plain.Code != 201 {
		t.Fatal("insert failed", plain.Code, plain.Body.String())
	}
	if strings.Contains(plain.Body.String(), "record_set") {
		t.Fatal("an insert carried a representation nobody asked for")
	}
	if plain.Header().Get("Preference-Applied") != "" {
		t.Fatal("a preference was reported applied that was never expressed")
	}

	// With it: the stored row, its own concrete type, and its version.
	asked := x.call(t, "POST", path, x.aliceTok, body,
		map[string]string{"Prefer": "return=representation"})
	if asked.Code != 201 {
		t.Fatal("insert failed", asked.Code, asked.Body.String())
	}
	if asked.Header().Get("Preference-Applied") != "return=representation" {
		t.Fatal("the preference was applied and not reported")
	}
	var written struct {
		Objects []struct {
			ID string `json:"id"`
		} `json:"objects"`
		RecordSet *struct {
			Meta struct {
				ID string `json:"id"`
			} `json:"meta"`
			Records []struct {
				ID struct {
					ID        string `json:"id"`
					DatasetID string `json:"dataset_id"`
				} `json:"id"`
				MetaID  string `json:"meta_id"`
				Version string `json:"version"`
			} `json:"records"`
		} `json:"record_set"`
	}
	if json.Unmarshal(asked.Body.Bytes(), &written) != nil || written.RecordSet == nil {
		t.Fatal("no representation in the answer", asked.Body.String())
	}
	if len(written.RecordSet.Records) != 1 || len(written.Objects) != 1 {
		t.Fatal("the answer did not name exactly what it stored", asked.Body.String())
	}
	row := written.RecordSet.Records[0]
	if row.ID.ID != written.Objects[0].ID {
		t.Fatal("the representation and the identity disagree", asked.Body.String())
	}
	if row.ID.DatasetID != x.aliceSet {
		t.Fatal("the representation does not qualify its identity with the dataset", row.ID.DatasetID)
	}
	if row.MetaID != typeID {
		t.Fatal("the representation does not name the concrete type it landed on", row.MetaID)
	}
	if row.Version == "" {
		t.Fatal("the representation carries no version, so a client still has to read for one")
	}

	// And it is what a read gives. Not similar to it: the same, because it is
	// produced by the read path rather than reshaped from the write.
	read := x.call(t, "GET", path+"/"+row.ID.ID, x.aliceTok, "", nil)
	if read.Code != 200 {
		t.Fatal("could not read back what was written", read.Code)
	}
	var fetched map[string]json.RawMessage
	if json.Unmarshal(read.Body.Bytes(), &fetched) != nil {
		t.Fatal("unreadable record", read.Body.String())
	}
	var carried map[string]json.RawMessage
	if json.Unmarshal(asked.Body.Bytes(), &carried) != nil {
		t.Fatal(asked.Body.String())
	}
	var inner map[string]json.RawMessage
	if json.Unmarshal(carried["record_set"], &inner) != nil {
		t.Fatal(string(carried["record_set"]))
	}
	for _, member := range []string{"meta", "records"} {
		if !bytes.Equal(inner[member], fetched[member]) {
			t.Fatal("the representation differs from a read of the same object", member,
				string(inner[member]), string(fetched[member]))
		}
	}
}
