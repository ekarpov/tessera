package sqlengine

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestEncryptedBackupAndRetirementFence(t *testing.T) {
	s, path, dataset := fixture(t)
	reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody))
	if err != nil {
		t.Fatal(err)
	}
	id := identity(t, reply)
	archive := filepath.Join(t.TempDir(), "backup.enc")
	key := bytes.Repeat([]byte{42}, 32)
	if err = s.Backup(t.Context(), archive, key); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(typeID)) {
		t.Fatal("plaintext escaped encryption")
	}
	destination := filepath.Join(t.TempDir(), "restored.db")
	if err = RestoreBackup(t.Context(), archive, destination, "", path+".retired", key); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(t.Context(), destination, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if record(t, restored, dataset, id).Records[0].Values[0].Payload() != int64(9007199254740993) {
		t.Fatal("backup changed value")
	}
	before, _ := s.Info(t.Context(), owner, sole(t, s))
	after, _ := restored.Info(t.Context(), owner, sole(t, restored))
	if before.ID != after.ID || before.Watermark == after.Watermark {
		t.Fatal("restore identity/generation incorrect")
	}
	if err = s.SetAuthorization(t.Context(), sole(t, s), owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Erase(t.Context(), owner, dataset, dataset); err != nil {
		t.Fatal(err)
	}
	// A backup from before the erasure still restores, and the erasure is
	// applied again on the way in; TestRestoreReappliesErasures has the rest.
	reapplied := filepath.Join(t.TempDir(), "reapplied.db")
	if err = RestoreBackup(t.Context(), archive, reapplied, "", path+".retired", key); err != nil {
		t.Fatal("a backup from before an erasure could not be restored:", err)
	}
	// A ledger that cannot be read refuses the restore: without it there is
	// no telling what must not come back.
	unreadable := filepath.Join(t.TempDir(), "unreadable.retired")
	if err = os.WriteFile(unreadable, []byte("not json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "blocked.db")
	if err = RestoreBackup(t.Context(), archive, blocked, "", unreadable, key); p.ErrorCode(err) != "dataset_unavailable" {
		t.Fatal("a restore went ahead without a readable ledger", err)
	}
	if _, err = os.Stat(blocked); !os.IsNotExist(err) {
		t.Fatal("failed restore left data behind")
	}
	raw[len(raw)-1] ^= 1
	tampered := filepath.Join(t.TempDir(), "bad.enc")
	if err = os.WriteFile(tampered, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = RestoreBackup(t.Context(), tampered, filepath.Join(t.TempDir(), "bad.db"), "", path+".retired", key); p.ErrorCode(err) != "invalid_field" {
		t.Fatal("tampered backup accepted", err)
	}
}

func TestFailedRestorePreservesExistingFence(t *testing.T) {
	s, path, _ := fixture(t)
	archive := filepath.Join(t.TempDir(), "backup.enc")
	key := bytes.Repeat([]byte{9}, 32)
	if err := s.Backup(t.Context(), archive, key); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "new.db")
	ledger := []byte("independent-retirement-evidence\n")
	if err := os.WriteFile(destination+".retired", ledger, 0600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBackup(t.Context(), archive, destination, "", path+".retired", key); p.ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(destination + ".retired")
	if err != nil || !bytes.Equal(retained, ledger) {
		t.Fatal("failed restore deleted existing retirement fence", err)
	}
}

// On a storage holding many people's datasets, one person erasing theirs
// must not make every earlier backup unrestorable. A backup taken before an
// erasure restores everything else, and the erased dataset stays erased: its
// identity and records do not come back, not even as pages in the file.
func TestRestoreReappliesErasures(t *testing.T) {
	s, path, erased := fixture(t)
	const marker = 9007199254740993
	if _, err := s.Mutate(t.Context(), owner, mutation(t, erased, "", insertBody)); err != nil {
		t.Fatal(err)
	}
	made, err := s.CreateDataset(t.Context(), owner, "Kept", p.Limits{}, []p.Meta{{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}}}, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	kept := made.DatasetID
	reply, err := s.Mutate(t.Context(), owner, mutation(t, kept, "", `{"operations":[{"op":"insert","type":"test.record","fields":{"n":{"type":"integer","value":4242}},"as":"entry"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	keptRecord := identity(t, reply)
	archive := filepath.Join(t.TempDir(), "backup.enc")
	key := bytes.Repeat([]byte{7}, 32)
	if err = s.Backup(t.Context(), archive, key); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(t.Context(), erased, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	replaced, err := s.Erase(t.Context(), owner, erased, erased)
	if err != nil {
		t.Fatal(err)
	}
	counted(t, s, replaced.DatasetID)

	destination := filepath.Join(t.TempDir(), "restored.db")
	if err = RestoreBackup(t.Context(), archive, destination, "", path+".retired", key); err != nil {
		t.Fatal("a backup from before an erasure could not be restored:", err)
	}
	restored, err := Open(t.Context(), destination, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	held, err := restored.Provisioned(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range held {
		if d.ID == erased {
			t.Fatal("an erased dataset came back from a backup")
		}
	}
	counted(t, restored, kept)
	if got := record(t, restored, kept, keptRecord).Records[0].Values[0].Payload(); got != int64(4242) {
		t.Fatal("the dataset nobody erased did not come back intact:", got)
	}
	reach, err := restored.Reach(t.Context(), store.Access{Principal: "restore", Authorization: "restore-reapplied-erasure"})
	if err != nil || len(reach) != 0 {
		t.Fatal("the handle that re-applied the erasure still holds something:", reach, err)
	}
	for _, file := range []string{destination, destination + "-wal"} {
		raw, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		// The erased identity itself stays, in the retirement record and as
		// replaced_dataset_id; it names nothing and recovery needs it. Its
		// records must not.
		if bytes.Contains(raw, []byte(strconv.FormatInt(marker, 10))) {
			t.Fatal("an erased record is still in the restored file:", file)
		}
	}
}

// An archive made before a dataset kept its record count and size lacks
// those columns. It still restores, and the counts come from the records it
// holds rather than from nothing.
func TestAnArchiveFromBeforeTheCountsRestoresWithThem(t *testing.T) {
	s, path, dataset := fixture(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody)); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "backup.enc")
	key := bytes.Repeat([]byte{5}, 32)
	if err := s.Backup(t.Context(), archive, key); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(t.TempDir(), "older.enc")
	rewriteArchive(t, archive, older, key, func(m *backupManifest) {
		for i, table := range m.Tables {
			if table.Name != "tessera_dataset" {
				continue
			}
			keep := []int{}
			columns := []string{}
			for j, c := range table.Columns {
				if c != "object_count" && c != "object_bytes" && c != "schema_cache_key" && c != "typed_fields" {
					keep = append(keep, j)
					columns = append(columns, c)
				}
			}
			for r, row := range table.Rows {
				trimmed := []backupValue{}
				for _, j := range keep {
					trimmed = append(trimmed, row[j])
				}
				table.Rows[r] = trimmed
			}
			table.Columns = columns
			m.Tables[i] = table
		}
	})
	destination := filepath.Join(t.TempDir(), "restored.db")
	if err := RestoreBackup(t.Context(), older, destination, "", path+".retired", key); err != nil {
		t.Fatal("an archive from before the counts could not be restored:", err)
	}
	restored, err := Open(t.Context(), destination, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	counted(t, restored, dataset)
	var rows int64
	if err = restored.db.QueryRowContext(t.Context(), `SELECT "object_count" FROM "tessera_dataset" WHERE "dataset_id"=$1`, dataset).Scan(&rows); err != nil || rows != 3 {
		t.Fatal("the restored dataset does not count its records:", rows, err)
	}
}

// rewriteArchive decrypts a backup, lets change edit its manifest, and seals
// the result as another backup under the same key.
func rewriteArchive(t *testing.T, from, to string, key []byte, change func(*backupManifest)) {
	t.Helper()
	aead, err := backupCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("TESSERA-BACKUP-1\n")
	raw = raw[len(prefix):]
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte("TESSERA-BACKUP-1"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for _, entry := range reader.File {
		in, e := entry.Open()
		if e != nil {
			t.Fatal(e)
		}
		content, e := io.ReadAll(in)
		in.Close()
		if e != nil {
			t.Fatal(e)
		}
		if entry.Name == "manifest.json" {
			var manifest backupManifest
			if e = json.Unmarshal(content, &manifest); e != nil {
				t.Fatal(e)
			}
			change(&manifest)
			if content, e = json.Marshal(manifest); e != nil {
				t.Fatal(e)
			}
		}
		target, e := writer.Create(entry.Name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = target.Write(content); e != nil {
			t.Fatal(e)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	sealed := append(append([]byte{}, prefix...), nonce...)
	sealed = aead.Seal(sealed, nonce, out.Bytes(), []byte("TESSERA-BACKUP-1"))
	if err = os.WriteFile(to, sealed, 0600); err != nil {
		t.Fatal(err)
	}
}
