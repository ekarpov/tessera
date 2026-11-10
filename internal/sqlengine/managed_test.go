package sqlengine

import (
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestManagedLayoutSharesTablesAndStoresNewDescendants(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := t.Context()
	root := p.Meta{ID: "test.managed", Fields: []p.Field{{ID: "number", MetaID: "test.managed", Type: p.IntegerType}}}
	var datasets []string
	for range 2 {
		made, err := s.CreateDataset(ctx, owner, "Synthetic", p.Limits{}, []p.Meta{root}, "", [32]byte{})
		if err != nil {
			t.Fatal(err)
		}
		datasets = append(datasets, made.DatasetID)
		if err = s.InstallManagedTypes(ctx, owner, made.DatasetID, []string{root.ID}); err != nil {
			t.Fatal(err)
		}
	}
	var layouts int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_managed_layouts"`).Scan(&layouts); err != nil || layouts != 1 {
		t.Fatalf("expected a shared layout: %d %v", layouts, err)
	}
	for _, dataset := range datasets {
		if err := s.ReadSchema(ctx, owner, dataset, func(store.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.managed.entries) != 1 {
		t.Fatalf("identical definitions did not share a cached map: %d", len(s.managed.entries))
	}
	value, _ := p.NewValue(p.IntegerType, int64(123))
	custom, _ := p.NewValue(p.TextType, "value")
	dataset := datasets[0]
	child := p.Meta{ID: "test.private_child", ParentID: &root.ID, Fields: []p.Field{{ID: "extra", MetaID: "test.private_child", Type: p.TextType}}}
	if err := s.Transact(ctx, owner, dataset, func(tx store.Tx) error {
		if _, err := tx.CreateMetadata(child); err != nil {
			return err
		}
		_, err := tx.Insert(child.ID, map[string]p.Value{"number": value, "extra": custom})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM "tessera_field_values" WHERE "dataset_id"=$1 AND "field_id"='number'`, dataset).Scan(&count); err != nil || count != 0 {
		t.Fatalf("managed value was duplicated in shared storage: %d %v", count, err)
	}
	if err := s.Read(ctx, owner, dataset, func(tx store.Tx) error {
		q := p.Query{TypeScope: &p.TypeScope{Kind: "descendants"}, Filter: []byte(`{"field":{"id":"number","exact":{"type":"integer","value":123}}}`)}
		records, err := tx.Query(root.ID, q, 10)
		if err == nil && (len(records.Records) != 1 || records.Records[0].MetaID != child.ID) {
			t.Fatal("managed lookup did not include the newly declared descendant")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	columns, err := s.db.QueryContext(ctx, `SELECT "name" FROM pragma_table_info('tessera_objects') WHERE "name"='fields'`)
	if err != nil {
		t.Fatal(err)
	}
	defer columns.Close()
	if columns.Next() {
		t.Fatal("legacy record payload column survived migration")
	}
}
