package sqlengine

import (
	"bytes"
	"path/filepath"
	"testing"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestOwnerCannotAttachFromAnotherDataset(t *testing.T) {
	s, _, source := fixture(t)
	ctx := t.Context()
	if err := s.EnableContent(ctx, filepath.Join(t.TempDir(), "bytes")); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx, owner, source)
	if err != nil {
		t.Fatal(err)
	}
	content := info.ContentStoreID
	value, err := s.ContentControl(ctx, owner, "begin", content, "",
		[]byte(`{"operation_id":"cross-dataset","content_type":"application/octet-stream","byte_count":0,"maximum_bytes":0}`))
	upload := contentMap(t, value, err)["upload_id"].(string)
	value, err = s.ContentControl(ctx, owner, "commit", content, upload, nil)
	item := contentMap(t, value, err)["item_id"].(string)
	const typ = "test.media_owner"
	created, err := s.CreateDataset(ctx, owner, "Other dataset", p.Limits{}, []p.Meta{
		{ID: typ, Fields: []p.Field{{ID: "media", MetaID: typ, Type: p.MediaType}}},
	}, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := p.NewValue(p.MediaType, content+"//"+item)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Transact(ctx, owner, created.DatasetID, func(tx store.Tx) error {
		_, err := tx.Insert(typ, map[string]p.Value{"media": ref})
		return err
	})
	if p.ErrorCode(err) != "permission_denied" {
		t.Fatal("another dataset's item was not refused", err)
	}
}

func TestContributionErasureClearsPagesOfPreviouslyDeletedRecords(t *testing.T) {
	s, _, dataset := fixture(t)
	ctx := t.Context()
	s.snapshotPages = true
	member := store.Access{Principal: "page-member", Authorization: "member"}
	if err := s.SetAuthorization(ctx, dataset, member, store.Authorization{
		View: store.FullView(), Grants: store.Grants{Write: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(ctx, owner, dataset, func(tx store.Tx) error {
		meta, err := tx.Metadata(typeID)
		if err != nil {
			return err
		}
		meta.Fields = append(meta.Fields, p.Field{ID: "creator", MetaID: typeID, Type: p.TextType, Creator: true})
		_, err = tx.UpdateMetadata(meta, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := s.Transact(ctx, member, dataset, func(tx store.Tx) error {
		for range 2 {
			set, err := tx.Insert(typeID, nil)
			if err != nil {
				return err
			}
			ids = append(ids, set.Records[0].ID.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	page, err := s.BeginPage(ctx, owner, dataset, typeID, p.Query{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(ctx, owner, dataset, func(tx store.Tx) error { return tx.Delete(typeID, ids[0]) }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPage(ctx, owner, dataset, page.Cursor); err != nil {
		t.Fatal("fixture did not retain a readable snapshot", err)
	}
	job, err := s.BeginContributionErasure(ctx, dataset, member.Principal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPage(ctx, owner, dataset, page.Cursor); p.ErrorCode(err) != "invalid_cursor" {
		t.Fatal("erasure start retained a readable snapshot", err)
	}
	during, err := s.BeginPage(ctx, owner, dataset, typeID, p.Query{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(ctx, owner, dataset, func(tx store.Tx) error { return tx.Delete(typeID, ids[1]) }); err != nil {
		t.Fatal(err)
	}
	if done, err := s.StepContributionErasure(ctx, dataset, job, 1); err != nil || !done {
		t.Fatal(done, err)
	}
	if _, err = s.NextPage(ctx, owner, dataset, during.Cursor); p.ErrorCode(err) != "invalid_cursor" {
		t.Fatal("empty erasure step retained a readable snapshot", err)
	}
	later, err := s.BeginPage(ctx, owner, dataset, typeID, p.Query{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if retried, err := s.BeginContributionErasure(ctx, dataset, member.Principal); err != nil || retried != job {
		t.Fatal(retried, err)
	}
	if _, err = s.NextPage(ctx, owner, dataset, later.Cursor); err != nil {
		t.Fatal("completed erasure replay invalidated a new snapshot", err)
	}
}

func TestContributionFenceAllowsRestoringRetiredDataset(t *testing.T) {
	s, _, dataset := fixture(t)
	ctx := t.Context()
	other, err := s.CreateDataset(ctx, owner, "Surviving dataset", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "before.enc")
	key := bytes.Repeat([]byte{32}, 32)
	if err = s.Backup(ctx, archive, key); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginContributionErasure(ctx, dataset, "fixture-principal"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthorization(ctx, dataset, owner, store.Authorization{
		View: store.FullView(), Grants: store.Grants{Read: true, Erase: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Erase(ctx, owner, dataset, dataset); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "restored.db")
	if err = RestoreBackup(ctx, archive, destination, "", s.path+".retired", key); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, destination, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err = restored.Info(ctx, owner, other.DatasetID); err != nil {
		t.Fatal("restore lost the unaffected dataset", err)
	}
	if _, err = restored.Info(ctx, owner, dataset); p.ErrorCode(err) != "not_found" {
		t.Fatal("restore revived the retired dataset", err)
	}
}
