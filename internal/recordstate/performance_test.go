package recordstate

import (
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// An aggregate scans every row of its type and sorts its groups by name
// afterwards, so it never looks at the order it scanned in. Sorting the scan
// by identity first was a full sort thrown away, and this is where that shows.
func BenchmarkAggregateScan(b *testing.B) {
	const records = 10000
	s, err := New(p.DevelopmentLimits())
	if err != nil {
		b.Fatal(err)
	}
	access := store.Access{Principal: "owner", Authorization: "bench"}
	dataset, err := s.SoleDataset(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	if err = s.EnableReads("aggregation"); err != nil {
		b.Fatal(err)
	}
	if err = s.SetAuthorization(b.Context(), dataset.ID, access,
		store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		b.Fatal(err)
	}
	if err = s.Transact(b.Context(), access, dataset.ID, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: "bench.aggregated", Fields: []p.Field{{ID: "n", MetaID: "bench.aggregated", Type: p.IntegerType}}}); e != nil {
			return e
		}
		for i := 0; i < records; i++ {
			value, e := p.NewValue(p.IntegerType, int64(i))
			if e != nil {
				return e
			}
			if _, e = tx.Insert("bench.aggregated", map[string]p.Value{"n": value}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err = s.Read(b.Context(), access, dataset.ID, func(tx store.Tx) error {
			_, e := tx.Aggregate("bench.aggregated", "n", "", nil)
			return e
		}); err != nil {
			b.Fatal(err)
		}
	}
}
