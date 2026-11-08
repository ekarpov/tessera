package recordstate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const (
	responses = "test.response"
	followUp  = "test.response.follow_up"
	questions = "test.question"
)

// contributorEngine holds a dataset with an owner and one contributor, which
// may add responses and, separately, read questions. The two scopes are the
// point: what a contributor adds to and what it sees are named apart.
func contributorEngine(t *testing.T) (*Store, store.Access, store.Access, string) {
	t.Helper()
	owner := store.Access{Principal: "owner", Authorization: "owner"}
	contributor := store.Access{Principal: "device", Authorization: "device"}
	parent := responses
	image := Image{ID: "s-0123456789abcdef0123456789abcdef", Next: 1, Watermark: "w:start",
		Types: map[string]p.Meta{
			responses: {ID: responses, Fields: []p.Field{{ID: "answer", MetaID: responses, Type: p.TextType}}},
			followUp:  {ID: followUp, ParentID: &parent, Fields: []p.Field{}},
			questions: {ID: questions, Fields: []p.Field{{ID: "text", MetaID: questions, Type: p.TextType}}},
		},
		Objects: map[string]Row{}}
	engine, err := Restore(image, p.DevelopmentLimits(), owner, store.Authorization{Grants: store.Grants{Read: true, Write: true}, View: store.FullView()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	grant := store.Authorization{Grants: store.Grants{Read: true, Insert: true}, View: store.SharedView([]string{questions}).WithInserts([]string{responses})}
	if err = engine.SetAuthorization(context.Background(), image.ID, contributor, grant); err != nil {
		t.Fatal(err)
	}
	return engine, owner, contributor, image.ID
}

func text(t *testing.T, s string) map[string]p.Value {
	t.Helper()
	v, err := p.NewValue(p.TextType, s)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]p.Value{"answer": v}
}

func code(err error) string { return p.ErrorCode(err) }

// A contributor adds records of the named type and of nothing else — not a
// descendant, which a type grant never covers, and not a type it only reads.
func TestAnInsertGrantAddsToItsTypesOnly(t *testing.T) {
	engine, _, contributor, dataset := contributorEngine(t)
	ctx := context.Background()
	var id string
	err := engine.Transact(ctx, contributor, dataset, func(tx store.Tx) error {
		result, e := tx.Insert(responses, text(t, "yes"))
		if e == nil {
			id = result.Records[0].ID.ID
		}
		return e
	})
	if err != nil || id == "" {
		t.Fatal("a contributor could not add to its type:", err)
	}
	for _, other := range []string{followUp, questions} {
		err = engine.Transact(ctx, contributor, dataset, func(tx store.Tx) error {
			_, e := tx.Insert(other, map[string]p.Value{})
			return e
		})
		if code(err) != "permission_denied" {
			t.Fatalf("a contributor added to %s: %v", other, err)
		}
	}
}

// What a contributor added is the dataset's the moment it commits. It cannot
// read it back, change it or delete it: an insert grant conveys no continuing
// authority over what was written.
func TestAContributorHasNoHoldOnWhatItAdded(t *testing.T) {
	engine, owner, contributor, dataset := contributorEngine(t)
	ctx := context.Background()
	var id string
	if err := engine.Transact(ctx, contributor, dataset, func(tx store.Tx) error {
		result, e := tx.Insert(responses, text(t, "yes"))
		if e == nil {
			id = result.Records[0].ID.ID
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Read(ctx, contributor, dataset, func(tx store.Tx) error {
		_, e := tx.Object(responses, id, nil)
		return e
	}); code(err) != "unavailable" {
		t.Fatal("a contributor read back what it added:", err)
	}
	if err := engine.Read(ctx, contributor, dataset, func(tx store.Tx) error {
		_, e := tx.Query(responses, p.Query{}, 10)
		return e
	}); err == nil {
		t.Fatal("a contributor queried the type it adds to")
	}
	if err := updateDose(engine, contributor, dataset, responses, id, text(t, "no")); code(err) != "permission_denied" {
		t.Fatal("a contributor changed what it added:", err)
	}
	if err := engine.Transact(ctx, contributor, dataset, func(tx store.Tx) error { return tx.Delete(responses, id) }); code(err) != "permission_denied" {
		t.Fatal("a contributor deleted what it added:", err)
	}
	// The owner holds it.
	if stored := storedFields(t, engine, owner, dataset, responses, id); stored["answer"].Payload() != "yes" {
		t.Fatal("the owner does not hold what was contributed")
	}
}

// Inserting needs the schema of the type, and nothing more of the dataset:
// the metadata a contributor reads is the types it adds to or reads.
func TestAContributorReadsTheSchemaItWritesAgainst(t *testing.T) {
	engine, _, _, dataset := contributorEngine(t)
	ctx := context.Background()
	only := store.Access{Principal: "sensor", Authorization: "sensor"}
	if err := engine.SetAuthorization(ctx, dataset, only, store.Authorization{Grants: store.Grants{Insert: true}, View: store.SharedView(nil).WithInserts([]string{responses})}); err != nil {
		t.Fatal(err)
	}
	err := engine.Read(ctx, only, dataset, func(tx store.Tx) error {
		listed, e := tx.ListMetadata()
		if e != nil {
			return e
		}
		if len(listed) != 1 || listed[0].ID != responses {
			t.Fatalf("an insert-only caller was shown %d types", len(listed))
		}
		if _, e = tx.Metadata(questions); code(e) != "unavailable" {
			t.Fatal("an insert-only caller read the schema of a type it cannot reach:", e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := engine.Info(ctx, only, dataset)
	if err != nil || !info.Access.Insert || info.Access.Read || info.Access.Write {
		t.Fatal("an insert-only caller's access is misreported:", err, info.Access)
	}
}

// A transaction is authorized per operation, and an insert grant satisfies an
// insert and nothing else: a contributor's transaction that also checks or
// changes anything is refused whole, at the operation that needed more.
func TestAContributorsTransactionIsOnlyInserts(t *testing.T) {
	engine, _, _, dataset := contributorEngine(t)
	ctx := context.Background()
	only := store.Access{Principal: "sensor", Authorization: "sensor"}
	if err := engine.SetAuthorization(ctx, dataset, only, store.Authorization{Grants: store.Grants{Insert: true}, View: store.SharedView(nil).WithInserts([]string{responses})}); err != nil {
		t.Fatal(err)
	}
	insert := p.Operation{Op: "insert", Type: responses, Fields: map[string]json.RawMessage{"answer": json.RawMessage(`{"type":"text","value":"a"}`)}, As: "first"}
	second := p.Operation{Op: "insert", Type: responses, Fields: map[string]json.RawMessage{"answer": json.RawMessage(`{"type":"text","value":"b"}`)}}
	err := engine.Transact(ctx, only, dataset, func(tx store.Tx) error {
		result, e := store.Execute(tx, p.Transaction{Operations: []p.Operation{insert, second}}, dataset, p.DevelopmentLimits())
		if e == nil && len(result.Results) != 2 {
			t.Fatal("two inserts did not both answer")
		}
		return e
	})
	if err != nil {
		t.Fatal("a contributor's inserts were refused:", err)
	}
	check := p.Operation{Op: "expect_absent", Type: responses, Filter: json.RawMessage(`{"all":[]}`), Guards: []string{"first"}}
	err = engine.Transact(ctx, only, dataset, func(tx store.Tx) error {
		_, e := store.Execute(tx, p.Transaction{Operations: []p.Operation{check, insert}}, dataset, p.DevelopmentLimits())
		return e
	})
	var failure *p.Error
	if code(err) != "permission_denied" || !errors.As(err, &failure) || failure.OperationIndex == nil || *failure.OperationIndex != 0 {
		t.Fatal("a contributor's check was not refused at its index:", err)
	}
}
