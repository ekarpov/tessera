package sqlengine

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"github.com/ekarpov/tessera/internal/dbtest"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestOnlineBackupPinsExactPrefixesWithoutBlockingTransfers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { s, c, d := contentFixture(t); onlineBackupProgress(t, s, c, d) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("TESSERA_TEST_POSTGRES_DSN") == "" {
			t.Skip("needs PostgreSQL")
		}
		postgresAdmin(t)
		s := postgresFixture(t)
		d := sole(t, s)
		if err := s.SetAuthorization(t.Context(), d, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
			t.Fatal(err)
		}
		if err := s.Transact(t.Context(), owner, d, func(tx store.Tx) error {
			_, e := tx.CreateMetadata(p.Meta{ID: typeID, Fields: []p.Field{{ID: "n", MetaID: typeID, Type: p.IntegerType}}})
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.EnableContent(t.Context(), filepath.Join(t.TempDir(), "bytes")); err != nil {
			t.Fatal(err)
		}
		info, err := s.Info(t.Context(), owner, d)
		if err != nil {
			t.Fatal(err)
		}
		onlineBackupProgress(t, s, info.ContentStoreID, d)
	})
}
func onlineBackupProgress(t *testing.T, s *Store, contentID, dataset string) {
	if err := s.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	reply, err := s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody))
	if err != nil {
		t.Fatal(err)
	}
	object := identity(t, reply)
	begin := func(op string) map[string]any {
		v, e := s.ContentControl(t.Context(), owner, "begin", contentID, "", []byte(`{"operation_id":"`+op+`","content_type":"application/octet-stream","maximum_bytes":1048576}`))
		return contentMap(t, v, e)
	}
	complete := begin("complete")
	if _, err = s.ContentTransfer(t.Context(), strings.TrimPrefix(complete["url"].(string), "/v1/transfers/"), "PATCH", "bytes 0-2/3", strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	v, err := s.ContentControl(t.Context(), owner, "commit", contentID, complete["upload_id"].(string), nil)
	item := contentMap(t, v, err)["item_id"].(string)
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: "test.backup.media", Fields: []p.Field{{ID: "photo", MetaID: "test.backup.media", Type: p.MediaType}}}); e != nil {
			return e
		}
		ref, _ := p.NewValue(p.MediaType, contentID+"//"+item)
		_, e := tx.Insert("test.backup.media", map[string]p.Value{"photo": ref})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	partial := begin("partial")
	token := strings.TrimPrefix(partial["url"].(string), "/v1/transfers/")
	prefix := bytes.Repeat([]byte{'x'}, int(minChunkBytes))
	if _, err = s.ContentTransfer(t.Context(), token, "PATCH", "bytes 0-262143/*", bytes.NewReader(prefix)); err != nil {
		t.Fatal(err)
	}
	// Simulate a crashed uncommitted tail; the next PATCH must truncate it,
	// without damaging the committed prefix retained by the backup.
	f, err := os.OpenFile(filepath.Join(s.contentDir, partial["upload_id"].(string)), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("uncommitted"))
	_ = f.Close()
	empty, _, err := s.CaptureRetirementEvidence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	s.backupExportHook = func() { close(entered); <-resume }
	path := filepath.Join(t.TempDir(), "archive.enc")
	key := bytes.Repeat([]byte{5}, 32)
	done := make(chan error, 1)
	go func() { done <- s.Backup(context.Background(), path, key) }()
	<-entered
	device := s
	if s.postgres {
		device, err = openPostgres(t.Context(), dbtest.Postgres(t), filepath.Join(t.TempDir(), "device-fence"), p.DevelopmentLimits(), false)
		if err != nil {
			t.Fatal(err)
		}
		defer device.Close()
		if err = device.EnableContent(t.Context(), s.contentDir); err != nil {
			t.Fatal(err)
		}
	}
	progressed := make(chan error, 1)
	go func() {
		v, e := device.ContentControl(t.Context(), owner, "resolve", contentID, item, []byte(`{"representation":"original"}`))
		if e != nil {
			progressed <- e
			return
		}
		r, e := device.ContentTransfer(t.Context(), strings.TrimPrefix(v.(map[string]any)["url"].(string), "/v1/transfers/"), "GET", "", nil)
		if e != nil {
			progressed <- e
			return
		}
		raw, e := io.ReadAll(r.Body)
		r.Body.Close()
		if e != nil || string(raw) != "abc" {
			progressed <- store.Refuse("internal_error")
			return
		}
		_, e = device.ContentTransfer(t.Context(), token, "PATCH", "bytes 262144-262146/262147", strings.NewReader("end"))
		progressed <- e
	}()
	select {
	case e := <-progressed:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("bulk backup blocked device transfers")
	}
	// Erasure and cleanup after S cannot remove the pinned inodes.
	if _, err = s.ContentControl(t.Context(), owner, "erase", contentID, "", []byte(`{"confirm":"`+contentID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err = device.CleanupContent(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored.db")
	dir := filepath.Join(t.TempDir(), "bytes")
	fence := filepath.Join(t.TempDir(), "evidence")
	if err = os.WriteFile(fence, empty, 0600); err != nil {
		t.Fatal(err)
	}
	var restored *Store
	if s.postgres {
		dsn := dbtest.PostgresRestore(t)
		if dsn == "" {
			return
		}
		admin, e := sql.Open("tessera-postgres", dsn)
		if e != nil {
			t.Fatal(e)
		}
		defer admin.Close()
		reset := func() {
			if _, e := admin.ExecContext(t.Context(), `DO $$ DECLARE t record; BEGIN FOR t IN SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'tessera\_%' LOOP EXECUTE format('DROP TABLE IF EXISTS %I CASCADE', t.tablename); END LOOP; END $$`); e != nil {
				t.Fatal(e)
			}
		}
		reset()
		defer reset()
		if err = RestorePostgres(t.Context(), path, dsn, dest, dir, fence, key); err != nil {
			t.Fatal(err)
		}
		restored, err = openPostgres(t.Context(), dsn, dest, p.DevelopmentLimits(), false)
	} else {
		if err = RestoreBackup(t.Context(), path, dest, dir, fence, key); err != nil {
			t.Fatal(err)
		}
		restored, err = Open(t.Context(), dest, p.DevelopmentLimits())
	}
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got := record(t, restored, dataset, object).Records[0].Values[0].Payload(); got != int64(9007199254740993) {
		t.Fatal(got)
	}
	for id, want := range map[string][]byte{complete["upload_id"].(string): []byte("abc"), partial["upload_id"].(string): prefix} {
		raw, e := os.ReadFile(filepath.Join(dir, id))
		if e != nil || !bytes.Equal(raw, want) {
			t.Fatal("restored content differs from S", e, len(raw), len(want))
		}
	}
	var associations int
	if err = restored.db.QueryRow(`SELECT count(*) FROM "tessera_attachments"`).Scan(&associations); err != nil || associations != 1 {
		t.Fatal(associations, err)
	}
}

func TestEvidenceBoundariesAndHistoricalRestore(t *testing.T) {
	s, _, before := fixture(t)
	grant := store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}
	if err := s.SetAuthorization(t.Context(), before, owner, grant); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.Erase(t.Context(), owner, before, before)
	if err != nil {
		t.Fatal(err)
	}
	between := replacement.DatasetID
	archive := filepath.Join(t.TempDir(), "archive")
	key := bytes.Repeat([]byte{4}, 32)
	entered, resume := make(chan struct{}), make(chan struct{})
	s.backupExportHook = func() { close(entered); <-resume }
	done := make(chan error, 1)
	go func() { done <- s.Backup(t.Context(), archive, key) }()
	<-entered
	replacement, err = s.Erase(t.Context(), owner, between, between)
	if err != nil {
		t.Fatal(err)
	}
	after := replacement.DatasetID
	close(resume)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	evidence, r, err := s.CaptureRetirementEvidence(t.Context())
	if err != nil || r == "" {
		t.Fatal(err)
	}
	if !bytes.Contains(evidence, []byte(before)) || !bytes.Contains(evidence, []byte(between)) || bytes.Contains(evidence, []byte(after)) {
		t.Fatal("R boundary is incorrect")
	}
	if _, err = s.Erase(t.Context(), owner, after, after); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(evidence, []byte(after)) {
		t.Fatal("immutable evidence changed")
	}
	newer, _, err := s.CaptureRetirementEvidence(t.Context())
	if err != nil || !bytes.Contains(newer, []byte(after)) {
		t.Fatal(err)
	}
	fence := filepath.Join(t.TempDir(), "evidence")
	if err = os.WriteFile(fence, newer, 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored.db")
	if err = RestoreBackup(t.Context(), archive, dest, "", fence, key); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(t.Context(), dest, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	held, _, err := restored.CaptureRetirementEvidence(t.Context())
	if err != nil || !bytes.Equal(held, newer) {
		t.Fatal("historical evidence lost across restore", err)
	}
}
func TestEvidenceFromOtherPostgresProcess(t *testing.T) {
	if os.Getenv("TESSERA_TEST_POSTGRES_DSN") == "" {
		t.Skip("needs PostgreSQL")
	}
	postgresAdmin(t)
	first := postgresFixture(t)
	dataset := sole(t, first)
	second, err := openPostgres(t.Context(), os.Getenv("TESSERA_TEST_POSTGRES_DSN"), filepath.Join(t.TempDir(), "fence"), p.DevelopmentLimits(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err = first.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	if _, err = first.Erase(t.Context(), owner, dataset, dataset); err != nil {
		t.Fatal(err)
	}
	raw, _, err := second.CaptureRetirementEvidence(t.Context())
	if err != nil || !bytes.Contains(raw, []byte(dataset)) {
		t.Fatal("missed already journaled foreign retirement", err)
	}
}
func TestMissingRequiredBackupContentFails(t *testing.T) {
	s, c, _ := contentFixture(t)
	v, e := s.ContentControl(t.Context(), owner, "begin", c, "", []byte(`{"operation_id":"missing","content_type":"application/octet-stream","maximum_bytes":10}`))
	opened := contentMap(t, v, e)
	if _, e = s.ContentTransfer(t.Context(), strings.TrimPrefix(opened["url"].(string), "/v1/transfers/"), "PATCH", "bytes 0-2/3", strings.NewReader("abc")); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(s.contentDir, opened["upload_id"].(string))); e != nil {
		t.Fatal(e)
	}
	if e = s.Backup(t.Context(), filepath.Join(t.TempDir(), "backup"), bytes.Repeat([]byte{1}, 32)); p.ErrorCode(e) != "store_unavailable" {
		t.Fatal("published missing bytes", e)
	}
}

func TestEvidenceRejectsMalformedHistoryAndAcceptsEmpty(t *testing.T) {
	s, path, _ := fixture(t)
	raw, r, err := s.CaptureRetirementEvidence(t.Context())
	if err != nil || len(raw) != 0 || r == "" {
		t.Fatal("empty evidence failed", err)
	}
	if err = os.WriteFile(path+".retired", []byte("not json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CaptureRetirementEvidence(t.Context()); p.ErrorCode(err) != "dataset_unavailable" {
		t.Fatal("malformed history accepted", err)
	}
}
func TestBackupReclaimsOnlyAbandonedPins(t *testing.T) {
	s, _, _ := contentFixture(t)
	dir := filepath.Join(s.contentDir, ".backup-pins")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	guard, err := lockDatabase(filepath.Join(s.contentDir, ".backup-export.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Backup(t.Context(), filepath.Join(t.TempDir(), "archive"), bytes.Repeat([]byte{1}, 32)); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal("backup ignored live pin owner", err)
	}
	if _, err = os.Stat(dir); err != nil {
		t.Fatal("backup removed another exporter's pins")
	}
	guard.Close()
	if err = s.Backup(t.Context(), filepath.Join(t.TempDir(), "archive"), bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal("abandoned pins prevented recovery", err)
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("abandoned pins survived", err)
	}
}

// This optional measurement is separate from the barrier-driven progress proof.
// Run with TESSERA_BACKUP_IMPACT=1 and -v; no timing threshold gates correctness.
func TestOnlineBackupImpactMeasurement(t *testing.T) {
	if os.Getenv("TESSERA_BACKUP_IMPACT") != "1" {
		t.Skip("opt-in synthetic performance measurement")
	}
	s, c, _ := contentFixture(t)
	data := make([]byte, 32<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	v, e := s.ContentControl(t.Context(), owner, "begin", c, "", []byte(`{"operation_id":"impact-large","content_type":"application/octet-stream","maximum_bytes":33554432}`))
	opened := contentMap(t, v, e)
	token := strings.TrimPrefix(opened["url"].(string), "/v1/transfers/")
	for start := 0; start < len(data); start += 1 << 20 {
		end := start + (1 << 20)
		if _, e = s.ContentTransfer(t.Context(), token, "PATCH", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(data)), bytes.NewReader(data[start:end])); e != nil {
			t.Fatal(e)
		}
	}
	v, e = s.ContentControl(t.Context(), owner, "commit", c, opened["upload_id"].(string), nil)
	item := contentMap(t, v, e)["item_id"].(string)
	v, e = s.ContentControl(t.Context(), owner, "resolve", c, item, []byte(`{"representation":"original"}`))
	readToken := strings.TrimPrefix(contentMap(t, v, e)["url"].(string), "/v1/transfers/")
	v, e = s.ContentControl(t.Context(), owner, "begin", c, "", []byte(`{"operation_id":"impact-small","content_type":"application/octet-stream","maximum_bytes":10}`))
	writeToken := strings.TrimPrefix(contentMap(t, v, e)["url"].(string), "/v1/transfers/")
	measure := func() []time.Duration {
		var samples []time.Duration
		for i := 0; i < 50; i++ {
			start := time.Now()
			r, e := s.ContentTransfer(t.Context(), readToken, "GET", "", nil)
			if e != nil {
				t.Fatal(e)
			}
			_, e = io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.ContentTransfer(t.Context(), writeToken, "PATCH", "bytes 0-2/3", strings.NewReader("abc")); e != nil {
				t.Fatal(e)
			}
			samples = append(samples, time.Since(start))
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples
	}
	baseline := measure()
	entered := make(chan struct{})
	s.backupExportHook = func() { close(entered) }
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- s.Backup(t.Context(), filepath.Join(t.TempDir(), "impact.enc"), bytes.Repeat([]byte{7}, 32))
	}()
	<-entered
	during := measure()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	t.Logf("SQLite; 32 MiB random content; 50 sequential full GET + 3-byte checkpoint/replay pairs: baseline p50=%s p95=%s; concurrent export p50=%s p95=%s; backup=%s", baseline[25], baseline[47], during[25], during[47], time.Since(start))
}

func TestBackupHostFilesAndSQLGrantsShareSnapshot(t *testing.T) {
	s, _, dataset := fixture(t)
	host := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(host, []byte(`{"write":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	var hostMu sync.Mutex
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(resume) }) })
	s.backupExportHook = func() { close(entered); <-resume }
	archive := filepath.Join(t.TempDir(), "archive")
	key := bytes.Repeat([]byte{8}, 32)
	done := make(chan error, 1)
	go func() {
		_, err := s.BackupCapturedWithFiles(t.Context(), archive, key, func() func() { hostMu.Lock(); return hostMu.Unlock }, func() []string { return []string{host} })
		done <- err
	}()
	<-entered
	hostMu.Lock()
	err := s.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true}, View: store.FullView()})
	if err == nil {
		err = os.WriteFile(host, []byte(`{"write":false}`), 0600)
	}
	hostMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(resume) })
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	evidence, _, err := s.CaptureRetirementEvidence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence := filepath.Join(t.TempDir(), "fence")
	if err = os.WriteFile(fence, evidence, 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "dataset.db")
	hostDir := t.TempDir()
	if err = RestoreBackup(t.Context(), archive, destination, "", fence, key, hostDir); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(t.Context(), destination, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	raw, err := os.ReadFile(filepath.Join(hostDir, "registry.json"))
	if err != nil || string(raw) != `{"write":true}` {
		t.Fatal("host files did not match S", err)
	}
	if _, err = restored.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody)); err != nil {
		t.Fatal("SQL grants did not match registry at S", err)
	}
}

func TestBackupFilePreparationBound(t *testing.T) {
	s, c, _ := contentFixture(t)
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxBackupFiles; i++ {
		id := fmt.Sprintf("u-synthetic-%d", i)
		if _, err = tx.ExecContext(t.Context(), `INSERT INTO "tessera_imports" ("upload_id","store_id","operation_id","principal","authorization","content_type","maximum_bytes","state","updated_at") VALUES ($1,$2,$1,'synthetic','synthetic','application/octet-stream',1,'open',0)`, id, c); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.Backup(t.Context(), filepath.Join(t.TempDir(), "archive"), bytes.Repeat([]byte{1}, 32)); p.ErrorCode(err) != "query_result_limit_exceeded" {
		t.Fatal("file preparation bound not enforced", err)
	}
}

func TestIndependentPostgresRegistriesRefuseArchiveButAllowEvidence(t *testing.T) {
	if os.Getenv("TESSERA_TEST_POSTGRES_DSN") == "" {
		t.Skip("needs PostgreSQL")
	}
	postgresAdmin(t)
	first := postgresFixture(t)
	if err := first.RegisterHostRegistry(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := openPostgres(t.Context(), dbtest.Postgres(t), filepath.Join(t.TempDir(), "second-fence"), p.DevelopmentLimits(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = second.RegisterHostRegistry(t.Context()); err != nil {
		second.Close()
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{2}, 32)
	if err = first.Backup(t.Context(), filepath.Join(t.TempDir(), "refused"), key); p.ErrorCode(err) != "not_supported" {
		second.Close()
		t.Fatal("captured independent registries as coherent", err)
	}
	if _, _, err = first.CaptureRetirementEvidence(t.Context()); err != nil {
		second.Close()
		t.Fatal("independent registries blocked evidence", err)
	}
	second.Close()
	if err = first.Backup(t.Context(), filepath.Join(t.TempDir(), "single-owner"), key); err != nil {
		t.Fatal("single registry owner could not capture", err)
	}
}
