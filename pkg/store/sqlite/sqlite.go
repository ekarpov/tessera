// Package sqlite provides durable SQLite storage.
//
// A store here holds **as many datasets as it is given**, in one file, keyed by
// dataset. Create them with CreateDataset, list what a caller may use with
// Datasets, and address each by identity. "Single-process" describes the writer,
// not the contents: one process at a time serves a file, and that process can
// serve any number of datasets in it.
//
// Do not read the single-dataset behaviour of the reference server as this
// package's limit. That is a deployment choice made by cmd/tesserad, and the
// in-memory evaluator in pkg/store/memory really does hold exactly one — which
// makes the two easy to confuse in the wrong direction.
package sqlite

import (
	"context"
	"github.com/ekarpov/tessera/internal/sqlengine"
	p "github.com/ekarpov/tessera/pkg/protocol"
)

type Store = sqlengine.Store

func Open(ctx context.Context, path string, limits p.Limits) (*Store, error) {
	return sqlengine.Open(ctx, path, limits)
}

func RestoreBackup(ctx context.Context, source, destination, contentDirectory, retirementFence string, key []byte, hostDirectory ...string) error {
	return sqlengine.RestoreBackup(ctx, source, destination, contentDirectory, retirementFence, key, hostDirectory...)
}
