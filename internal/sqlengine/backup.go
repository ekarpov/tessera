package sqlengine

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ekarpov/tessera/internal/recordstate"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const maxBackupBytes = 256 << 20

var backupTables = []string{"tessera_dataset", "tessera_types", "tessera_objects", "tessera_access", "tessera_receipts", "tessera_retirements", "tessera_content_stores", "tessera_imports", "tessera_items", "tessera_attachments", "tessera_field_values", "tessera_managed_layouts", "tessera_managed_installations"}

type backupValue struct {
	Bytes   []byte  `json:"bytes,omitempty"`
	Text    *string `json:"text,omitempty"`
	Integer *int64  `json:"integer,omitempty"`
	Kind    string  `json:"kind"`
}
type backupTable struct {
	Name    string
	Columns []string
	Rows    [][]backupValue
}
type backupManifest struct {
	Capture string `json:",omitempty"`
	Limits  p.Limits
	Tables  []backupTable
}

func backupCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, store.Refuse("invalid_field")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Backup writes an authenticated encrypted snapshot. The key and independent
// retirement ledger must be retained separately. Existing output is never replaced.
func (s *Store) Backup(ctx context.Context, path string, key []byte, hostFiles ...string) error {
	return s.BackupConsistent(ctx, path, key, nil, hostFiles...)
}

// BackupConsistent is Backup for a host whose files describe the same state
// as the SQL: an account registry whose grants the SQL access rows carry.
// freeze stops the host changing them and returns what lets it continue. The
// SQL snapshot is pinned and the host files are read while it holds, so the
// archive cannot pair a registry from one moment with grants from another —
// which, restored, could give a token more than the live server did when the
// backup was taken. It holds only for the pin and the read, not the export.
func (s *Store) BackupConsistent(ctx context.Context, path string, key []byte, freeze func() func(), hostFiles ...string) error {
	_, err := s.BackupCaptured(ctx, path, key, freeze, hostFiles...)
	return err
}

// BackupCaptured also returns the opaque S label authenticated inside the archive.
func (s *Store) BackupCaptured(ctx context.Context, path string, key []byte, freeze func() func(), hostFiles ...string) (string, error) {
	capture, err := cursorToken()
	if err != nil {
		return "", err
	}
	err = s.backupCaptured(ctx, path, key, freeze, capture, nil, hostFiles...)
	if err != nil {
		return "", err
	}
	return capture, nil
}

// BackupCapturedWithFiles resolves optional host files while their owner is frozen.
func (s *Store) BackupCapturedWithFiles(ctx context.Context, path string, key []byte, freeze func() func(), files func() []string) (string, error) {
	capture, err := cursorToken()
	if err != nil {
		return "", err
	}
	err = s.backupCaptured(ctx, path, key, freeze, capture, files)
	if err != nil {
		return "", err
	}
	return capture, nil
}

