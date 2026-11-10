package sqlengine

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestResolvedReceiptScopesAndWatermarks(t *testing.T) {
	s, _, dataset := fixture(t)
	schema := store.Access{Principal: "schema", Authorization: "schema"}
	if err := s.SetAuthorization(t.Context(), sole(t, s), schema, store.Authorization{Grants: store.Grants{Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	m := mutation(t, dataset, key(81), `{"operations":[{"op":"define_type","meta":{"id":"test.receipt_schema","fields":[]}}]}`)
	reply, err := s.Mutate(t.Context(), schema, m)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(t.Context(), schema, dataset, m.Key)
	if err != nil || status.ResultWithheld || !bytes.Equal(status.Result, reply.Body) || status.Watermark != replyWatermark(reply.Body) {
		t.Fatal("schema replay", err)
	}
	if err = s.Revoke(t.Context(), sole(t, s), schema); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status(t.Context(), schema, dataset, m.Key)
	if err != nil || !status.ResultWithheld || status.Watermark != replyWatermark(reply.Body) {
		t.Fatal("revoked schema body disclosed", err)
	}
	created, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(82), insertBody))
	if err != nil {
		t.Fatal(err)
	}
	var result p.TransactionResult
	if json.Unmarshal(created.Body, &result) != nil {
		t.Fatal("result")
	}
	id := result.Results[0].Objects[0].ID
	deleted := mutation(t, dataset, key(83), `{"operations":[{"op":"delete","target":{"type":"test.record","id":"`+id+`"}}]}`)
	reply, err = s.Mutate(t.Context(), owner, deleted)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Revoke(t.Context(), sole(t, s), owner); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status(t.Context(), owner, dataset, deleted.Key)
	if err != nil || status.ResultWithheld || !bytes.Equal(status.Result, reply.Body) {
		t.Fatal("delete-only body withheld", err)
	}
	status, err = s.Status(t.Context(), owner, dataset, key(82))
	if err != nil || !status.ResultWithheld || len(status.Objects) != 1 || status.Watermark != replyWatermark(created.Body) {
		t.Fatal("created/deleted recovery", err)
	}
}

func TestNoOpCommitKeepsObservedWatermark(t *testing.T) {
	s, _, dataset := fixture(t)
	created, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(84), insertBody))
	if err != nil {
		t.Fatal(err)
	}
	var result p.TransactionResult
	_ = json.Unmarshal(created.Body, &result)
	id := result.Results[0].Objects[0].ID
	update := `{"op":"update","target":{"type":"test.record","id":"` + id + `"},"fields":{"n":{"type":"integer","value":9007199254740993}}}`
	for i, body := range []string{
		`{"operations":[` + update + `]}`,
		`{"operations":[{"op":"update","target":{"type":"test.record","id":"` + id + `"},"fields":{"n":{"type":"integer","value":2}}},` + update + `]}`,
		`{"operations":[{"op":"expect","target":{"type":"test.record","id":"` + id + `"},"fields":{"n":{"type":"integer","value":9007199254740993}}}]}`,
	} {
		reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(85+i), body))
		if err != nil {
			t.Fatal(err)
		}
		info, err := s.Info(t.Context(), owner, sole(t, s))
		if err != nil || replyWatermark(reply.Body) != result.Watermark || info.Watermark != result.Watermark {
			t.Fatal("no-op advanced watermark", err)
		}
	}
}

func TestLegacyReceiptMigrationAndBackupRestore(t *testing.T) {
	s, path, dataset := fixture(t)
	reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(90), insertBody))
	if err != nil {
		t.Fatal(err)
	}
	original := replyWatermark(reply.Body)
	payloads := legacyRecordPayloads(t, s)
	archive := filepath.Join(t.TempDir(), "current.enc")
	backupKey := bytes.Repeat([]byte{31}, 32)
	if err = s.Backup(t.Context(), archive, backupKey); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(t.TempDir(), "legacy.enc")
	rewriteArchive(t, archive, older, backupKey, func(m *backupManifest) {
		for _, table := range m.Tables {
			switch table.Name {
			case "tessera_objects":
				if len(table.Columns) != 4 || table.Columns[3] != "fields" || len(table.Rows) != 1 {
					t.Fatal("backup must pack the record fields into a single archive row")
				}
				row := table.Rows[0]
				if !bytes.Equal(row[3].Bytes, payloads[dataset][*row[1].Text]) {
					t.Fatal("backup payload did not preserve the record fields")
				}
			case "tessera_field_values":
				if len(table.Rows) != 0 {
					t.Fatal("backup repeated fields as expanded SQL rows")
				}
			}
		}
		m.Tables = m.Tables[:len(m.Tables)-3]
		for i := range m.Tables {
			table := &m.Tables[i]
			switch table.Name {
			case "tessera_dataset":
				stripBackupColumn(table, "managed_key")
			case "tessera_receipts":
				for _, name := range []string{"retain_until", "executor_id", "watermark", "replay_scope"} {
					stripBackupColumn(table, name)
				}
			}
		}
	})
	demoteRecordColumns(t, s, payloads)
	for _, statement := range []string{
		`DROP INDEX "tessera_receipts_retention_deadline"`, `ALTER TABLE "tessera_receipts" DROP COLUMN "retain_until"`,
		`DROP INDEX "tessera_receipts_executor"`, `ALTER TABLE "tessera_receipts" DROP COLUMN "executor_id"`,
		`ALTER TABLE "tessera_receipts" DROP COLUMN "watermark"`, `ALTER TABLE "tessera_receipts" DROP COLUMN "replay_scope"`,
		`ALTER TABLE "tessera_transfer_tokens" DROP COLUMN "representation"`, `ALTER TABLE "tessera_sequences" DROP COLUMN "meta"`,
		`DROP INDEX "tessera_imports_uncleaned"`, `ALTER TABLE "tessera_imports" DROP COLUMN "cleaned"`,
		`DROP TABLE "tessera_page_rows"`, `ALTER TABLE "tessera_pages" DROP COLUMN "position"`, `PRAGMA user_version=5`,
	} {
		if _, err = s.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check := func(engine *Store) {
		t.Helper()
		status, e := engine.Status(t.Context(), owner, dataset, key(90))
		if e != nil || status.Watermark != original || !bytes.Equal(status.Result, reply.Body) {
			t.Fatal("legacy receipt changed", e)
		}
	}
	check(s)
	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	if err = RestoreBackup(t.Context(), older, restoredPath, "", path+".retired", backupKey); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(t.Context(), restoredPath, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	check(restored)
}
