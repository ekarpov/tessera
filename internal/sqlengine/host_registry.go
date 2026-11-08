package sqlengine

import (
	"context"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

// A fixed database-scoped key, separate from executor and dataset lock spaces.
const hostRegistryKey int64 = 0x7465737368726567

// RegisterHostRegistry marks a reference-server file registry owner for its
// lifetime. Call during composition, before any registry/grant reconciliation.
// Multiple owners may serve, but none can capture all their independent files.
func (s *Store) RegisterHostRegistry(ctx context.Context) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	if !s.postgres {
		return nil
	}
	var held bool
	if err := s.pgOwner.QueryRowContext(ctx, `SELECT pg_try_advisory_lock_shared($1)`, hostRegistryKey).Scan(&held); err != nil {
		return dbError(ctx, err)
	}
	if !held {
		return store.Refuse("dataset_unavailable")
	}
	s.hostRegistry = true
	return nil
}

// pinHostRegistry excludes other registered file-registry owners for S only.
// SQL-only embedding processes remain supported; their host must ensure grant
// changes go through the same authority as any host files included in a backup.
func (s *Store) pinHostRegistry(ctx context.Context) (func(), error) {
	if !s.postgres || !s.hostRegistry {
		return func() {}, nil
	}
	var alone bool
	if err := s.pgOwner.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, hostRegistryKey).Scan(&alone); err != nil {
		return nil, dbError(ctx, err)
	}
	if !alone {
		return nil, store.Refuse("not_supported")
	}
	return func() {
		// Unlock even when export's context was cancelled. If the owner connection
		// was lost, enter already refuses further work on this store.
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = s.pgOwner.ExecContext(cleanup, `SELECT pg_advisory_unlock($1)`, hostRegistryKey)
	}, nil
}