func (s *Store) backupCaptured(ctx context.Context, path string, key []byte, freeze func() func(), capture string, files func() []string, hostFiles ...string) error {
	if !s.backupMu.TryLock() {
		return store.Refuse("limit_exceeded")
	}
	defer s.backupMu.Unlock()
	aead, err := backupCipher(key)
	if err != nil {
		return err
	}
	if err = s.enter(); err != nil {
		return err
	}
	defer s.life.RUnlock()
	// A separate lifetime lock proves no other process owns pin files before
	// reclaiming an interrupted export. Device checkpoints never take it.
	if directory := s.byteDirectory(); directory != "" {
		guard, e := lockDatabase(filepath.Join(directory, ".backup-export.lock"))
		if e != nil {
			if lockContended(e) {
				return store.Refuse("limit_exceeded")
			}
			return store.Refuse("store_unavailable")
		}
		defer guard.Close()
		if e = os.RemoveAll(filepath.Join(directory, ".backup-pins")); e != nil {
			return store.Refuse("store_unavailable")
		}
	}
	if _, _, err = s.captureRetirementEvidence(ctx); err != nil {
		return err
	}
	// Ensure a durable restore fence exists even before the first retirement.
	fence, err := os.OpenFile(s.path+".retired", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return store.Refuse("dataset_unavailable")
	}
	err = fence.Sync()
	fence.Close()
	if err != nil {
		return store.Refuse("dataset_unavailable")
	}
	// The host is frozen before the transfer lock is taken: a host holding
	// its own lock may call into this store, which may take the transfer
	// lock, so the other order could deadlock against it.
	thaw := func() {}
	if freeze != nil {
		thaw = freeze()
	}
	unpinHost, err := s.pinHostRegistry(ctx)
	if err != nil {
		thaw()
		return err
	}
	thawed := false
	release := func() {
		if !thawed {
			thawed = true
			unpinHost()
			thaw()
		}
	}
	defer release()
	unlock, err := s.lockTransfers(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, 2*time.Second)
	defer cancelPrepare()
	// One snapshot across every table. SQLite's read transaction already is
	// one; PostgreSQL's default isolation is not — under read committed each
	// statement sees its own moment, so ten tables read while writes land can
	// describe no state the database was ever in. That never mattered while
	// backups needed the server stopped. Taken online, it would.
	options := &sql.TxOptions{ReadOnly: true}
	if s.postgres {
		options.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.reader.BeginTx(ctx, options)
	if err != nil {
		return dbError(ctx, err)
	}
	defer tx.Rollback()
	// A read transaction's snapshot is taken at its first read, not when it
	// begins, on both engines. This read is that moment, and the host files
	// are read with the host still frozen at it.
	var pinned int
	if err = tx.QueryRowContext(prepareCtx, `SELECT count(*) FROM (SELECT "dataset_id" FROM "tessera_dataset" LIMIT 1) AS "pin"`).Scan(&pinned); err != nil {
		return dbError(ctx, err)
	}
	type hostFile struct {
		name    string
		content []byte
	}
	if files != nil {
		hostFiles = files()
	}
	var hostContents []hostFile
	hosts := map[string]bool{}
	for _, path := range hostFiles {
		name := filepath.Base(path)
		if hosts[name] {
			return store.Refuse("invalid_field")
		}
		hosts[name] = true
		file, e := os.Open(path)
		if e != nil {
			return store.Refuse("dataset_unavailable")
		}
		content, e := io.ReadAll(io.LimitReader(backupContextReader{prepareCtx, file}, maxBackupBytes+1))
		file.Close()
		if e != nil {
			return store.Refuse("dataset_unavailable")
		}
		if len(content) > maxBackupBytes {
			return store.Refuse("query_result_limit_exceeded")
		}
		hostContents = append(hostContents, hostFile{name, content})
	}
	pins, pinDir, err := s.pinBackupContent(prepareCtx, tx)
	if pinDir != "" {
		defer os.RemoveAll(pinDir)
	}
	if err != nil {
		return err
	}
	if err = prepareCtx.Err(); err != nil {
		return err
	}
	cancelPrepare()
	unlock()
	unlock = nil
	release()
	if s.backupExportHook != nil {
		s.backupExportHook()
	}
	manifest := backupManifest{Limits: s.bounds(), Capture: capture}
	total := 0
	for _, name := range backupTables {
		query := `SELECT * FROM "` + name + `"`
		switch name {
		case "tessera_objects":
			// Pack one portable payload per object, as the legacy archive did.
			// Charging a full archive row for every field exhausted the memory
			// budget much earlier even for data the previous release could back up.
			query = `SELECT "dataset_id","object_id","type_id","fields" FROM "tessera_read_objects"`
			if s.postgres {
				query = `SELECT "object_id","type_id","dataset_id","fields" FROM "tessera_read_objects"`
			}
		case "tessera_field_values":
			// Retain the archive table inventory. All values now travel with the
			// object payload, including the managed columns.
			query += " LIMIT 0"
		}
		rows, e := tx.QueryContext(ctx, query)
		if e != nil {
			return dbError(ctx, e)
		}
		columns, e := rows.Columns()
		if e != nil {
			rows.Close()
			return dbError(ctx, e)
		}
		table := backupTable{Name: name, Columns: columns}
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(values))
			for i := range values {
				targets[i] = &values[i]
			}
			if e = rows.Scan(targets...); e != nil {
				break
			}
			if name == "tessera_objects" {
				raw, ok := values[3].([]byte)
				var record recordstate.Row
				if !ok || decodeStoredFields(raw, &record) != nil {
					e = store.Refuse("internal_error")
					break
				}
				values[3], e = p.MarshalWire(record.Fields)
				if e != nil {
					break
				}
			}
			row := make([]backupValue, len(values))
			for i, value := range values {
				total += 64 // materialized backupValue and scan/row bookkeeping
				switch v := value.(type) {
				case nil:
					row[i].Kind = "null"
				case []byte:
					row[i] = backupValue{Kind: "bytes", Bytes: append([]byte(nil), v...)}
					total += len(v)
				case string:
					copy := v
					row[i] = backupValue{Kind: "text", Text: &copy}
					total += len(v)
				case int64:
					copy := v
					row[i] = backupValue{Kind: "integer", Integer: &copy}
					total += 8
				default:
					e = store.Refuse("internal_error")
				}
			}
			if e != nil || total > maxBackupBytes {
				e = store.Refuse("query_result_limit_exceeded")
				break
			}
			table.Rows = append(table.Rows, row)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
		manifest.Tables = append(manifest.Tables, table)
	}
	_ = tx.Rollback()
	var plain bytes.Buffer
	archive := zip.NewWriter(&plain)
	entry, err := archive.Create("manifest.json")
	if err != nil {
		return err
	}
	remaining := int64(maxBackupBytes)
	if err = json.NewEncoder(backupBoundWriter{entry, &remaining}).Encode(manifest); err != nil {
		return err
	}
	for _, pin := range pins {
		if err = ctx.Err(); err != nil {
			return err
		}
		total += int(pin.size)
		if total > maxBackupBytes {
			return store.Refuse("query_result_limit_exceeded")
		}
		entry, err = archive.Create("bytes/" + pin.name)
		if err != nil {
			return err
		}
		if pin.size == 0 {
			continue
		}
		file, e := os.Open(filepath.Join(pinDir, pin.name))
		if e != nil {
			return store.Refuse("store_unavailable")
		}
		_, e = io.CopyN(backupBoundWriter{entry, &remaining}, file, pin.size)
		file.Close()
		if e != nil {
			return store.Refuse("store_unavailable")
		}
	}

	for _, host := range hostContents {
		if len(host.content) > maxBackupBytes-total {
			return store.Refuse("query_result_limit_exceeded")
		}
		total += len(host.content)
		hostEntry, e := archive.Create("host/" + host.name)
		if e == nil {
			_, e = (backupBoundWriter{hostEntry, &remaining}).Write(host.content)
		}
		if e != nil {
			return store.Refuse("dataset_unavailable")
		}
	}
	if err = archive.Close(); err != nil {
		return err
	}
	if plain.Len() > maxBackupBytes {
		return store.Refuse("query_result_limit_exceeded")
	}
	// A pinned owner connection cannot reconnect. Its continued liveness proves
	// the host-registry guard was not lost during S/host-file capture.
	if s.pgOwner != nil && s.pgOwner.PingContext(ctx) != nil {
		return store.Refuse("dataset_unavailable")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	encrypted := append([]byte("TESSERA-BACKUP-1\n"), nonce...)
	encrypted = aead.Seal(encrypted, nonce, plain.Bytes(), []byte("TESSERA-BACKUP-1"))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return store.Refuse("dataset_unavailable")
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err = file.Write(encrypted); err == nil {
		err = file.Sync()
	}
	if err != nil {
		return store.Refuse("dataset_unavailable")
	}
	if err = syncDirectory(filepath.Dir(path)); err != nil {
		return store.Refuse("dataset_unavailable")
	}
	ok = true
	return nil
}

// RestoreBackup restores into a fresh SQLite destination, never overwriting a
// running dataset. fence is the independent current ledger, not a ledger from
// inside the backup. A retired snapshot is refused before any data is served.
func RestoreBackup(ctx context.Context, source, destination, contentDir, fencePath string, key []byte, hostDirectory ...string) error {
	return restoreBackup(ctx, source, destination, contentDir, fencePath, key, Open, hostDirectory...)
}

func RestorePostgres(ctx context.Context, source, dsn, destinationFence, contentDir, currentFence string, key []byte, hostDirectory ...string) error {
	return restoreBackup(ctx, source, destinationFence, contentDir, currentFence, key, func(ctx context.Context, path string, limits p.Limits) (*Store, error) {
		return openPostgres(ctx, dsn, path, limits, true)
	}, hostDirectory...)
}

func restoreBackup(ctx context.Context, source, destination, contentDir, fencePath string, key []byte, open func(context.Context, string, p.Limits) (*Store, error), hostDirectory ...string) error {
	aead, err := backupCipher(key)
	if err != nil {
		return err
	}
	file, err := os.Open(source)
	if err != nil {
		return store.Refuse("dataset_unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBackupBytes+1024))
	file.Close()
	if err != nil || len(raw) > maxBackupBytes+512 {
		return store.Refuse("invalid_field")
	}
	prefix := []byte("TESSERA-BACKUP-1\n")
	if !bytes.HasPrefix(raw, prefix) || len(raw) < len(prefix)+aead.NonceSize() {
		return store.Refuse("invalid_field")
	}
	raw = raw[len(prefix):]
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte("TESSERA-BACKUP-1"))
	if err != nil {
		return store.Refuse("invalid_field")
	}
	archive, err := zip.NewReader(bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		return store.Refuse("invalid_field")
	}
	var manifest backupManifest
	var manifestFound bool
	var size uint64
	for _, entry := range archive.File {
		size += entry.UncompressedSize64
		if size > maxBackupBytes {
			return store.Refuse("query_result_limit_exceeded")
		}
		if entry.Name == "manifest.json" {
			if manifestFound {
				return store.Refuse("invalid_field")
			}
			manifestFound = true
			reader, e := entry.Open()
			if e != nil {
				return e
			}
			e = json.NewDecoder(reader).Decode(&manifest)
			reader.Close()
			if e != nil {
				return store.Refuse("invalid_field")
			}
		}
	}
	if !manifestFound || (len(manifest.Tables) != len(backupTables) && len(manifest.Tables) != len(backupTables)-2 && len(manifest.Tables) != len(backupTables)-3) {
		return store.Refuse("invalid_field")
	}
	fence, err := os.ReadFile(fencePath)
	if err != nil {
		return store.Refuse("dataset_unavailable")
	}
	erasedDatasets, erasedStores, err := retiredInBackup(manifest, fence)
	if err != nil {
		return err
	}
	if _, err = os.Stat(destination); !os.IsNotExist(err) {
		return store.Refuse("conflict")
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return store.Refuse("dataset_unavailable")
	}
	target, e := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return store.Refuse("conflict")
	}
	target.Close()
	success := false
	createdContent := false
	createdFence := false
	defer func() {
		if !success {
			for _, path := range []string{destination, destination + "-wal", destination + "-shm"} {
				_ = os.Remove(path)
			}
			if createdFence {
				_ = os.Remove(destination + ".retired")
			}
			if createdContent {
				_ = os.RemoveAll(contentDir)
			}
		}
	}()
	fenceFile, e := os.OpenFile(destination+".retired", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return store.Refuse("conflict")
	}
	createdFence = true
	_, e = fenceFile.Write(fence)
	if e == nil {
		e = fenceFile.Sync()
	}
	fenceFile.Close()
	if e != nil {
		return store.Refuse("dataset_unavailable")
	}

	if err = syncDirectory(filepath.Dir(destination)); err != nil {
		return store.Refuse("dataset_unavailable")
	}
	s, err := open(ctx, destination, manifest.Limits)
	if err != nil {
		return err
	}
	defer s.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return dbError(ctx, err)
	}
	defer tx.Rollback()
	for i := len(backupTables) - 1; i >= 0; i-- {
		if _, err = tx.ExecContext(ctx, `DELETE FROM "`+backupTables[i]+`"`); err != nil {
			return dbError(ctx, err)
		}
	}
	var packedObjects backupTable
	var packedPayloads [][]byte
	// Packed archives carry the values with their objects and have an empty
	// field table. Older archives can mix packed and flattened values.
	packed := len(manifest.Tables) == len(backupTables) &&
		len(manifest.Tables[slices.Index(backupTables, "tessera_field_values")].Rows) == 0

	for i, table := range manifest.Tables {
		if table.Name != backupTables[i] {
			return store.Refuse("invalid_field")
		}
		var legacyPayloads [][]byte
		switch table.Name {
		case "tessera_dataset":
			stripBackupColumn(&table, "typed_fields")
			// The creator-only schema predates the managed storage columns.
			if slices.Contains(table.Columns, "creator_secret") && !slices.Contains(table.Columns, "schema_cache_key") {
				index := slices.Index(table.Columns, "creator_secret")
				for i, row := range table.Rows {
					if len(row) != len(table.Columns) {
						return store.Refuse("invalid_field")
					}
					creator := row[index]
					row = slices.Delete(row, index, index+1)
					table.Rows[i] = append(row, nullBackupValue(), nullBackupValue(), creator)
				}
				table.Columns = append(slices.Delete(table.Columns, index, index+1), "schema_cache_key", "managed_key", "creator_secret")
			}
		case "tessera_objects":
			packed = packed && slices.Contains(table.Columns, "fields") && !slices.Contains(table.Columns, "wire_bytes")
			legacyPayloads, err = normalizeObjectArchive(&table)
			if err != nil {
				return err
			}
		}
		actual, err := tx.QueryContext(ctx, `SELECT * FROM "`+table.Name+`" LIMIT 0`)
		if err != nil {
			return dbError(ctx, err)
		}
		columns, err := actual.Columns()
		actual.Close()
		// An older archive is short exactly the receipt columns later migrations
		// appended, in the order they appended them. Each one has a known fill,
		// so an archive missing any suffix of them can be carried forward. Every
		// other shape mismatch is still an error.
		if err == nil && table.Name == "tessera_receipts" && len(columns) > len(table.Columns) && strings.Join(columns[:len(table.Columns)], "\x00") == strings.Join(table.Columns, "\x00") {
			bodyIndex := -1
			for j, name := range table.Columns {
				if name == "body" {
					bodyIndex = j
				}
			}
			filled := true
			for _, name := range columns[len(table.Columns):] {
				if _, known := receiptColumnFills[name]; !known {
					filled = false
				}
			}
			if filled && bodyIndex >= 0 {
				for j, row := range table.Rows {
					if len(row) != len(table.Columns) {
						return store.Refuse("invalid_field")
					}
					raw := row[bodyIndex].Bytes
					if row[bodyIndex].Text != nil {
						raw = []byte(*row[bodyIndex].Text)
					}
					for _, name := range columns[len(table.Columns):] {
						row = append(row, receiptColumnFills[name](raw))
					}
					table.Rows[j] = row
				}
				table.Columns = columns
			}
		}
		// An older archive may be short columns a later migration appended to
		// a table, in the order it appended them. Where each missing one has
		// a known fill, the archive is carried forward; any other shape
		// mismatch is still refused below.
		if err == nil && table.Name != "tessera_receipts" && len(columns) > len(table.Columns) &&
			strings.Join(columns[:len(table.Columns)], "\x00") == strings.Join(table.Columns, "\x00") {
			fills, known := archiveColumnFills[table.Name], true
			for _, name := range columns[len(table.Columns):] {
				_, has := fills[name]
				known = known && has
			}
			if known {
				for j, row := range table.Rows {
					for _, name := range columns[len(table.Columns):] {
						row = append(row, fills[name]())
					}
					table.Rows[j] = row
				}
				table.Columns = columns
			}
		}
		if err != nil || strings.Join(columns, "\x00") != strings.Join(table.Columns, "\x00") {
			return store.Refuse("invalid_field")
		}
		insert := restoreInsert{prefix: `INSERT INTO "` + table.Name + `" VALUES `}
		datasetColumn, objectColumn := slices.Index(columns, "dataset_id"), slices.Index(columns, "object_id")
		for index, row := range table.Rows {
			if len(row) != len(columns) {
				return store.Refuse("invalid_field")
			}
			values := make([]any, len(row))
			for j, value := range row {
				switch value.Kind {
				case "null":
				case "bytes":
					// Preserve an empty binary value even when the archive omitted its bytes.
					values[j] = append([]byte{}, value.Bytes...)
				case "text":
					values[j] = value.Text
				case "integer":
					values[j] = value.Integer
				default:
					return store.Refuse("invalid_field")
				}
			}
			if err = insert.add(ctx, tx, values); err != nil {
				return err
			}
			if !packed && len(legacyPayloads) != 0 && legacyPayloads[index] != nil {
				if err = insert.flush(ctx, tx); err != nil {
					return err
				}
				dataset, datasetOK := values[datasetColumn].(*string)
				id, idOK := values[objectColumn].(*string)
				if !datasetOK || !idOK || dataset == nil || id == nil {
					return store.Refuse("invalid_field")
				}
				if err = importLegacyFields(ctx, tx, *dataset, *id, legacyPayloads[index]); err != nil {
					return err
				}
			}
		}
		if err = insert.flush(ctx, tx); err != nil {
			return err
		}
		if packed && table.Name == "tessera_objects" {
			packedObjects, packedPayloads = table, legacyPayloads
		}

	}
	// A restored copy is a distinct storage and every dataset in it gets its own
	// fresh version secret: sharing one across the copy would let a watermark
	// minted in the original be accepted here. Creator identifiers belong to
	// the logical dataset and remain stable when its records are restored.
	// A dataset or content store erased since this backup was taken is
	// erased again here, in the same transaction that loads it, so what was
	// erased is never committed and never served. Refusing the whole backup
	// instead, as restore once did, made every earlier backup unrestorable as
	// soon as anybody on a storage holding many people's datasets erased
	// anything — a disaster before the next backup then left nothing to
	// restore. The replacement is held by nobody, as after an account
	// deletion's erasure.
	// A dataset's record count and size are worked out from the records the
	// archive holds rather than trusted from it: they are derived, and an
	// archive from before they were kept has none.
	if err = s.restoreManagedRows(ctx, tx, packedObjects, packedPayloads); err != nil {
		return err
	}
	if err = recount(ctx, tx); err != nil {
		return err
	}
	restorer := store.Access{Principal: "restore", Authorization: "restore-reapplied-erasure"}
	for _, id := range erasedStores {
		if _, err = s.eraseContent(ctx, tx, restorer, id); err != nil {
			return err
		}
	}
	for _, id := range erasedDatasets {
		replaced, e := s.eraseWithin(ctx, tx, restorer, id, manifest.Limits)
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM "tessera_access" WHERE "dataset_id"=$1`, replaced.DatasetID); err != nil {
			return dbError(ctx, err)
		}
	}
	// Bytes an erasure just removed are not written out at all.
	erasedBytes := map[string]bool{}
	if len(erasedDatasets)+len(erasedStores) > 0 {
		rows, e := tx.QueryContext(ctx, `SELECT "upload_id" FROM "tessera_items" WHERE "deleted"=1`)
		if e != nil {
			return dbError(ctx, e)
		}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				break
			}
			erasedBytes[id] = true
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return dbError(ctx, e)
		}
	}
	restoredIDs, err := datasetIdentities(ctx, tx)
	if err != nil {
		return err
	}
	for _, id := range restoredIDs {
		var secret [32]byte
		if _, err = rand.Read(secret[:]); err != nil {
			return err
		}
		watermark, e := cursorToken()
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `UPDATE "tessera_dataset" SET "creator_secret"=coalesce("creator_secret","secret"),"secret"=$1,"watermark"=$2 WHERE "dataset_id"=$3`, secret[:], "w:"+watermark, id); err != nil {
			return dbError(ctx, err)
		}
	}
	if contentDir != "" {
		if err = os.Mkdir(contentDir, 0700); err != nil {
			return store.Refuse("conflict")
		}
		createdContent = true
	}

	restoredHostFiles := []string{}
	defer func() {
		if !success {
			for _, path := range restoredHostFiles {
				_ = os.Remove(path)
			}
		}
	}()
	for _, entry := range archive.File {
		if entry.Name == "manifest.json" {
			continue
		}

		if strings.HasPrefix(entry.Name, "host/") {
			if len(hostDirectory) != 1 || filepath.Base(entry.Name) != strings.TrimPrefix(entry.Name, "host/") {
				return store.Refuse("invalid_field")
			}
			reader, e := entry.Open()
			if e != nil {
				return e
			}
			path := filepath.Join(hostDirectory[0], filepath.Base(entry.Name))
			out, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				reader.Close()
				return store.Refuse("conflict")
			}
			restoredHostFiles = append(restoredHostFiles, path)
			_, e = io.Copy(out, reader)
			if e == nil {
				e = out.Sync()
			}
			out.Close()
			reader.Close()
			if e != nil {
				return store.Refuse("dataset_unavailable")
			}
			continue
		}
		// Bytes an erasure removed are skipped before anything else is asked of
		// them, including whether there is anywhere to put them.
		if strings.HasPrefix(entry.Name, "bytes/") && erasedBytes[strings.TrimPrefix(entry.Name, "bytes/")] {
			continue
		}
		if contentDir == "" || !strings.HasPrefix(entry.Name, "bytes/u-") || filepath.Base(entry.Name) != strings.TrimPrefix(entry.Name, "bytes/") {
			return store.Refuse("invalid_field")
		}
		reader, e := entry.Open()
		if e != nil {
			return e
		}
		out, e := os.OpenFile(filepath.Join(contentDir, filepath.Base(entry.Name)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			reader.Close()
			return store.Refuse("store_unavailable")
		}
		_, e = io.Copy(out, reader)
		if e == nil {
			e = out.Sync()
		}
		out.Close()
		reader.Close()
		if e != nil {
			return store.Refuse("store_unavailable")
		}
	}
	if contentDir != "" {
		if err = syncDirectory(contentDir); err != nil {
			return store.Refuse("store_unavailable")
		}
	}
	if len(hostDirectory) == 1 {
		if err = syncDirectory(hostDirectory[0]); err != nil {
			return store.Refuse("dataset_unavailable")
		}
	}
	if err = tx.Commit(); err != nil {
		return dbError(ctx, err)
	}
	// Pages that held erased rows are rewritten before the restore reports
	// success, rather than on the server's first maintenance pass.
	if len(erasedDatasets)+len(erasedStores) > 0 {
		if contentDir != "" {
			if err = s.SetContentCleanupDirectory(contentDir); err != nil {
				return err
			}
			if err = s.CleanupContent(ctx); err != nil {
				return err
			}
		}
		if err = s.CheckpointCleanup(ctx); err != nil {
			return err
		}
	}
	success = true
	return nil
}

// receiptColumnFills gives a value for a receipt column an older archive does
// not carry, from the retained body. A restored claim names no executor, which
// is what the empty string means everywhere else: made before executors were
// recorded. It is safe because a restore targets a destination nothing is
// running against.
var receiptColumnFills = map[string]func(body []byte) backupValue{
	"watermark":    func(body []byte) backupValue { v := replyWatermark(body); return backupValue{Kind: "text", Text: &v} },
	"replay_scope": func([]byte) backupValue { v := "read"; return backupValue{Kind: "text", Text: &v} },
	"executor_id":  func([]byte) backupValue { v := ""; return backupValue{Kind: "text", Text: &v} },
	// A receipt from before server scope carries no promise, so it is null
	// rather than back-dated: null means the dataset window governs it, which
	// is the rule it was written under and the one it should keep.
	"retain_until": func([]byte) backupValue { return backupValue{Kind: "null"} },
}

// retiredInBackup names the datasets and content stores the backup holds
// live that the current ledger says were erased since it was taken. A ledger
// that does not parse refuses the restore: without it there is no telling
// what must not come back.
func retiredInBackup(manifest backupManifest, fence []byte) (datasets, stores []string, err error) {
	retired := map[string]bool{}
	for _, line := range bytes.Split(fence, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var id string
		if json.Unmarshal(line, &id) != nil {
			return nil, nil, store.Refuse("dataset_unavailable")
		}
		retired[id] = true
	}
	erasedDataset := map[string]bool{}
	type owned struct{ id, dataset string }
	var erasedStores []owned
	for _, table := range manifest.Tables {
		if table.Name != "tessera_dataset" && table.Name != "tessera_content_stores" {
			continue
		}
		for _, row := range table.Rows {
			if len(row) != len(table.Columns) {
				return nil, nil, store.Refuse("invalid_field")
			}
			id, owner := "", ""
			active := table.Name == "tessera_dataset"
			for i, column := range table.Columns {
				if (table.Name == "tessera_dataset" && column == "dataset_id") || (table.Name == "tessera_content_stores" && column == "id") {
					if row[i].Text != nil {
						id = *row[i].Text
					}
				}
				if table.Name == "tessera_content_stores" && column == "dataset_id" && row[i].Text != nil {
					owner = *row[i].Text
				}
				if column == "active" && row[i].Integer != nil {
					active = *row[i].Integer == 1
				}
			}
			if !active || !retired[id] {
				continue
			}
			if table.Name == "tessera_dataset" {
				datasets = append(datasets, id)
				erasedDataset[id] = true
			} else {
				erasedStores = append(erasedStores, owned{id, owner})
			}
		}
	}
	// A content store whose dataset is erased goes with the dataset.
	for _, entry := range erasedStores {
		if !erasedDataset[entry.dataset] {
			stores = append(stores, entry.id)
		}
	}
	sort.Strings(datasets)
	sort.Strings(stores)
	return datasets, stores, nil
}

func datasetIdentities(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT "dataset_id" FROM "tessera_dataset"`)
	if err != nil {
		return nil, dbError(ctx, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, dbError(ctx, err)
	}
	return ids, nil
}

// recount sets every dataset's record count and stored bytes from its records.
func recount(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE "tessera_dataset" SET
		"object_count" = (SELECT count(*) FROM "tessera_objects" o WHERE o."dataset_id" = "tessera_dataset"."dataset_id"),
		"object_bytes" = (SELECT coalesce(sum(o."wire_bytes"), 0) FROM "tessera_objects" o WHERE o."dataset_id" = "tessera_dataset"."dataset_id")`)
	if err != nil {
		return dbError(ctx, err)
	}
	return nil
}

// archiveColumnFills gives the value of a column a later migration appended,
// for a row from an archive made before it. The dataset's counts are filled
// with zero and then worked out from the records. The creator key starts
// null so restore can preserve the original secret before rotating it.
// An aborted import is taken as not yet cleaned.
var archiveColumnFills = map[string]map[string]func() backupValue{
	"tessera_dataset": {
		"object_count":     zeroValue,
		"object_bytes":     zeroValue,
		"schema_cache_key": nullBackupValue,
		"managed_key":      nullBackupValue,
		"creator_secret":   nullBackupValue,
	},
	"tessera_imports": {
		"cleaned": zeroValue,
	},
}

func nullBackupValue() backupValue { return backupValue{Kind: "null"} }

func zeroValue() backupValue {
	zero := int64(0)
	return backupValue{Kind: "integer", Integer: &zero}
}

// backupBoundWriter enforces restore's aggregate uncompressed ZIP ceiling.
type backupBoundWriter struct {
	writer    io.Writer
	remaining *int64
}

func (b backupBoundWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > *b.remaining {
		return 0, store.Refuse("query_result_limit_exceeded")
	}
	n, err := b.writer.Write(p)
	*b.remaining -= int64(n)
	return n, err
}

type backupContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r backupContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
