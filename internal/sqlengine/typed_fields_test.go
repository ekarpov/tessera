package sqlengine

import (
	"bytes"
	"path/filepath"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestTypedFieldsBackupAndWireValues(t *testing.T) {
	s, path, dataset := fixture(t)
	ctx := t.Context()
	const typeID = "test.typed_fields"
	fields := map[string]p.Value{}
	declared := []p.Field{}
	for _, sample := range []struct {
		id   string
		kind p.Type
		data any
	}{
		{"label", p.TextType, "one\x00two"},
		{"empty", p.TextType, ""},
		{"number", p.IntegerType, int64(9007199254740993)},
		{"ratio", p.FloatType, 1.0000000000000002},
		{"amount", p.FixedType, "9007199254740993.01"},
		{"enabled", p.BooleanType, true},
		{"stamp", p.DateTimeType, "2026-01-01T00:00:00.123Z"},
		{"opaque", p.JSONType, `{ "n": 1.00 }`},
		{"bytes", p.BlobType, "AAE="},
		{"link", p.ReferenceType, p.Reference{DatasetID: dataset, MetaID: typeID, ObjectID: "same"}},
	} {
		value, err := p.NewValue(sample.kind, sample.data)
		if err != nil {
			t.Fatal(err)
		}
		fields[sample.id] = value
		field := p.Field{ID: sample.id, MetaID: typeID, Type: sample.kind}
		if sample.kind == p.FixedType {
			scale := 2
			field.Scale = &scale
		}
		declared = append(declared, field)
	}
	var object string
	if err := s.Transact(ctx, owner, dataset, func(tx store.Tx) error {
		if _, err := tx.CreateMetadata(p.Meta{ID: typeID, Fields: declared}); err != nil {
			return err
		}
		set, err := tx.Insert(typeID, fields)
		if err == nil {
			object = set.Records[0].ID.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check := func(engine *Store) {
		t.Helper()
		var count int
		if err := engine.db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_field_values"
            WHERE "dataset_id"=$1 AND "object_id"=$2`, dataset, object).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != len(fields) {
			t.Fatalf("typed record has %d fields, want %d", count, len(fields))
		}
		if err := engine.ReadRecord(ctx, owner, dataset, func(tx store.Tx) error {
			set, err := tx.Object(typeID, object, nil)
			if err != nil {
				return err
			}
			for i, field := range set.Meta.Fields {
				got, err := p.MarshalWire(set.Records[0].Values[i])
				if err != nil {
					return err
				}
				want, err := p.MarshalWire(fields[field.ID])
				if err != nil {
					return err
				}
				if !bytes.Equal(got, want) {
					t.Errorf("field %s changed during typed round trip", field.ID)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(s)
	archive := filepath.Join(t.TempDir(), "typed.enc")
	key := bytes.Repeat([]byte{4}, 32)
	if err := s.Backup(ctx, archive, key); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored.db")
	if err := RestoreBackup(ctx, archive, destination, "", path+".retired", key); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, destination, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	check(restored)
	if err = restored.Transact(ctx, owner, dataset, func(tx store.Tx) error { return tx.Delete(typeID, object) }); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = restored.db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_field_values" WHERE "dataset_id"=$1 AND "object_id"=$2`, dataset, object).Scan(&count); err != nil || count != 0 {
		t.Fatalf("typed field rows survived deletion: %d %v", count, err)
	}
}

func TestManagedProvisioningRefusesExistingRecordsAndGuests(t *testing.T) {
	s, _, dataset := fixture(t)
	ctx := t.Context()
	guest := store.Access{Principal: "guest", Authorization: "reader"}
	if err := s.SetAuthorization(ctx, dataset, guest, store.Authorization{
		Grants: store.Grants{Read: true}, View: store.FullView(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallManagedTypes(ctx, guest, dataset, []string{typeID}); p.ErrorCode(err) != "permission_denied" {
		t.Fatalf("guest changed the storage layout: %v", err)
	}
	if _, err := s.Mutate(ctx, owner, mutation(t, dataset, "", insertBody)); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallManagedTypes(ctx, owner, dataset, []string{typeID}); p.ErrorCode(err) != "conflict" {
		t.Fatalf("populated dataset accepted an in-place switch: %v", err)
	}
}
