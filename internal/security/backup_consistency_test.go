package security_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ekarpov/tessera/internal/standalone"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

// An online backup must not pair the registry from one moment with the SQL
// grants from another: restored, that can let an existing token read what the
// registry says it may not. A share is narrowed while the backup is part way
// through reading its files; the restored server's registry and grants must
// agree — here, on the state from before the narrowing, which had to wait.
func TestAnOnlineBackupKeepsTheRegistryAndGrantsTogether(t *testing.T) {
	x := tenants(t)
	for _, typ := range []string{"visible.entry", "hidden.entry"} {
		if w := x.call(t, "POST", "/v1/datasets/"+x.aliceSet+"/meta", x.aliceTok, `{"type":{"id":"`+typ+`","fields":[]}}`, nil); w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if err := x.runtime.Authority.AddPerson("guest", password, x.aliceSet, store.Grants{Read: true}, false); err != nil {
		t.Fatal(err)
	}
	token, err := x.runtime.Authority.AddAutomation(t.Context(), "guest", x.aliceSet)
	if err != nil {
		t.Fatal(err)
	}
	hidden := "/v1/datasets/" + x.aliceSet + "/meta/hidden.entry"
	dir := t.TempDir()
	// A host file the backup reads that blocks until this test lets it: the
	// backup is held part way through, where a change could slip between
	// what it read of the SQL and what it read of the registry.
	gate := filepath.Join(dir, "gate")
	if err = syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	archive := filepath.Join(dir, "backup.enc")
	done := make(chan error, 1)
	go func() {
		done <- x.runtime.Engine.BackupConsistent(t.Context(), archive, key, x.runtime.Authority.Freeze,
			gate, filepath.Join(x.runtime.Config.DataDir, "identity.json"))
	}()
	writer, err := os.OpenFile(gate, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	narrowed := make(chan error, 1)
	go func() {
		narrowed <- x.runtime.Authority.SetReader(t.Context(), "alice", x.aliceSet, "guest", []string{"visible.entry"}, nil, "")
	}()
	time.Sleep(100 * time.Millisecond)
	writer.Close()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = <-narrowed; err != nil {
		t.Fatal(err)
	}
	if got := x.call(t, "GET", hidden, token, "", nil).Code; got < 400 {
		t.Fatal("the live server did not narrow the share:", got)
	}

	restored := t.TempDir()
	if err = os.Chmod(restored, 0700); err != nil {
		t.Fatal(err)
	}
	if err = sqlite.RestoreBackup(t.Context(), archive, filepath.Join(restored, "dataset.db"), filepath.Join(restored, "content"),
		filepath.Join(x.runtime.Config.DataDir, "dataset.db.retired"), key, restored); err != nil {
		t.Fatal(err)
	}
	config := x.runtime.Config
	config.DataDir = restored
	r, err := standalone.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	live := x.runtime
	x.runtime = r
	defer func() { x.runtime = live }()
	shared := false
	for _, reader := range mustReaders(t, r, x.aliceSet) {
		shared = shared || (reader.Account == "guest" && len(reader.Types) > 0)
	}
	readsHidden := x.call(t, "GET", hidden, token, "", nil).Code == 200
	if shared == readsHidden {
		t.Fatal("the restored registry and grants disagree: registry scoped", shared, "while the token reads the hidden type", readsHidden)
	}
}

func mustReaders(t *testing.T, r *standalone.Runtime, dataset string) []identityReader {
	t.Helper()
	readers, err := r.Authority.Readers("alice", dataset)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]identityReader, 0, len(readers))
	for _, reader := range readers {
		out = append(out, identityReader{Account: reader.Account, Types: reader.Types})
	}
	return out
}

type identityReader struct {
	Account string
	Types   []string
}
