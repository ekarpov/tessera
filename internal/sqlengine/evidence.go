package sqlengine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/ekarpov/tessera/pkg/store"
)

const maxEvidenceBytes = 16 << 20

// CaptureRetirementEvidence captures all SQL retirements at one database snapshot
// plus independently retained historical evidence. It never relies on journaled
// flags, timestamps or a maintenance pass. The result is the supported restore
// fence format, including the valid empty fence. The label identifies R only.
func (s *Store) CaptureRetirementEvidence(ctx context.Context) ([]byte, string, error) {
	if err := s.enter(); err != nil {
		return nil, "", err
	}
	defer s.life.RUnlock()
	return s.captureRetirementEvidence(ctx)
}

func (s *Store) captureRetirementEvidence(ctx context.Context) ([]byte, string, error) {
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	options := &sql.TxOptions{ReadOnly: true}
	if s.postgres {
		options.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.reader.BeginTx(ctx, options)
	if err != nil {
		return nil, "", dbError(ctx, err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT "dataset_id" FROM "tessera_retirements"`)
	if err != nil {
		return nil, "", dbError(ctx, err)
	}
	held := map[string]bool{}
	size := 0
	add := func(id string) bool {
		if id == "" || len(id) > 1024 {
			return false
		}
		if !held[id] {
			size += len(id) + 8
			held[id] = true
		}
		return size <= maxEvidenceBytes
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		if !add(id) {
			err = store.Refuse("limit_exceeded")
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, "", dbError(ctx, err)
	}
	_ = tx.Rollback()
	f, err := os.Open(s.path + ".retired")
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(io.LimitReader(f, maxEvidenceBytes+1))
		f.Close()
	}
	if err != nil && !os.IsNotExist(err) || len(raw) > maxEvidenceBytes {
		return nil, "", store.Refuse("dataset_unavailable")
	}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var id string
		if json.Unmarshal(line, &id) != nil || !add(id) {
			return nil, "", store.Refuse("dataset_unavailable")
		}
	}
	ids := make([]string, 0, len(held))
	for id := range held {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out bytes.Buffer
	for _, id := range ids {
		b, _ := json.Marshal(id)
		out.Write(b)
		out.WriteByte('\n')
	}
	if out.Len() > maxEvidenceBytes {
		return nil, "", store.Refuse("limit_exceeded")
	}
	// Retain the union independently for restore followed by later exports, even
	// when SQL no longer contains a historical erased identity.
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".retired-")
	if err != nil {
		return nil, "", store.Refuse("dataset_unavailable")
	}
	defer os.Remove(temp.Name())
	_, err = temp.Write(out.Bytes())
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp.Name(), s.path+".retired")
	}
	if err == nil {
		err = syncDirectory(filepath.Dir(s.path))
	}
	if err != nil {
		return nil, "", store.Refuse("dataset_unavailable")
	}
	label, err := cursorToken()
	return out.Bytes(), label, err
}
