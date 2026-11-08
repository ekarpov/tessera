package recordstate

import (
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// An evaluator built without its records must refuse to scan rather than
// answer from the empty image it holds.
//
// Before streaming existed this was possible and was prevented only by the
// caller's wrapper refusing to scan: built for a point read with no records
// and handed a query, the evaluator ranged an empty map and reported that
// nothing matched. That is a narrowed result that looks exactly like a
// complete one — the failure the protocol forbids everywhere else — and it was
// one wrapper away from reaching a client. The general read path now builds
// partial images routinely, so the protection moves into the evaluator.
func TestAPartialImageWithoutAScanSourceRefusesToScan(t *testing.T) {
	const typeID = "test.partial"
	owner := store.Access{Principal: "synthetic", Authorization: "owner"}
	image := Image{
		ID:        "d-partial",
		Watermark: "w:partial",
		Next:      1,
		Types:     map[string]p.Meta{typeID: {ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}},
		Objects:   map[string]Row{},
	}
	grant := store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}
	s, err := Adopt(image, p.DevelopmentLimits(), owner, grant, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Built for point reads: a source for one record by identity, and none
	// for scanning. Every record the dataset holds is out of reach of a scan.
	s.SetObjectSource(func(string) (Row, bool, error) { return Row{}, false, nil })

	err = s.Evaluate(t.Context(), owner, image.ID, func(tx store.Tx) error {
		set, e := tx.Query(typeID, p.Query{}, 10)
		if e == nil {
			t.Fatalf("a partial image answered a scan with %d records as if it were complete", len(set.Records))
		}
		return e
	})
	if p.ErrorCode(err) != "internal_error" {
		t.Fatal("a partial image did not refuse to scan:", err)
	}
}
