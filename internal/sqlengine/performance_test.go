package sqlengine

import (
	"fmt"
	"path/filepath"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// BenchmarkRecordOperations measures the cost of a fixed-size operation as the
// containing dataset grows. Setup uses synthetic data and is outside timing.
// It measures the SQLite store boundary, without HTTP or receipt overhead.
func BenchmarkRecordOperations(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("rows=%d", count), func(b *testing.B) {
			s, err := Open(b.Context(), filepath.Join(b.TempDir(), "performance.db"), p.DevelopmentLimits())
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := s.Close(); err != nil {
					b.Error(err)
				}
			})
			dataset, err := s.SoleDataset(b.Context())
			if err != nil {
				b.Fatal(err)
			}
			if err = s.SetAuthorization(b.Context(), dataset.ID, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
				b.Fatal(err)
			}
			err = s.Transact(b.Context(), owner, dataset.ID, func(tx store.Tx) error {
				if _, err := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}); err != nil {
					return err
				}
				for i := 0; i < count; i++ {
					value, err := p.NewValue(p.IntegerType, int64(i))
					if err != nil {
						return err
					}
					if _, err = tx.Insert(typeID, map[string]p.Value{"n": value}); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
			for _, operation := range []string{"object", "metadata", "query10", "queryAll", "update"} {
				b.Run(operation, func(b *testing.B) {
					b.ReportAllocs()
					// A request that changes nothing takes the read path,
					// which is what the server routes it to, so that is what
					// is measured here. Measuring a read through Transact
					// measures a write's preparation for a read's work.
					evaluate := s.Transact
					switch operation {
					case "metadata":
						evaluate = s.ReadSchema
					case "object":
						evaluate = s.ReadRecord
					case "query10", "queryAll":
						evaluate = s.Read
					}
					for i := 0; i < b.N; i++ {
						err := evaluate(b.Context(), owner, dataset.ID, func(tx store.Tx) error {
							switch operation {
							case "object":
								_, err := tx.Object(typeID, "1", nil)
								return err
							case "metadata":
								_, err := tx.Metadata(typeID)
								return err
							case "query10":
								_, err := tx.Query(typeID, p.Query{}, 10)
								return err
							case "queryAll":
								// A limited query builds ten rows whatever
								// the dataset holds, so per-row work does not
								// show in it. This is where it shows.
								_, err := tx.Query(typeID, p.Query{}, count)
								return err
							default:
								value, err := p.NewValue(p.IntegerType, int64(i+count))
								if err != nil {
									return err
								}
								return tx.Update(typeID, "1", map[string]p.Value{"n": value})
							}
						})
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

// Starting a page set writes every page before returning the first cursor, so
// its cost is set by the whole result and by the page size. A page size of
// one over a large result is the shape that shows what that costs.
func BenchmarkBeginPage(b *testing.B) {
	for _, size := range []int{1, 50} {
		b.Run(fmt.Sprintf("page_size=%d", size), func(b *testing.B) {
			const records = 2000
			s, dataset, typeID := pagedFixture(b, records)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				start, err := s.BeginPage(b.Context(), owner, dataset, typeID, p.Query{}, size)
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err = s.EndPage(b.Context(), owner, dataset, start.Cursor); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}

func pagedFixture(b *testing.B, records int) (*Store, string, string) {
	b.Helper()
	s, err := Open(b.Context(), filepath.Join(b.TempDir(), "paging.db"), p.DevelopmentLimits())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if e := s.Close(); e != nil {
			b.Error(e)
		}
	})
	dataset, err := s.SoleDataset(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	if err = s.SetAuthorization(b.Context(), dataset.ID, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		b.Fatal(err)
	}
	const typeID = "bench.paged"
	if err = s.Transact(b.Context(), owner, dataset.ID, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}); e != nil {
			return e
		}
		for i := 0; i < records; i++ {
			value, e := p.NewValue(p.IntegerType, int64(i))
			if e != nil {
				return e
			}
			if _, e = tx.Insert(typeID, map[string]p.Value{"n": value}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	return s, dataset.ID, typeID
}

// Measure encrypted archive export and restore of the same synthetic snapshot.
// Seed writes, temporary path creation and fixture cleanup are outside timing.
func BenchmarkAttributedBackupRestore(b *testing.B) {
	const records = 1000
	s, source, dataset := fixture(b)
	if err := s.Transact(b.Context(), owner, dataset, func(tx store.Tx) error {
		meta := p.Meta{ID: typeID, Fields: []p.Field{
			{ID: "n", MetaID: typeID, Type: p.IntegerType},
			{ID: "created_by", MetaID: typeID, Type: p.TextType, Creator: true},
		}}
		if _, err := tx.UpdateMetadata(meta, ""); err != nil {
			return err
		}
		for i := range records {
			n, _ := p.NewValue(p.IntegerType, int64(i))
			if _, err := tx.Insert(typeID, map[string]p.Value{"n": n}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	backupKey := make([]byte, 32)
	archive := filepath.Join(b.TempDir(), "source.enc")
	if err := s.Backup(b.Context(), archive, backupKey); err != nil {
		b.Fatal(err)
	}
	b.Run("backup", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			destination := filepath.Join(b.TempDir(), "backup.enc")
			b.StartTimer()
			if err := s.Backup(b.Context(), destination, backupKey); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("restore", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			destination := filepath.Join(b.TempDir(), "restored.db")
			b.StartTimer()
			if err := RestoreBackup(b.Context(), archive, destination, "", source+".retired", backupKey); err != nil {
				b.Fatal(err)
			}
		}
	})
}
