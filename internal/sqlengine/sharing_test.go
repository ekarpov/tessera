package sqlengine

import (
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"testing"
)

func TestReadOnlySharingAndCursorWithdrawal(t *testing.T) {
	s, _, dataset := fixture(t)
	reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody))
	if err != nil {
		t.Fatal(err)
	}
	id := identity(t, reply)
	guest := store.Access{Principal: "guest", Authorization: "reader"}
	auth := store.Authorization{Grants: store.Grants{Read: true}, View: store.SharedView(nil, store.ObjectKey{TypeID: typeID, ObjectID: id})}
	if err = s.SetAuthorization(t.Context(), sole(t, s), guest, auth); err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(t.Context(), guest, sole(t, s), func(tx store.Tx) error { _, e := tx.Object(typeID, id, nil); return e }); err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(t.Context(), guest, sole(t, s), func(tx store.Tx) error { _, e := tx.Query(typeID, p.Query{}, 10); return e }); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("object grant discoverable", err)
	}
	if err = s.Transact(t.Context(), guest, sole(t, s), func(tx store.Tx) error { _, e := tx.Object(typeID, "absent", nil); return e }); p.ErrorCode(err) != "unavailable" {
		t.Fatal("hidden existence disclosed", err)
	}
	bad := auth
	bad.Grants.Write = true
	if err = s.SetAuthorization(t.Context(), sole(t, s), guest, bad); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("guest write grant accepted", err)
	}
	auth.View = store.SharedView([]string{typeID})
	if err = s.SetAuthorization(t.Context(), sole(t, s), guest, auth); err != nil {
		t.Fatal(err)
	}
	before, err := s.Info(t.Context(), guest, sole(t, s))
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.BeginPage(t.Context(), guest, dataset, typeID, p.Query{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPage(t.Context(), guest, dataset, page.Cursor); err != nil {
		t.Fatal(err)
	}
	auth.View = store.SharedView(nil, store.ObjectKey{TypeID: typeID, ObjectID: id})
	if err = s.SetAuthorization(t.Context(), sole(t, s), guest, auth); err != nil {
		t.Fatal(err)
	}
	after, err := s.Info(t.Context(), guest, sole(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if before.ViewVersion == after.ViewVersion || before.Watermark != after.Watermark {
		t.Fatal("view and data generations conflated")
	}
	if _, err = s.NextPage(t.Context(), guest, dataset, page.Cursor); p.ErrorCode(err) != "view_changed" {
		t.Fatal("cached page survived withdrawal", err)
	}
}

// A type grant covers the named concrete type and nothing under it. Resolved by
// the author as review finding 9: creating a subtype must not silently widen a
// view that somebody else was granted earlier.
func TestTypeGrantsCoverOnlyTheNamedConcreteType(t *testing.T) {
	s, _, dataset := fixture(t)
	const child = "test.record.child"
	err := s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		parent := typeID
		_, e := tx.CreateMetadata(p.Meta{ID: child, ParentID: &parent, Fields: []p.Field{{ID: "m", MetaID: child, Type: p.IntegerType}}})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody)); err != nil {
		t.Fatal(err)
	}
	childInsert := `{"operations":[{"op":"insert","type":"` + child + `","fields":{"m":{"type":"integer","value":4}},"as":"entry"}]}`
	if _, err = s.Mutate(t.Context(), owner, mutation(t, dataset, "", childInsert)); err != nil {
		t.Fatal(err)
	}
	guest := store.Access{Principal: "guest", Authorization: "parent-only"}
	auth := store.Authorization{Grants: store.Grants{Read: true}, View: store.SharedView([]string{typeID})}
	if err = s.SetAuthorization(t.Context(), sole(t, s), guest, auth); err != nil {
		t.Fatal(err)
	}
	// The parent grant reveals the parent's own rows and not the subtype's,
	// through every read that can reach rows.
	count := func(scope string) int {
		t.Helper()
		var rows int
		e := s.Transact(t.Context(), guest, sole(t, s), func(tx store.Tx) error {
			q := p.Query{}
			if scope == "descendants" {
				q.TypeScope = &p.TypeScope{Kind: "descendants"}
			}
			set, err := tx.Query(typeID, q, 100)
			rows = len(set.Records)
			return err
		})
		if e != nil {
			t.Fatal(e)
		}
		return rows
	}
	if count("exact") != 1 || count("descendants") != 1 {
		t.Fatal("a descendant-scoped query crossed an exact type grant")
	}
	// The subtype itself is not readable, and is not disclosed as existing.
	if err = s.Transact(t.Context(), guest, sole(t, s), func(tx store.Tx) error { _, e := tx.Query(child, p.Query{}, 10); return e }); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("subtype readable through a parent grant", err)
	}
	// A subtype created after the grant does not widen it either.
	const later = "test.record.later"
	err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		parent := typeID
		_, e := tx.CreateMetadata(p.Meta{ID: later, ParentID: &parent, Fields: []p.Field{{ID: "k", MetaID: later, Type: p.IntegerType}}})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	laterInsert := `{"operations":[{"op":"insert","type":"` + later + `","fields":{"k":{"type":"integer","value":5}},"as":"entry"}]}`
	if _, err = s.Mutate(t.Context(), owner, mutation(t, dataset, "", laterInsert)); err != nil {
		t.Fatal(err)
	}
	if count("descendants") != 1 {
		t.Fatal("a subtype created after the grant widened the guest view")
	}
	// The guest sees one row because of the grant, not because descendant scope
	// is broken: the owner's identical query reaches all three.
	var ownerRows int
	if err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		set, e := tx.Query(typeID, p.Query{TypeScope: &p.TypeScope{Kind: "descendants"}}, 100)
		ownerRows = len(set.Records)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if ownerRows != 3 {
		t.Fatal("descendant scope is not reaching subtypes at all, so the guest check proves nothing", ownerRows)
	}
	// Aggregation and distinct references are reads too, and respect the same
	// intersection rather than counting rows the guest cannot see.
	var aggregates []p.Aggregate
	if err = s.Transact(t.Context(), guest, sole(t, s), func(tx store.Tx) error {
		var e error
		aggregates, e = tx.Aggregate(typeID, "n", "", nil)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 1 || aggregates[0].RowCount != 1 {
		t.Fatal("aggregate counted rows outside the guest's exact grant")
	}
	if err = s.Transact(t.Context(), guest, sole(t, s), func(tx store.Tx) error { _, e := tx.Aggregate(child, "m", "", nil); return e }); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("aggregate reached an ungranted subtype", err)
	}
}

// A data write is not a permission change. Conflating them would let an ordinary
// field edit invalidate every outstanding cursor, and would let a real
// withdrawal hide behind one.
func TestDataWritesDoNotChangeTheViewGeneration(t *testing.T) {
	s, _, dataset := fixture(t)
	guest := store.Access{Principal: "guest", Authorization: "reader"}
	auth := store.Authorization{Grants: store.Grants{Read: true}, View: store.SharedView([]string{typeID})}
	if err := s.SetAuthorization(t.Context(), sole(t, s), guest, auth); err != nil {
		t.Fatal(err)
	}
	before, err := s.Info(t.Context(), guest, sole(t, s))
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.BeginPage(t.Context(), guest, dataset, typeID, p.Query{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody)); err != nil {
		t.Fatal(err)
	}
	after, err := s.Info(t.Context(), guest, sole(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if before.ViewVersion != after.ViewVersion {
		t.Fatal("a data write moved the view generation")
	}
	if before.Watermark == after.Watermark {
		t.Fatal("a data write did not move the watermark")
	}
	// The cursor survives the write, because nothing about access changed.
	if _, err = s.NextPage(t.Context(), guest, dataset, page.Cursor); err != nil {
		t.Fatal("a data write invalidated an outstanding page", err)
	}
}

// A guest is read-only, and that is enforced where the write would happen, not
// only where the grant is issued. Filtered mutations and schema changes are
// writes like any other.
func TestGuestsCannotWriteByAnyRoute(t *testing.T) {
	s, _, dataset := fixture(t)
	if _, err := s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody)); err != nil {
		t.Fatal(err)
	}
	guest := store.Access{Principal: "guest", Authorization: "reader"}
	auth := store.Authorization{Grants: store.Grants{Read: true}, View: store.SharedView([]string{typeID})}
	if err := s.SetAuthorization(t.Context(), sole(t, s), guest, auth); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"insert":       insertBody,
		"update_where": `{"operations":[{"op":"update_where","type":"` + typeID + `","fields":{"n":{"type":"integer","value":1}}}]}`,
		"delete_where": `{"operations":[{"op":"delete_where","type":"` + typeID + `"}]}`,
		"define_type":  `{"operations":[{"op":"define_type","meta":{"id":"test.guest","fields":[]}}]}`,
		"delete_type":  `{"operations":[{"op":"delete_type","type":"` + typeID + `","cascade":true}]}`,
	} {
		_, err := s.Mutate(t.Context(), guest, mutation(t, dataset, "", body))
		if p.ErrorCode(err) != "permission_denied" {
			t.Fatal("a guest reached a write through "+name, err)
		}
	}
	// A write grant cannot be issued to a restricted view in the first place,
	// so the two defences agree rather than relying on one another.
	writing := auth
	writing.Grants.Write = true
	if err := s.SetAuthorization(t.Context(), sole(t, s), guest, writing); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("a restricted view was given write", err)
	}
}
