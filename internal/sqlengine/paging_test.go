package sqlengine

import (
	"bytes"
	"context"
	"encoding/json"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"testing"
	"time"
)

func TestPageReplayMutationRestartAndRelease(t *testing.T) {
	s, path, dataset := fixture(t)
	ids := []string{}
	for i := 0; i < 5; i++ {
		reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, identity(t, reply))
	}
	cursor, err := beginTestPage(s, t.Context(), owner, dataset, typeID, p.Query{IncludeVersions: true}, 2)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.NextPage(t.Context(), owner, dataset, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error { return tx.Delete(typeID, ids[0]) }); err != nil {
		t.Fatal(err)
	}
	replay, err := s.NextPage(t.Context(), owner, dataset, cursor)
	if err != nil || !bytes.Equal(first, replay) {
		t.Fatal("page changed after deletion", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seen := map[string]bool{}
	next := cursor
	last := ""
	for next != "" {
		raw, e := s.NextPage(t.Context(), owner, dataset, next)
		if e != nil {
			t.Fatal(e)
		}
		var page struct {
			p.RecordSet
			Next *string `json:"next_cursor"`
		}
		if e = json.Unmarshal(raw, &page); e != nil {
			t.Fatal(e)
		}
		for _, r := range page.Records {
			if seen[r.ID.ID] {
				t.Fatal("duplicate record")
			}
			seen[r.ID.ID] = true
			if r.Version == "" {
				t.Fatal("missing version")
			}
		}
		last = next
		next = ""
		if page.Next != nil {
			next = *page.Next
		}
	}
	if len(seen) != 5 {
		t.Fatal("stable records skipped", seen)
	}
	if err = s.EndPage(t.Context(), owner, dataset, last); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPage(t.Context(), owner, dataset, cursor); p.ErrorCode(err) != "invalid_cursor" {
		t.Fatal(err)
	}
}
func TestPageAuthorizationExpiryAndBounds(t *testing.T) {
	s, _, dataset := fixture(t)
	cursor, err := beginTestPage(s, t.Context(), owner, dataset, typeID, p.Query{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPage(t.Context(), store.Access{Principal: owner.Principal, Authorization: "other"}, dataset, cursor); p.ErrorCode(err) != "invalid_cursor" {
		t.Fatal(err)
	}
	if _, err = s.NextPage(t.Context(), owner, dataset, "tampered"); p.ErrorCode(err) != "invalid_cursor" {
		t.Fatal(err)
	}
	if _, err = beginTestPage(s, t.Context(), owner, dataset, typeID, p.Query{}, 0); p.ErrorCode(err) != "query_result_limit_exceeded" {
		t.Fatal(err)
	}
	if err = s.Revoke(t.Context(), sole(t, s), owner); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPage(t.Context(), owner, dataset, cursor); p.ErrorCode(err) != "view_changed" {
		t.Fatal(err)
	}
	auth := store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}
	if err = s.SetAuthorization(t.Context(), sole(t, s), owner, auth); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPage(t.Context(), owner, dataset, cursor); p.ErrorCode(err) != "view_changed" {
		t.Fatal("regrant revived old cursor", err)
	}
	cursor, err = beginTestPage(s, t.Context(), owner, dataset, typeID, p.Query{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE "tessera_sequences" SET "expires_at"=?`, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPage(t.Context(), owner, dataset, cursor); p.ErrorCode(err) != "cursor_expired" {
		t.Fatal(err)
	}
}

func beginTestPage(s *Store, ctx context.Context, a store.Access, dataset, typ string, q p.Query, size int) (string, error) {
	start, err := s.BeginPage(ctx, a, dataset, typ, q, size)
	return start.Cursor, err
}

// A page set keeps its schema once, on the sequence, and every page is
// served exactly as it was when each page carried its own — including a page
// stored that way before the schema moved.
func TestAPageIsServedWithTheSchemaStoredOnce(t *testing.T) {
	s, _, dataset := fixture(t)
	for i := 0; i < 4; i++ {
		if _, err := s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody)); err != nil {
			t.Fatal(err)
		}
	}
	cursor, err := beginTestPage(s, t.Context(), owner, dataset, typeID, p.Query{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	served, err := s.NextPage(t.Context(), owner, dataset, cursor)
	if err != nil {
		t.Fatal(err)
	}
	var page map[string]json.RawMessage
	if err = json.Unmarshal(served, &page); err != nil || page["meta"] == nil || page["records"] == nil || page["next_cursor"] == nil {
		t.Fatal("a served page lacks what a page carries:", string(served))
	}
	var stored []byte
	if err = s.db.QueryRow(`SELECT "body" FROM "tessera_pages" WHERE "cursor"=$1`, cursor).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(`"meta"`)) {
		t.Fatal("the schema is still stored in every page")
	}
	// The same page as it was stored before: whole, and nothing on the
	// sequence. It is served unchanged.
	if _, err = s.db.Exec(`UPDATE "tessera_pages" SET "body"=$1 WHERE "cursor"=$2`, []byte(served), cursor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE "tessera_sequences" SET "meta"=NULL`); err != nil {
		t.Fatal(err)
	}
	again, err := s.NextPage(t.Context(), owner, dataset, cursor)
	if err != nil || !bytes.Equal(again, served) {
		t.Fatal("a page stored whole was not served as it was stored", err)
	}
}

// One caller's page sets count against that caller's bound, not everybody's:
// a caller holding a large page set must not refuse paging to another on the
// same dataset (finding 35), and still meets its own bound.
func TestPageStorageIsCountedPerCaller(t *testing.T) {
	s, _, dataset := fixture(t)
	for i := 0; i < 40; i++ {
		if _, err := s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody)); err != nil {
			t.Fatal(err)
		}
	}
	reader := store.Access{Principal: "reader", Authorization: "reader-app"}
	if err := s.SetAuthorization(t.Context(), dataset, reader, store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	// A page set's storage grows as it is read, a page at a time, so each is
	// read to its end: that is when it holds the most.
	walk := func(a store.Access) error {
		begun, err := s.BeginPage(t.Context(), a, dataset, typeID, p.Query{}, 1)
		if err != nil {
			return err
		}
		for cursor := begun.Cursor; cursor != ""; {
			raw, err := s.NextPage(t.Context(), a, dataset, cursor)
			if err != nil {
				return err
			}
			var page struct {
				Next *string `json:"next_cursor"`
			}
			if err = json.Unmarshal(raw, &page); err != nil {
				return err
			}
			cursor = ""
			if page.Next != nil {
				cursor = *page.Next
			}
		}
		return nil
	}
	// Measure one page set read whole, then bound the dataset to a little
	// under two.
	if err := walk(owner); err != nil {
		t.Fatal(err)
	}
	var one int64
	if err := s.db.QueryRow(`SELECT coalesce(sum(length(p."body")),0)+(SELECT sum(length("query"))+sum(coalesce(length("meta"),0)) FROM "tessera_sequences") FROM "tessera_pages" p`).Scan(&one); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM "tessera_sequences"`); err != nil {
		t.Fatal(err)
	}
	limits, err := s.DatasetLimits(t.Context(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	limits.MaxMaterializedBytes = int(one * 3 / 2)
	if err = s.SetDatasetLimits(t.Context(), dataset, limits); err != nil {
		t.Fatal(err)
	}
	if err = walk(owner); err != nil {
		t.Fatal(err)
	}
	if err = walk(reader); err != nil {
		t.Fatal("one caller's page set refused another's:", err)
	}
	if err = walk(owner); p.ErrorCode(err) != "query_result_limit_exceeded" {
		t.Fatal("a caller went past its own page storage bound:", err)
	}
}
