// Package postgres provides durable storage in a dedicated PostgreSQL database.
//
// A store here holds **as many datasets as it is given**, keyed by dataset
// within that one database. The dedication is of the database to this engine,
// not of the engine to one dataset. See pkg/store/sqlite for the same note at
// more length.
package postgres

import (
	"context"
	"github.com/ekarpov/tessera/internal/sqlengine"
	p "github.com/ekarpov/tessera/pkg/protocol"
)

type Store = sqlengine.Store

func Open(ctx context.Context, dsn, retirementFencePath string, limits p.Limits) (*Store, error) {
	return sqlengine.OpenPostgres(ctx, dsn, retirementFencePath, limits)
}

func RestoreBackup(ctx context.Context, source, dsn, destinationFence, contentDirectory, currentRetirementFence string, key []byte, hostDirectory ...string) error {
	return sqlengine.RestorePostgres(ctx, source, dsn, destinationFence, contentDirectory, currentRetirementFence, key, hostDirectory...)
}
