package sqlengine

import "testing"

// SQLite admits one writer at a time, and more than one write connection only
// makes them compete through the busy handler, which is not a queue. Under a
// burst of HTTP writes on a slow disk one such connection lost for longer
// than the busy timeout and answered 503 dataset_unavailable to an ordinary
// insert, about once in twenty thousand. The burst needs a slow disk to
// reproduce, so this holds the cause instead: one write connection, for which
// writers queue in order.
func TestSQLiteHasOneWriteConnection(t *testing.T) {
	s, _, _ := fixture(t)
	if s.postgres {
		t.Skip("PostgreSQL writers serialize per dataset by advisory lock")
	}
	if n := s.db.Stats().MaxOpenConnections; n != 1 {
		t.Fatal("SQLite write pool allows", n, "connections")
	}
}
