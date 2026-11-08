package sqlengine

import (
	"path/filepath"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// A storage quota is the operator's, and optional. With one, a write that
// would take the dataset past it is refused 507 dataset_full, naming the
// quota; without one, size alone never stops a write. The memory bound is a
// different setting with a different reason, and a write that needs the whole
// dataset in memory past it is refused with that reason instead.
func quotaFixture(t *testing.T, limits p.Limits) (*Store, string, func() error) {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "bound.db"), limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	dataset, err := s.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(t.Context(), dataset.ID, owner, store.Authorization{
		Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView(),
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(t.Context(), owner, dataset.ID, func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	fill := func() error {
		return s.Transact(t.Context(), owner, dataset.ID, func(tx store.Tx) error {
			for i := 0; i < 200; i++ {
				v, e := p.NewValue(p.IntegerType, int64(i))
				if e != nil {
					return e
				}
				if _, e = tx.Insert(typeID, map[string]p.Value{"n": v}); e != nil {
					return e
				}
			}
			return nil
		})
	}
	return s, dataset.ID, fill
}

func TestADatasetPastItsQuotaRefusesWritesAndSaysWhy(t *testing.T) {
	limits := p.DevelopmentLimits()
	limits.MaxStoredBytes = 1 << 16
	s, dataset, fill := quotaFixture(t, limits)
	var refusal error
	for attempt := 0; attempt < 40 && refusal == nil; attempt++ {
		refusal = fill()
	}
	if refusal == nil {
		t.Fatal("the dataset never reached its quota, so this test proves nothing")
	}
	if code := p.ErrorCode(refusal); code != "dataset_full" {
		t.Fatalf("a write past the quota answered %q; a caller inserting records made no query", code)
	}
	if p.ErrorStatus("dataset_full") != 507 {
		t.Fatal("dataset_full must not be a 4xx: the request was legal and no smaller one succeeds")
	}
	failure, ok := refusal.(*p.Error)
	if !ok || failure.Details["reason"] != "storage_limit" || failure.Details["maximum"] != limits.MaxStoredBytes {
		t.Fatalf("the refusal does not name the quota it hit: %v", refusal)
	}
	usage, err := s.DatasetUsage(t.Context(), dataset)
	if err != nil || usage.MaxBytes == nil || *usage.MaxBytes != 1<<16 {
		t.Fatal("usage does not report the quota", usage, err)
	}
}

func TestADatasetWithoutAQuotaGrowsPastTheMemoryBound(t *testing.T) {
	limits := p.DevelopmentLimits()
	limits.MaxMaterializedBytes = 1 << 16
	s, dataset, fill := quotaFixture(t, limits)
	for attempt := 0; attempt < 40; attempt++ {
		if err := fill(); err != nil {
			t.Fatal("a dataset with no quota refused a write by size:", err)
		}
	}
	usage, err := s.DatasetUsage(t.Context(), dataset)
	if err != nil || usage.MaxBytes != nil || usage.Bytes <= 1<<16 {
		t.Fatal("usage of a dataset with no quota names a maximum, or the fixture stayed small:", usage, err)
	}
	// A write that needs the whole dataset in memory is the one the memory
	// bound still stops, with its own reason under the same code.
	s.wholeImageWrites = true
	refusal := fill()
	failure, ok := refusal.(*p.Error)
	if p.ErrorCode(refusal) != "dataset_full" || !ok || failure.Details["reason"] != "materialization_limit" ||
		failure.Details["maximum"] != limits.MaxMaterializedBytes {
		t.Fatal("a whole-dataset write past the memory bound did not say so:", refusal)
	}
}
