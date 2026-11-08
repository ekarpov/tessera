package docsnippets_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	protocol "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/server"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

// The snippets in docs/consuming-tessera.md are the first code an embedding
// host copies, so they are compiled here rather than proofread. A
// documented example that no longer builds is worse than no example: it costs
// the reader the time to discover that the problem was never theirs.
//
// This never runs. Compiling it is the test.
func TestDocumentedEmbeddingSnippetsCompile(t *testing.T) {
	t.Skip("compiling this file is the check; running it would need a database")
	var ctx context.Context
	var path, issuer string
	var authenticate server.Authenticate
	var access store.Access
	var mutation store.Mutation

	engine, err := sqlite.Open(ctx, path, protocol.DevelopmentLimits())
	_ = err
	handler, err := server.NewWithOptions(engine, authenticate, server.Options{
		AuthMethods:          []string{"oauth2"},
		AuthorizationServers: []string{issuer},
	})
	var _ http.Handler = handler

	outcome, err := engine.MutateWith(ctx, access, mutation, func(core store.Tx, host *sql.Tx) error {
		_, e := host.ExecContext(ctx, `INSERT INTO "exchange_receipts" ("id") VALUES (1)`)
		return e
	})
	_, _ = outcome, err
}
