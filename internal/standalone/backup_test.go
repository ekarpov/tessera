package standalone

import (
	"bytes"
	"github.com/ekarpov/tessera/internal/acquisition"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"github.com/ekarpov/tessera/pkg/store/sqlite"
)

// A backup is taken while the server is running, restores to what was
// written, and old backups are rotated away.
//
// Before this a backup needed the server stopped: the command that took one
// opened the storage itself and needed the lock the running server holds. So
// the proof that matters is that this happens with the runtime open, and that
// what it wrote restores.
func TestABackupIsTakenWhileTheServerRunsAndRestores(t *testing.T) {
	directory := t.TempDir()
	backups := filepath.Join(directory, "backups")
	if err := os.Mkdir(backups, 0700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(directory, "backup.key")
	key := bytes.Repeat([]byte{7}, 32)
	if err := os.WriteFile(keyFile, key, 0600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(directory, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	config := Config{Issuer: "https://tessera.example", DataDir: data, Engine: "sqlite", EncryptedStorage: true,
		Backup: &BackupConfig{Directory: backups, KeyFile: keyFile, EveryHours: 24, Keep: 2}}
	runtime, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	dataset := sole(t, runtime.Engine)
	owner := store.Access{Principal: "synthetic", Authorization: "backup-owner"}
	if err = runtime.Engine.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	const typeID = "test.backed.up"
	if err = runtime.Engine.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}); e != nil {
			return e
		}
		v, _ := p.NewValue(p.IntegerType, 42)
		_, e := tx.Insert(typeID, map[string]p.Value{"n": v})
		return e
	}); err != nil {
		t.Fatal(err)
	}

	var taken []string
	for i := 0; i < 3; i++ {
		path, err := runtime.BackupNow(t.Context())
		if err != nil {
			t.Fatal("a backup could not be taken while the server runs:", err)
		}
		taken = append(taken, path)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.Acquisitions.SchedulerObservation().Retention.State != "converged" {
		if time.Now().After(deadline) {
			t.Fatal("rotation did not converge")
		}
		time.Sleep(time.Millisecond)
	}
	entries, err := os.ReadDir(filepath.Join(backups, ".acquisitions"))
	if err != nil {
		t.Fatal(err)
	}
	sets := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "set-") {
			sets++
		}
	}
	if sets != 2 {
		t.Fatal("rotation kept", sets, "backups, configured to keep 2")
	}
	if _, err = os.Stat(taken[0]); !os.IsNotExist(err) {
		t.Fatal("rotation removed a newer backup rather than the oldest")
	}

	restored := filepath.Join(directory, "restored")
	if err = os.Mkdir(restored, 0700); err != nil {
		t.Fatal(err)
	}
	if err = sqlite.RestoreBackup(t.Context(), taken[2], filepath.Join(restored, "dataset.db"), "", filepath.Join(data, "dataset.db.retired"), key, restored); err != nil {
		t.Fatal("a backup taken while the server ran does not restore:", err)
	}
	engine, err := sqlite.Open(t.Context(), filepath.Join(restored, "dataset.db"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err = engine.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = engine.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		set, e := tx.Query(typeID, p.Query{}, 10)
		count = len(set.Records)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("the restored backup does not hold what was written:", count)
	}
	if _, err = os.Stat(filepath.Join(restored, "identity.json")); err != nil && !strings.Contains(err.Error(), "no such") {
		t.Fatal(err)
	}
}

func TestAsynchronousPublicationRecordsOnlySuccessfulBackups(t *testing.T) {
	directory := t.TempDir()
	backups := filepath.Join(directory, "backups")
	if err := os.Mkdir(backups, 0700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(directory, "backup.key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{6}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	config := Config{Issuer: "https://tessera.example", DataDir: filepath.Join(directory, "data"), Engine: "sqlite", EncryptedStorage: true, Backup: &BackupConfig{Directory: backups, KeyFile: keyFile, Keep: 2}}
	runtime, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if runtime != nil {
			runtime.Close()
		}
	}()
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err = runtime.Authority.RecordBackup(old); err != nil {
		t.Fatal(err)
	}
	acquire := func(kind string) acquisition.Operation {
		t.Helper()
		id := acquisition.NewID()
		if _, e := runtime.Acquisitions.Start(id, kind); e != nil {
			t.Fatal(e)
		}
		deadline := time.After(5 * time.Second)
		for {
			op, e := runtime.Acquisitions.Status(id)
			if e != nil {
				t.Fatal(e)
			}
			if op.State != "running" {
				return op
			}
			select {
			case <-deadline:
				t.Fatal("acquisition did not complete")
			case <-time.After(time.Millisecond):
			}
		}
	}
	if op := acquire("evidence"); op.State != "complete" {
		t.Fatal(op.State)
	}
	if !runtime.Authority.LastBackup().Equal(old) {
		t.Fatal("evidence-only acquisition changed last_backup_at")
	}
	if op := acquire("backup"); op.State != "complete" {
		t.Fatal(op.State)
	}
	recorded := runtime.Authority.LastBackup()
	if !recorded.After(old) {
		t.Fatal("asynchronous backup did not update last_backup_at")
	}
	// Simulate a crash after durable completion but before the separate overview
	// timestamp write. Reopening must replay publication without another backup.
	if err = runtime.Authority.RecordBackup(old); err != nil {
		t.Fatal(err)
	}
	runtime.Close()
	runtime = nil
	runtime, err = Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.Authority.LastBackup().Equal(recorded) {
		t.Fatal("restart did not repair missed publication recording")
	}
	// Failure after an archive starts must not count as successful publication.
	if err = os.WriteFile(filepath.Join(config.DataDir, "dataset.db.retired"), []byte("malformed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if op := acquire("backup"); op.State != "failed" {
		t.Fatal(op.State)
	}
	if !runtime.Authority.LastBackup().Equal(recorded) {
		t.Fatal("failed acquisition changed last_backup_at")
	}
}
