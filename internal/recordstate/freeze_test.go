package recordstate

import (
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// A missing record inside the frozen view is absence. A missing record at
// the cut, or an identity with no minted sequence, must still be concealed.
func TestFrozenLookupMissingMintedSequence(t *testing.T) {
	for _, sourced := range []bool{false, true} {
		engine := &Store{}
		if sourced {
			engine.source = func(string) (Row, bool, error) { return Row{}, false, nil }
		}
		tx := &transaction{engine: engine, state: state{types: map[string]p.Meta{"test.record": {ID: "test.record"}}, objects: map[string]object{}}, auth: store.Authorization{View: store.FullView().BeforeSequence(12)}}
		for _, c := range []struct{ id, want string }{{"2", "not_found"}, {"11", "not_found"}, {"12", "unavailable"}, {"20", "unavailable"}, {"missing", "unavailable"}, {"18446744073709551616", "unavailable"}, {"0", "unavailable"}} {
			_, err := tx.lookup("test.record", c.id)
			if got := p.ErrorCode(err); got != c.want {
				t.Errorf("sourced=%v id=%s: got %s, want %s", sourced, c.id, got, c.want)
			}
		}
	}
}
