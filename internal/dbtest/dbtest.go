// Package dbtest answers one question for every test that needs a PostgreSQL
// database: is there one, and is going without allowed here?
//
// It exists because of a false green. A release gate reported both engines
// passing while the database had been down for two hours; the cached results
// were half of that and are fixed, and this is the other half. With no DSN
// every PostgreSQL test skips, `go test` prints `ok` for the package, and a
// run that exercised one engine is indistinguishable from a run that
// exercised two. Nothing was wrong with any individual skip — a developer
// without a database should still be able to work — and the composition of
// them is a gate that cannot say what it checked.
//
// So the decision moves to the caller. TESSERA_REQUIRE_POSTGRES turns every
// absent DSN into a failure that names which variable is missing, and the
// release gate sets it. A run that does not set it is a developer run and
// skips as before, loudly enough to read.
package dbtest

import (
	"os"
	"testing"
)

// Required is the variable that makes an absent database a failure rather
// than a skip. Its value is not read: set means required.
const Required = "TESSERA_REQUIRE_POSTGRES"

// Postgres returns the disposable database's DSN, or stops the test. The
// second DSN, for restore, is a separate database because a restore that
// wrote over the source would prove nothing about a restore.
func Postgres(t *testing.T) string { return dsn(t, "TESSERA_TEST_POSTGRES_DSN") }

// PostgresRestore returns the DSN a backup is restored into, or an empty
// string when there is none and this run allows that. It is a separate
// database because a restore that wrote over its source would prove nothing.
//
// It does not skip, because its caller is a branch part-way through a test
// whose earlier assertions have already run, and marking the whole test
// skipped would misreport them. What it does instead is refuse to be silent:
// absence is a failure under Required, and a logged line otherwise.
//
// This one was worse than a skip. The variable was read inline and the branch
// went quiet when it was empty, inside a test that went on to pass — so the
// package printed `ok`, the test printed nothing, and the only evidence that
// a backup restores at all had not run.
func PostgresRestore(t *testing.T) string {
	t.Helper()
	const name = "TESSERA_TEST_POSTGRES_RESTORE_DSN"
	if value := os.Getenv(name); value != "" {
		return value
	}
	if _, required := os.LookupEnv(Required); required {
		t.Fatalf("%s is set and %s is not: backup and restore will not be exercised", Required, name)
	}
	t.Logf("%s is unset: backup and restore are not exercised by this run", name)
	return ""
}

func dsn(t *testing.T, name string) string {
	t.Helper()
	if value := os.Getenv(name); value != "" {
		return value
	}
	if _, required := os.LookupEnv(Required); required {
		t.Fatalf("%s is set and %s is not: this run claims to cover PostgreSQL and cannot", Required, name)
	}
	t.Skipf("requires a disposable PostgreSQL database in %s", name)
	return ""
}
