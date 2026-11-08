package sqlengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// A configured default reaches datasets created afterwards, including on a
// database that already exists.
//
// It used not to. New datasets took the widest limits any stored dataset
// declared, so the limits an engine was opened with reached only the first
// dataset of a brand-new database — every later configuration change was
// read and ignored.
func TestConfiguredLimitsReachDatasetsCreatedLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits.db")
	first, err := Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	configured := p.DevelopmentLimits()
	configured.MaxMaterializedBytes = 256 << 20
	s, err := Open(t.Context(), path, configured)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	made, err := s.CreateDataset(t.Context(), owner, "Later", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.DatasetLimits(t.Context(), made.DatasetID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MaxMaterializedBytes != 256<<20 {
		t.Fatal("a dataset created on an existing database ignored the configured limits:", stored.MaxMaterializedBytes)
	}
}

// Raising the bound a full dataset hit is what makes dataset_full an answer an
// operator can act on. And the retention window only grows, because clients
// were promised it.
func TestRaisingAFullDatasetsBoundLetsItAcceptWritesAgain(t *testing.T) {
	limits := p.DevelopmentLimits()
	limits.MaxStoredBytes = 1 << 14
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "full.db"), limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	dataset, err := s.SoleDataset(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(t.Context(), dataset.ID, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(t.Context(), owner, dataset.ID, func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	write := func() error {
		return s.Transact(t.Context(), owner, dataset.ID, func(tx store.Tx) error {
			for i := 0; i < 50; i++ {
				v, _ := p.NewValue(p.IntegerType, int64(i))
				if _, e := tx.Insert(typeID, map[string]p.Value{"n": v}); e != nil {
					return e
				}
			}
			return nil
		})
	}
	var full error
	for i := 0; i < 40 && full == nil; i++ {
		full = write()
	}
	if p.ErrorCode(full) != "dataset_full" {
		t.Fatal("the fixture never filled the dataset, so this proves nothing:", full)
	}
	usage, err := s.DatasetUsage(t.Context(), dataset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Records == 0 || usage.Bytes == 0 || usage.MaxBytes == nil || *usage.MaxBytes != 1<<14 {
		t.Fatal("usage does not describe a full dataset:", usage)
	}

	current, err := s.DatasetLimits(t.Context(), dataset.ID)
	if err != nil {
		t.Fatal(err)
	}
	shorter, err := p.MergeLimits(current, json.RawMessage(`{"idempotency_retention_seconds":86400}`))
	if err != nil {
		t.Fatal(err)
	}
	if current.IdempotencyRetentionSeconds > 86400 {
		if err = s.SetDatasetLimits(t.Context(), dataset.ID, shorter); p.ErrorCode(err) != "conflict" {
			t.Fatal("a promised retention window was shortened:", err)
		}
	}
	raised, err := p.MergeLimits(current, json.RawMessage(`{"max_stored_bytes":16777216}`))
	if err != nil {
		t.Fatal(err)
	}
	if raised.MaxScannedRows != current.MaxScannedRows || raised.IdempotencyRetentionSeconds != current.IdempotencyRetentionSeconds {
		t.Fatal("raising one limit changed others it did not name")
	}
	if err = s.SetDatasetLimits(t.Context(), dataset.ID, raised); err != nil {
		t.Fatal(err)
	}
	if err = write(); err != nil {
		t.Fatal("a dataset whose bound was raised still refuses writes:", err)
	}
	// Zero removes the quota: no maximum reported, and a negative one refused.
	none, err := p.MergeLimits(raised, json.RawMessage(`{"max_stored_bytes":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetDatasetLimits(t.Context(), dataset.ID, none); err != nil {
		t.Fatal(err)
	}
	if usage, err = s.DatasetUsage(t.Context(), dataset.ID); err != nil || usage.MaxBytes != nil {
		t.Fatal("a dataset whose quota was removed still reports one:", usage, err)
	}
	if negative, err := p.MergeLimits(raised, json.RawMessage(`{"max_stored_bytes":-1}`)); err == nil && negative.Validate() == nil {
		t.Fatal("a negative quota was accepted")
	}
}

// A partial override cannot zero what it leaves out, and cannot name a limit
// that does not exist.
func TestMergingLimitsKeepsWhatItDoesNotName(t *testing.T) {
	base := p.DevelopmentLimits()
	merged, err := p.MergeLimits(base, json.RawMessage(`{"max_page_size":50}`))
	if err != nil {
		t.Fatal(err)
	}
	if merged.MaxPageSize != 50 || merged.MaxMaterializedBytes != base.MaxMaterializedBytes || merged.MaxFixedDigits != base.MaxFixedDigits {
		t.Fatal("merging changed members it did not name:", merged)
	}
	if _, err = p.MergeLimits(base, json.RawMessage(`{"max_invented":1}`)); err == nil {
		t.Fatal("an unknown limit was accepted")
	}
	if _, err = p.MergeLimits(base, json.RawMessage(`{"max_page_size":0}`)); err == nil {
		t.Fatal("a bound of nothing was accepted")
	}
}

// Raising a dataset's max_request_bytes takes effect for bodies read before
// their dataset is known at once, not at the next restart.
func TestARaisedRequestBoundAppliesWithoutARestart(t *testing.T) {
	s, _, dataset := fixture(t)
	limits, err := s.DatasetLimits(t.Context(), dataset)
	if err != nil {
		t.Fatal(err)
	}
	raised := limits.MaxRequestBytes * 2
	limits.MaxRequestBytes = raised
	if err = s.SetDatasetLimits(t.Context(), dataset, limits); err != nil {
		t.Fatal(err)
	}
	if got := s.RecoveryLimits().MaxRequestBytes; got != raised {
		t.Fatal("a raised request bound waited for a restart:", got, "want", raised)
	}
}

// The storage's request bound follows every dataset, including one created
// with a wider bound than any before it and ones changed at the same moment:
// whichever change publishes last, the bound is the widest committed.
func TestTheRequestBoundIsTheWidestCommittedWhateverTheOrder(t *testing.T) {
	engines := map[string]func(*testing.T) *Store{"sqlite": func(t *testing.T) *Store { s, _, _ := fixture(t); return s }}
	if os.Getenv("TESSERA_TEST_POSTGRES_DSN") != "" {
		engines["postgres"] = func(t *testing.T) *Store { postgresAdmin(t); return postgresFixture(t) }
	}
	for name, open := range engines {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			base := s.RecoveryLimits()
			wider := p.DevelopmentLimits()
			wider.MaxRequestBytes = base.MaxRequestBytes * 2
			made, err := s.CreateDataset(t.Context(), owner, "Wider", wider, nil, "", [32]byte{})
			if err != nil {
				t.Fatal(err)
			}
			if got := s.RecoveryLimits().MaxRequestBytes; got != wider.MaxRequestBytes {
				t.Fatal("a dataset created with a wider request bound did not widen the storage's:", got)
			}
			// Several datasets raised at once, each to its own width.
			datasets := []string{made.DatasetID}
			for i := 0; i < 5; i++ {
				more, e := s.CreateDataset(t.Context(), owner, "More", p.DevelopmentLimits(), nil, "", [32]byte{})
				if e != nil {
					t.Fatal(e)
				}
				datasets = append(datasets, more.DatasetID)
			}
			widest := 0
			var group sync.WaitGroup
			for i, dataset := range datasets {
				limits, e := s.DatasetLimits(t.Context(), dataset)
				if e != nil {
					t.Fatal(e)
				}
				limits.MaxRequestBytes = base.MaxRequestBytes * (3 + i)
				widest = max(widest, limits.MaxRequestBytes)
				group.Add(1)
				go func() {
					defer group.Done()
					if e := s.SetDatasetLimits(context.Background(), dataset, limits); e != nil {
						t.Error(e)
					}
				}()
			}
			group.Wait()
			if got := s.RecoveryLimits().MaxRequestBytes; got != widest {
				t.Fatal("after concurrent changes the storage's request bound is", got, "not the widest committed,", widest)
			}
		})
	}
}
