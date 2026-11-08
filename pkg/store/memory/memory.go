// Package memory provides a synthetic, non-durable evaluator holding exactly
// one dataset.
//
// The cardinality is the difference that matters when reading this repository:
// this evaluator holds one and says so everywhere, while the durable engines in
// pkg/store/sqlite and pkg/store/postgres hold many. A reader who meets this
// package first, and the reference server's one-dataset deployment second, can
// reasonably conclude the whole system is single-dataset. It is not.
package memory

import (
	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
)

type Store = recordstate.Store

func New(limits p.Limits) (*Store, error) { return recordstate.New(limits) }
