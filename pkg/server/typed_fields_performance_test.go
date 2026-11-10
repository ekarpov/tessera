package server_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
)

// TestTypedFieldPerformance measures synthetic requests through the v1
// handler in-process. PostgreSQL runs out of process. Each layout gets an
// isolated schema and the same records, including the same indexes where
// applicable. Provisioning and seed time are excluded from the timings.
func TestTypedFieldPerformance(t *testing.T) {
	if os.Getenv("TESSERA_MEASURE_TYPED_FIELDS") != "1" {
		t.Skip("set TESSERA_MEASURE_TYPED_FIELDS=1 for the PostgreSQL comparison")
	}
	const records = 10000
	const runs = 120
	for _, mode := range []string{"json", "typed"} {
		t.Run(mode, func(t *testing.T) {
			engine, admin, schema := openProfilePostgresSchema(t)
			t.Cleanup(func() { engine.Close() })
			handler, err := server.New(engine, func(_ context.Context, token string) (store.Access, error) {
				return profileAccess(token), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			owner := profileAccess("owner")
			made, err := engine.CreateDataset(t.Context(), owner, "Synthetic benchmark", p.Limits{}, nil, "", [32]byte{})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "typed" {
				if err = engine.EnableTypedFields(t.Context(), owner, made.DatasetID); err != nil {
					t.Fatal(err)
				}
			}
			f := profileWire{engine, handler, made.DatasetID}
			f.transact(t, `{"operations":[
              {"op":"define_type","meta":{"id":"bench.root","fields":[
                {"id":"rank","meta_id":"bench.root","type":"integer"},
                {"id":"stamp","meta_id":"bench.root","type":"date_time"}]}},
              {"op":"define_type","meta":{"id":"bench.value","parent_id":"bench.root","fields":[
                {"id":"bucket","meta_id":"bench.value","type":"integer"},
                {"id":"label","meta_id":"bench.value","type":"text"}]}}
            ]}`)
			label := strings.Repeat("synthetic-", 10)
			stamp := benchmarkValue(t, p.DateTimeType, "2026-01-01T10:00:00.001Z")
			text := benchmarkValue(t, p.TextType, label)
			for start := 0; start < records; start += 100 {
				err = engine.Transact(t.Context(), owner, made.DatasetID, func(tx store.Tx) error {
					for i := start; i < min(start+100, records); i++ {
						fields := map[string]p.Value{
							"rank":   benchmarkValue(t, p.IntegerType, int64(i)),
							"bucket": benchmarkValue(t, p.IntegerType, int64(i%10)),
							"label":  text, "stamp": stamp,
						}
						if _, err := tx.Insert("bench.value", fields); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = admin.ExecContext(t.Context(), `ANALYZE "`+schema+`"."tessera_objects", "`+schema+`"."tessera_field_values"`); err != nil {
				t.Fatal(err)
			}
			var objectBytes, valueBytes int64
			if err = admin.QueryRowContext(t.Context(), `SELECT
			  pg_total_relation_size(format('%I.tessera_objects',$1::text)::regclass),
			  pg_total_relation_size(format('%I.tessera_field_values',$1::text)::regclass)`, schema).Scan(&objectBytes, &valueBytes); err != nil {
				t.Fatal(err)
			}
			t.Logf("records=%d record_bytes=%d field_bytes=%d total_bytes=%d", records, objectBytes, valueBytes, objectBytes+valueBytes)
			for _, workload := range []timedWorkload{
				{"point_read", func(i int) {
					f.call(t, "GET", "/objects/bench.value/"+strconv.Itoa(records-i%runs), "owner", "", 200)
				}},
				{"selective_range", func(i int) {
					n := records - 20 - i%runs
					body := fmt.Sprintf(`{"query":{"filter":{"field":{"id":"rank","range":{"lower":{"type":"integer","value":%d},"upper":{"type":"integer","value":%d}}}}},"limit":10}`, n, n+9)
					f.call(t, "POST", "/objects/bench.value:query", "owner", body, 200)
				}},
				{"two_field_filter", func(i int) {
					n := records - 20 - i%runs
					body := fmt.Sprintf(`{"query":{"filter":{"and":[{"field":{"id":"rank","range":{"lower":{"type":"integer","value":%d}}}},{"field":{"id":"bucket","exact":{"type":"integer","value":1}}}]}},"limit":10}`, n)
					f.call(t, "POST", "/objects/bench.value:query", "owner", body, 200)
				}},
				{"patch_one_field", func(i int) {
					f.call(t, "PATCH", "/objects/bench.value/"+strconv.Itoa(i+1), "owner",
						`{"fields":{"bucket":{"type":"integer","value":11}}}`, 200)
				}},
				{"insert_four_fields", func(i int) {
					body := fmt.Sprintf(`{"fields":{"rank":{"type":"integer","value":%d},"bucket":{"type":"integer","value":12},
					  "label":{"type":"text","value":%q},"stamp":{"type":"date_time","value":"2026-01-01T10:00:00.001Z"}}}`, records+i, label)
					f.call(t, "POST", "/objects/bench.value", "owner", body, 201)
				}},
			} {
				timeWorkload(t, workload, runs)
			}
			benchmarkCompatibilityQueries(t, f)

		})
	}
}

type timedWorkload struct {
	name string
	run  func(int)
}

func timeWorkload(t *testing.T, workload timedWorkload, runs int) {
	t.Helper()
	// Write warmups use separate positions so measured patches still change
	// a value.
	for i := runs; i < runs+10; i++ {
		workload.run(i)
	}
	samples := make([]time.Duration, runs)
	for i := range samples {
		start := time.Now()
		workload.run(i)
		samples[i] = time.Since(start)
	}
	slices.Sort(samples)
	t.Logf("%s p50=%s p95=%s n=%d", workload.name, samples[runs/2], samples[runs*95/100], runs)
}

func benchmarkCompatibilityQueries(t *testing.T, f profileWire) {
	const samples = 30
	// Additional fixtures are seeded after the storage measurement above.
	result := f.transact(t, `{"operations":[
      {"op":"define_type","meta":{"id":"bench.decimal","fields":[
        {"id":"amount","meta_id":"bench.decimal","type":"fixed","scale":2},
        {"id":"bucket","meta_id":"bench.decimal","type":"text"}]}},
      {"op":"define_type","meta":{"id":"bench.parent","fields":[]}},
      {"op":"define_type","meta":{"id":"bench.linked","fields":[
        {"id":"link","meta_id":"bench.linked","type":"reference"},
        {"id":"rank","meta_id":"bench.linked","type":"integer"}]}},
      {"op":"insert","type":"bench.parent","fields":{}},
      {"op":"insert","type":"bench.parent","fields":{}}
    ]}`)
	parent := result.Results[3].Objects[0].ID
	other := result.Results[4].Objects[0].ID
	for start := 0; start < 1000; start += 100 {
		err := f.engine.Transact(t.Context(), profileAccess("owner"), f.dataset, func(tx store.Tx) error {
			for i := start; i < start+100; i++ {
				if _, err := tx.Insert("bench.decimal", map[string]p.Value{
					"amount": benchmarkValue(t, p.FixedType, "9007199254740993.01"),
					"bucket": benchmarkValue(t, p.TextType, strconv.Itoa(i%10)),
				}); err != nil {
					return err
				}
				id := other
				if i%10 == 0 {
					id = parent
				}
				if _, err := tx.Insert("bench.linked", map[string]p.Value{
					"link": benchmarkValue(t, p.ReferenceType, p.Reference{DatasetID: f.dataset, ObjectID: id, MetaID: "bench.parent"}),
					"rank": benchmarkValue(t, p.IntegerType, int64(i)),
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	f.grant(t, "guest", store.Grants{Read: true}, store.SharedView([]string{"bench.value"}))
	for _, workload := range []timedWorkload{
		{"inherited_top_ten", func(int) {
			f.call(t, "POST", "/objects/bench.root:query", "owner",
				`{"query":{"type_scope":{"kind":"descendants"},"result_sort":[{"field_id":"stamp","ascending":false}]},"limit":10}`, 200)
		}},
		{"exact_decimal_groups", func(int) {
			f.call(t, "POST", "/objects/bench.decimal:aggregate", "owner", `{"field":"amount","group_by":"bucket"}`, 200)
		}},
		{"reference_page", func(int) {
			body := fmt.Sprintf(`{"query":{"filter":{"field":{"id":"link","exact":{"type":"reference","value":{"dataset_id":%q,"meta_id":"bench.parent","object_id":%q}}}},"result_sort":[{"field_id":"rank","ascending":true}]},"page_size":10}`, f.dataset, parent)
			cursor := decode[store.PageStart](t, f.call(t, "POST", "/objects/bench.linked:page", "owner", body, 200)).Cursor
			f.call(t, "POST", "/pages/"+cursor+":next", "owner", "", 200)
			f.call(t, "DELETE", "/pages/"+cursor, "owner", "", 204)
		}},
		{"scoped_integer_query", func(int) {
			f.call(t, "POST", "/objects/bench.value:query", "guest",
				`{"query":{"filter":{"field":{"id":"rank","exact":{"type":"integer","value":9975}}}},"limit":10}`, 200)
		}},
	} {
		timeWorkload(t, workload, samples)
	}
}

func benchmarkValue(t *testing.T, kind p.Type, payload any) p.Value {
	t.Helper()
	value, err := p.NewValue(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
