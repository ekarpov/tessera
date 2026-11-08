package sqlengine

import (
	"bytes"
	"database/sql"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"os"
	"path/filepath"
	"testing"
)

func TestEraseReplacementRecoveryAndRestoreFence(t *testing.T) {
	s, path, dataset := fixture(t)
	if _, err := s.Erase(t.Context(), owner, dataset, dataset); p.ErrorCode(err) != "permission_denied" {
		t.Fatal(err)
	}
	auth := store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}
	if err := s.SetAuthorization(t.Context(), sole(t, s), owner, auth); err != nil {
		t.Fatal(err)
	}
	other := store.Access{Principal: "other", Authorization: "other"}
	if err := s.SetAuthorization(t.Context(), sole(t, s), other, auth); err != nil {
		t.Fatal(err)
	}
	reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, key(500), insertBody))
	if err != nil {
		t.Fatal(err)
	}
	id := identity(t, reply)
	preview, err := s.PreviewErase(t.Context(), owner, dataset)
	if err != nil || preview.ObjectCount != 1 || preview.TypeCount != 1 || preview.AffectedPrincipals != 1 {
		t.Fatal(preview, err)
	}
	if _, err = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Erase(t.Context(), owner, dataset, "wrong"); p.ErrorCode(err) != "invalid_field" {
		t.Fatal(err)
	}
	result, err := s.Erase(t.Context(), owner, dataset, dataset)
	if err != nil {
		t.Fatal(err)
	}
	if result.DatasetID == dataset || result.Watermark == preview.Watermark {
		t.Fatal("generation reused")
	}
	// An erase provisions only the erasing authorization, so the other caller
	// holds nothing on the replacement and is told unavailable rather than
	// permission_denied — it may not learn the replacement exists.
	if _, err = s.Info(t.Context(), other, sole(t, s)); p.ErrorCode(err) != "unavailable" {
		t.Fatal("grants transferred", err)
	}
	info, err := s.Info(t.Context(), owner, sole(t, s))
	if err != nil || info.ReplacedDatasetID != dataset {
		t.Fatal(info, err)
	}
	if _, err = s.Erase(t.Context(), owner, dataset, dataset); p.ErrorCode(err) != "not_found" {
		t.Fatal("retry erased replacement", err)
	}
	receipt, err := s.Status(t.Context(), owner, dataset, key(500))
	if err != nil || !receipt.ResultWithheld || len(receipt.Objects) != 1 || receipt.Objects[0].ObjectID != id {
		t.Fatal(receipt, err)
	}
	if err = s.CheckpointCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	info, err = s.Info(t.Context(), owner, sole(t, s))
	if err != nil || info.ID != result.DatasetID {
		t.Fatal(info, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// A pre-erase database cannot be served while the independent retirement
	// ledger is retained, even if an operator replaces the database file.
	if err = os.WriteFile(path, backup, 0600); err != nil {
		t.Fatal(err)
	}
	if restored, e := Open(t.Context(), path, p.DevelopmentLimits()); e == nil {
		restored.Close()
		t.Fatal("restore resurrected erased dataset")
	}
}

// Migrations are forward-only, so an operator who rolls a binary back after an
// upgrade meets a database it does not understand. That must be a clean
// refusal at startup rather than a binary operating on a schema it cannot
// read: the damage from guessing is silent and the refusal is not.
func TestAnOlderBinaryRefusesANewerDatabase(t *testing.T) {
	s, path, _ := fixture(t)
	if _, err := s.db.ExecContext(t.Context(), `PRAGMA user_version=999`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), path, p.DevelopmentLimits())
	if err == nil {
		reopened.Close()
		t.Fatal("a database from a newer version was opened anyway")
	}
	if p.ErrorCode(err) != "not_supported" {
		t.Fatal("the refusal did not name the cause", p.ErrorCode(err), err)
	}
	// The database is untouched, so the operator can roll forward again rather
	// than restore from backup.
	direct, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	var version int
	if err = direct.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 999 {
		t.Fatal("a refused open rewrote the schema version", version)
	}
}

// A retirement another process journaled into its own ledger still reaches
// this one. Each process keeps a ledger and they share one database, so a
// flag in the database saying "journaled" cannot say which ledgers have it; a
// ledger that trusted it missed every erasure another process made, and a
// restore given that ledger resurrected the dataset.
func TestEveryLedgerLearnsEveryRetirement(t *testing.T) {
	s, path, _ := fixture(t)
	// As another process leaves it: retired, and flagged as journaled there.
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO "tessera_retirements" ("dataset_id","replacement_id","retired_at","journaled") VALUES ('s-erased-elsewhere','s-its-replacement',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := s.journalRetirements(t.Context()); err != nil {
		t.Fatal(err)
	}
	ledger, err := os.ReadFile(path + ".retired")
	if err != nil || !bytes.Contains(ledger, []byte(`"s-erased-elsewhere"`)) {
		t.Fatal("this process's ledger missed a retirement another process journaled:", string(ledger), err)
	}
	// And journaling again adds nothing twice.
	if err = s.journalRetirements(t.Context()); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path + ".retired")
	if bytes.Count(again, []byte(`"s-erased-elsewhere"`)) != 1 {
		t.Fatal("a retirement was written to the ledger twice")
	}
}

// Two processes on one PostgreSQL database, each with its own ledger: an
// erasure by one reaches the other's ledger at its next pass.
func TestTwoProcessesLedgersBothLearnAnErasure(t *testing.T) {
	if os.Getenv("TESSERA_TEST_POSTGRES_DSN") == "" {
		t.Skip("needs PostgreSQL")
	}
	postgresAdmin(t)
	first := postgresFixture(t)
	dataset := sole(t, first)
	secondFence := filepath.Join(t.TempDir(), "second-fence")
	second, err := openPostgres(t.Context(), os.Getenv("TESSERA_TEST_POSTGRES_DSN"), secondFence, p.DevelopmentLimits(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	eraser := store.Access{Principal: "p", Authorization: "a"}
	if err = first.SetAuthorization(t.Context(), dataset, eraser, store.Authorization{Grants: store.Grants{Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	if _, err = first.Erase(t.Context(), eraser, dataset, dataset); err != nil {
		t.Fatal(err)
	}
	if err = second.journalRetirements(t.Context()); err != nil {
		t.Fatal(err)
	}
	ledger, err := os.ReadFile(secondFence + ".retired")
	if err != nil || !bytes.Contains(ledger, []byte(`"`+dataset+`"`)) {
		t.Fatal("the second process's ledger missed the first process's erasure:", string(ledger), err)
	}
}
