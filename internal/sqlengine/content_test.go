package sqlengine

import (
	"bytes"
	"context"
	"encoding/json"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func contentMap(t *testing.T, value any, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}
func TestContentImportAttachmentAndRetirement(t *testing.T) {
	for _, mode := range []string{"shared", "managed"} {
		t.Run(mode, func(t *testing.T) { checkContentAttachmentRetirement(t, mode == "managed") })
	}
}

func checkContentAttachmentRetirement(t *testing.T, typed bool) {
	s, source, dataset := fixture(t)
	if err := s.EnableContent(t.Context(), filepath.Join(t.TempDir(), "bytes")); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(t.Context(), owner, sole(t, s))
	if err != nil {
		t.Fatal(err)
	}
	contentID := info.ContentStoreID
	if contentID == "" {
		t.Fatal("no content store")
	}
	request := json.RawMessage(`{"operation_id":"test-import","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":10}`)
	value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", request)
	opened := contentMap(t, value, err)
	upload := opened["upload_id"].(string)
	token := strings.TrimPrefix(opened["url"].(string), "/v1/transfers/")
	if _, err = s.ContentControl(t.Context(), owner, "commit", contentID, upload, nil); p.ErrorCode(err) != "conflict" {
		t.Fatal("truncated commit", err)
	}
	for i := 0; i < 2; i++ {
		if _, err = s.ContentTransfer(t.Context(), token, "PATCH", "bytes 0-2/3", bytes.NewReader([]byte("abc"))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.ContentTransfer(t.Context(), token, "PATCH", "bytes 0-2/3", bytes.NewReader([]byte("xyz"))); p.ErrorCode(err) != "conflict" {
		t.Fatal("overwrote committed bytes", err)
	}
	value, err = s.ContentControl(t.Context(), owner, "commit", contentID, upload, nil)
	committed := contentMap(t, value, err)
	item := committed["item_id"].(string)
	value, err = s.ContentControl(t.Context(), owner, "begin", contentID, "", request)
	replay := contentMap(t, value, err)
	if replay["item_id"] != item || replay["state"] != "committed" {
		t.Fatal("lost response duplicated import")
	}
	if err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		_, e := tx.CreateMetadata(p.Meta{ID: "test.media", Fields: []p.Field{{ID: "photo", MetaID: "test.media", Type: p.MediaType},
			{ID: "membership", MetaID: "test.media", Type: p.TextType, Collection: true}}})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if typed {
		if err := s.InstallManagedTypes(t.Context(), owner, dataset, []string{"test.media"}); err != nil {
			t.Fatal(err)
		}
	}
	ref, _ := p.NewValue(p.MediaType, contentID+"//"+item)
	member, _ := p.NewValue(p.TextType, "alpha")
	var object string
	if err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		r, e := tx.Insert("test.media", map[string]p.Value{"photo": ref, "membership": member})
		if e == nil {
			object = r.Records[0].ID.ID
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	guest := store.Access{Principal: "guest", Authorization: "reader"}
	if err = s.SetAuthorization(t.Context(), sole(t, s), guest, store.Authorization{Grants: store.Grants{Read: true}, View: store.SharedView(nil, store.ObjectKey{TypeID: "test.media", ObjectID: object})}); err != nil {
		t.Fatal(err)
	}
	value, err = s.ContentControl(t.Context(), guest, "resolve", contentID, item, []byte(`{"representation":"original"}`))
	resolved := contentMap(t, value, err)
	read, err := s.ContentTransfer(t.Context(), strings.TrimPrefix(resolved["url"].(string), "/v1/transfers/"), "GET", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(read.Body)
	read.Body.Close()
	if err != nil || string(data) != "abc" {
		t.Fatal("wrong bytes", err)
	}
	if _, err = s.ContentControl(t.Context(), guest, "resolve", contentID, "unknown", []byte(`{"representation":"original"}`)); p.ErrorCode(err) != "unavailable" {
		t.Fatal("item existence leaked", err)
	}
	install := func(collection string, cut uint64, insert bool) {
		t.Helper()
		scopes := []store.CollectionScope{{TypeID: "test.media", ID: collection}}
		var readScopes, insertScopes []store.CollectionScope
		if insert {
			insertScopes = scopes
		} else {
			readScopes = scopes
		}
		view, err := store.SharedView(nil).WithCollections(readScopes, insertScopes)
		if err != nil {
			t.Fatal(err)
		}
		grant := store.Authorization{View: view.BeforeSequence(cut), Grants: store.Grants{Read: !insert, Insert: insert}}
		if err = s.SetAuthorization(t.Context(), dataset, guest, grant); err != nil {
			t.Fatal(err)
		}
	}
	resolve := func(engine *Store) error {
		_, err := engine.ContentControl(t.Context(), guest, "resolve", contentID, item, []byte(`{"representation":"original"}`))
		return err
	}
	for _, hidden := range []struct {
		id  string
		cut uint64
	}{{"beta", 0}, {"alpha", 1}} {
		install(hidden.id, hidden.cut, false)
		if err = resolve(s); p.ErrorCode(err) != "unavailable" {
			t.Fatal("collection view disclosed bytes outside its scope", err)
		}
	}
	install("alpha", 0, true)
	if err = s.Transact(t.Context(), guest, dataset, func(tx store.Tx) error {
		_, err := tx.Insert("test.media", map[string]p.Value{"photo": ref, "membership": member})
		return err
	}); p.ErrorCode(err) != "permission_denied" {
		t.Fatal("scoped insert attached content", err)
	}
	install("alpha", 0, false)
	if err = s.Read(t.Context(), guest, dataset, func(tx store.Tx) error {
		set, err := tx.Query("test.media", p.Query{}, 10)
		if err == nil && (len(set.Records) != 1 || set.Records[0].ID.ID != object) {
			t.Fatal("collection query lost its attached record")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err = resolve(s); err != nil {
		t.Fatal("a collection reader could not resolve attached bytes", err)
	}
	dir := t.TempDir()
	archive, destination, files := filepath.Join(dir, "snapshot.enc"), filepath.Join(dir, "restored.db"), filepath.Join(dir, "bytes")
	key := make([]byte, 32)
	if err = s.Backup(t.Context(), archive, key); err != nil {
		t.Fatal(err)
	}
	if err = RestoreBackup(t.Context(), archive, destination, files, source+".retired", key); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(t.Context(), destination, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.EnableContent(t.Context(), files); err != nil {
		t.Fatal(err)
	}
	if err = resolve(restored); err != nil {
		t.Fatal("restore lost collection access to attached bytes", err)
	}
	if err = s.SetAuthorization(t.Context(), sole(t, s), owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	value, err = s.ContentControl(t.Context(), owner, "erase", contentID, "", []byte(`{"confirm":"`+contentID+`"}`))
	replacement := contentMap(t, value, err)
	if replacement["store_id"] == contentID {
		t.Fatal("content identity reused")
	}
	if err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		r, e := tx.Object("test.media", object, nil)
		if e == nil && r.Records[0].Values[0].Payload() != contentID+"//"+item {
			t.Fatal("stored reference rewritten")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ContentControl(t.Context(), owner, "resolve", contentID, item, []byte(`{"representation":"original"}`)); p.ErrorCode(err) != "not_found" {
		t.Fatal("retired store redirected", err)
	}
	if err = s.CleanupContent(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestContentStreamingLimitsAndAtomicAttachmentFailure(t *testing.T) {
	s, _, _ := fixture(t)
	if err := s.EnableContent(t.Context(), filepath.Join(t.TempDir(), "bytes")); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(t.Context(), owner, sole(t, s))
	if err != nil {
		t.Fatal(err)
	}
	begin := func(op string) map[string]any {
		v, e := s.ContentControl(t.Context(), owner, "begin", info.ContentStoreID, "", []byte(`{"operation_id":"`+op+`","content_type":"application/octet-stream","maximum_bytes":1048576}`))
		return contentMap(t, v, e)
	}
	opened := begin("unknown-total")
	token := strings.TrimPrefix(opened["url"].(string), "/v1/transfers/")
	if _, err = s.ContentTransfer(t.Context(), token, "PATCH", "bytes 0-2/*", bytes.NewReader([]byte("abc"))); p.ErrorCode(err) != "limit_exceeded" {
		t.Fatal("nonfinal undersized chunk accepted", err)
	}
	chunk := bytes.Repeat([]byte{'x'}, int(minChunkBytes))
	if _, err = s.ContentTransfer(t.Context(), token, "PATCH", "bytes 0-262143/*", bytes.NewReader(chunk)); err != nil {
		t.Fatal(err)
	}
	resumed := begin("unknown-total")
	if resumed["received_bytes"] != int64(minChunkBytes) {
		t.Fatal("resume checkpoint lost")
	}
	if _, err = s.ContentControl(t.Context(), owner, "commit", info.ContentStoreID, opened["upload_id"].(string), nil); p.ErrorCode(err) != "conflict" {
		t.Fatal("unknown total committed", err)
	}
	if _, err = s.ContentTransfer(t.Context(), token, "PATCH", "bytes 262144-262146/262147", bytes.NewReader([]byte("end"))); err != nil {
		t.Fatal(err)
	}
	value, err := s.ContentControl(t.Context(), owner, "commit", info.ContentStoreID, opened["upload_id"].(string), nil)
	item := contentMap(t, value, err)["item_id"].(string)
	ref, _ := p.NewValue(p.MediaType, info.ContentStoreID+"//"+item)
	err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: "test.rollback_media", Fields: []p.Field{{ID: "photo", MetaID: "test.rollback_media", Type: p.MediaType}}}); e != nil {
			return e
		}
		if _, e := tx.Insert("test.rollback_media", map[string]p.Value{"photo": ref}); e != nil {
			return e
		}
		return store.Refuse("conflict")
	})
	if p.ErrorCode(err) != "conflict" {
		t.Fatal(err)
	}
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM "tessera_attachments"`).Scan(&n); err != nil || n != 0 {
		t.Fatal("record failure left attachment", err)
	}
}

// contentFixture opens a store with content enabled and returns its ID.
func contentFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	s, path, dataset := fixture(t)
	if err := s.EnableContent(t.Context(), filepath.Join(filepath.Dir(path), "bytes")); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(t.Context(), owner, sole(t, s))
	if err != nil || info.ContentStoreID == "" {
		t.Fatal("no content store", err)
	}
	return s, info.ContentStoreID, dataset
}

// Review finding 7, as resolved: an operation ID belongs to one logical store
// and to the principal and application authorization that claimed it, and the
// parameters it was reserved with are part of its identity.
func TestImportIdentityIsScopedAndFixed(t *testing.T) {
	s, contentID, _ := contentFixture(t)
	const base = `{"operation_id":"scoped","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":10}`
	if _, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", json.RawMessage(base)); err != nil {
		t.Fatal(err)
	}
	// Another principal, and the same principal through another application
	// authorization, learn nothing: not the import, not its item identity.
	stranger := store.Access{Principal: "other", Authorization: "app"}
	sameDifferentApp := store.Access{Principal: owner.Principal, Authorization: "another-app"}
	for _, who := range []store.Access{stranger, sameDifferentApp} {
		if err := s.SetAuthorization(t.Context(), sole(t, s), who, store.Authorization{Grants: store.Grants{Read: true, Write: true}, View: store.FullView()}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ContentControl(t.Context(), who, "begin", contentID, "", json.RawMessage(base)); p.ErrorCode(err) != "permission_denied" {
			t.Fatal("an operation ID was resumable by someone who did not claim it", who.Authorization, err)
		}
	}
	// The ceiling is fixed at reservation. Raising it is a conflict even though
	// both ceilings exceed the received bytes and the store limit, because a
	// resume must not silently revise a bound another transfer is enforcing.
	for _, changed := range []string{
		`{"operation_id":"scoped","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":20}`,
		`{"operation_id":"scoped","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":4}`,
		`{"operation_id":"scoped","content_type":"text/plain","byte_count":3,"maximum_bytes":10}`,
		`{"operation_id":"scoped","content_type":"application/octet-stream","byte_count":4,"maximum_bytes":10}`,
		// Omitting byte_count and declaring it are different requests.
		`{"operation_id":"scoped","content_type":"application/octet-stream","maximum_bytes":10}`,
	} {
		if _, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", json.RawMessage(changed)); p.ErrorCode(err) != "conflict" {
			t.Fatal("changed reservation parameters were accepted on replay", changed, err)
		}
	}
}

// Abort and reclamation are terminal. The operation ID keeps answering for its
// old attempt rather than quietly becoming a fresh import, and it keeps doing
// so after a restart.
func TestTerminatedImportsStayTerminatedAcrossRestart(t *testing.T) {
	s, path, _ := fixture(t)
	directory := filepath.Join(filepath.Dir(path), "bytes")
	if err := s.EnableContent(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(t.Context(), owner, sole(t, s))
	if err != nil {
		t.Fatal(err)
	}
	contentID := info.ContentStoreID
	const request = `{"operation_id":"terminated","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":10}`
	value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", json.RawMessage(request))
	opened := contentMap(t, value, err)
	upload := opened["upload_id"].(string)
	token := strings.TrimPrefix(opened["url"].(string), "/v1/transfers/")
	if _, err = s.ContentControl(t.Context(), owner, "abort", contentID, upload, nil); err != nil {
		t.Fatal(err)
	}
	// An unchanged replay reports the termination; it does not open a new one.
	if _, err = s.ContentControl(t.Context(), owner, "begin", contentID, "", json.RawMessage(request)); p.ErrorCode(err) != "expired" {
		t.Fatal("an aborted operation ID opened a fresh import", err)
	}
	// Conflicting parameters are still a conflict, not an expiry.
	conflicting := `{"operation_id":"terminated","content_type":"text/plain","byte_count":3,"maximum_bytes":10}`
	if _, err = s.ContentControl(t.Context(), owner, "begin", contentID, "", json.RawMessage(conflicting)); p.ErrorCode(err) != "conflict" {
		t.Fatal("conflicting replay of a terminated import was not a conflict", err)
	}
	// A transfer URI issued before the abort cannot revive the staging bytes,
	// and the abandoned upload cannot be committed.
	if _, err = s.ContentTransfer(t.Context(), token, "PATCH", "bytes 0-2/3", bytes.NewReader([]byte("abc"))); err == nil {
		t.Fatal("an aborted import accepted bytes")
	}
	if _, err = s.ContentControl(t.Context(), owner, "commit", contentID, upload, nil); p.ErrorCode(err) != "expired" {
		t.Fatal("an aborted import was committable", err)
	}
	// The tombstone is durable: reopening the database keeps the same answers.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if err = reopened.EnableContent(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.ContentControl(t.Context(), owner, "begin", contentID, "", json.RawMessage(request)); p.ErrorCode(err) != "expired" {
		t.Fatal("restart forgot that an operation ID had been aborted", err)
	}
}

// An empty item is declared, never inferred. Zero is the one total no
// completing range can establish, so an interrupted unknown-length transfer
// must not be mistaken for a successful empty one.
func TestEmptyImportsMustBeDeclared(t *testing.T) {
	s, contentID, _ := contentFixture(t)
	declared := `{"operation_id":"empty-declared","content_type":"text/plain","byte_count":0,"maximum_bytes":0}`
	value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", json.RawMessage(declared))
	opened := contentMap(t, value, err)
	upload := opened["upload_id"].(string)
	// It commits with no transfer at all.
	value, err = s.ContentControl(t.Context(), owner, "commit", contentID, upload, nil)
	committed := contentMap(t, value, err)
	item, ok := committed["item_id"].(string)
	if !ok || item == "" {
		t.Fatal("a declared empty import did not commit")
	}
	// Replay returns the one identity rather than a second empty item.
	value, err = s.ContentControl(t.Context(), owner, "begin", contentID, "", json.RawMessage(declared))
	replay := contentMap(t, value, err)
	if replay["state"] != "committed" || replay["item_id"] != item {
		t.Fatal("a declared empty import replayed as a different item", replay)
	}
	// An unknown-length import has no total until a range completes it, and no
	// nonempty range can complete an empty item, so it is never committable.
	unknown := `{"operation_id":"empty-unknown","content_type":"text/plain","maximum_bytes":10}`
	value, err = s.ContentControl(t.Context(), owner, "begin", contentID, "", json.RawMessage(unknown))
	open := contentMap(t, value, err)
	if _, err = s.ContentControl(t.Context(), owner, "commit", contentID, open["upload_id"].(string), nil); p.ErrorCode(err) != "conflict" {
		t.Fatal("an unknown-length import committed as empty", err)
	}
	// And it never became readable under any item identity.
	if _, err = s.ContentControl(t.Context(), owner, "resolve", contentID, open["upload_id"].(string), json.RawMessage(`{"representation":"original"}`)); err == nil {
		t.Fatal("an uncommitted import was readable")
	}
}

// Three requirements whose mechanisms existed but which nothing exercised: an
// object identity is never reused after deletion, a transfer URI is bounded by
// its advertised lifetime, and a URI issued before an erase cannot write bytes
// back into the store it was issued for.
func TestIdentitiesAreNeverReusedAndURIsExpire(t *testing.T) {
	s, _, dataset := contentFixture(t)

	// Deleting the highest object must not let the next insert take its ID.
	first, err := s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody))
	if err != nil {
		t.Fatal(err)
	}
	deleted := identity(t, first)
	if err = s.Transact(t.Context(), owner, sole(t, s), func(tx store.Tx) error { return tx.Delete(typeID, deleted) }); err != nil {
		t.Fatal(err)
	}
	next, err := s.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody))
	if err != nil {
		t.Fatal(err)
	}
	if identity(t, next) == deleted {
		t.Fatal("an object identity was reused after deletion")
	}
	// The counter is not rewound by a restart either.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), filepath.Join(filepath.Dir(s.path), "dataset.db"), p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	after, err := reopened.Mutate(t.Context(), owner, mutation(t, dataset, "", insertBody))
	if err != nil {
		t.Fatal(err)
	}
	if identity(t, after) == deleted || identity(t, after) == identity(t, next) {
		t.Fatal("a restart rewound the object identity counter")
	}
}

// An import authorized before an erase must not be able to write bytes into
// the store afterwards, and a retired store's read URIs stop working. This is
// the case the content protocol names explicitly, because expiry alone does
// not cover a URI issued moments before.
func TestURIsIssuedBeforeAnEraseCannotWriteBack(t *testing.T) {
	s, contentID, _ := contentFixture(t)
	request := json.RawMessage(`{"operation_id":"pre-erase","content_type":"text/plain","byte_count":3,"maximum_bytes":3}`)
	value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", request)
	opened := contentMap(t, value, err)
	transfer := strings.TrimPrefix(opened["url"].(string), "/v1/transfers/")

	// A second item is committed so there is a read URI outstanding too.
	other := json.RawMessage(`{"operation_id":"pre-erase-read","content_type":"text/plain","byte_count":3,"maximum_bytes":3}`)
	value, err = s.ContentControl(t.Context(), owner, "begin", contentID, "", other)
	staged := contentMap(t, value, err)
	if _, err = s.ContentTransfer(t.Context(), strings.TrimPrefix(staged["url"].(string), "/v1/transfers/"), "PATCH", "bytes 0-2/3", bytes.NewReader([]byte("abc"))); err != nil {
		t.Fatal(err)
	}
	value, err = s.ContentControl(t.Context(), owner, "commit", contentID, staged["upload_id"].(string), nil)
	item := contentMap(t, value, err)["item_id"].(string)
	value, err = s.ContentControl(t.Context(), owner, "resolve", contentID, item, json.RawMessage(`{"representation":"original"}`))
	read := strings.TrimPrefix(contentMap(t, value, err)["url"].(string), "/v1/transfers/")

	if err = s.SetAuthorization(t.Context(), sole(t, s), owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ContentControl(t.Context(), owner, "erase", contentID, "", json.RawMessage(`{"confirm":"`+contentID+`"}`)); err != nil {
		t.Fatal(err)
	}
	// Neither URI works against the retired store: the write cannot resurrect
	// bytes under a deleted key, and the read cannot serve them.
	if _, err = s.ContentTransfer(t.Context(), transfer, "PATCH", "bytes 0-2/3", bytes.NewReader([]byte("abc"))); err == nil {
		t.Fatal("a transfer URI issued before the erase wrote bytes back")
	}
	if _, err = s.ContentTransfer(t.Context(), read, "GET", "", nil); err == nil {
		t.Fatal("a read URI issued before the erase still served bytes")
	}
}

// The record protocol requires that two datasets never name one logical content
// store: an item cannot both survive for one and be destroyed for the other.
// While a database held one dataset this was structurally unreachable and
// therefore untested. It is reachable now, so it is guarded and tested.
func TestTwoDatasetsNeverNameOneContentStore(t *testing.T) {
	s, _, first := fixture(t)
	directory := filepath.Join(t.TempDir(), "bytes")
	if err := s.EnableContent(t.Context(), directory); err != nil {
		t.Fatal(err)
	}
	other := store.Access{Principal: "other", Authorization: "other"}
	made, err := s.CreateDataset(t.Context(), other, "Second quarter", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	// A creation reports an identity; its content store is discovery state and
	// is asked for separately.
	second, err := s.Info(t.Context(), other, made.DatasetID)
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := s.Info(t.Context(), owner, first)
	if err != nil {
		t.Fatal(err)
	}
	if firstInfo.ContentStoreID == "" || second.ContentStoreID == "" {
		t.Fatal("a dataset was left without a content store", firstInfo.ContentStoreID, second.ContentStoreID)
	}
	if firstInfo.ContentStoreID == second.ContentStoreID {
		t.Fatal("two datasets named one content store", second.ContentStoreID)
	}
	// The association is what decides ownership, so neither caller can reach the
	// other's store even though both sit over one byte directory. A store whose
	// dataset is not the caller's is unavailable, exactly as that dataset is,
	// and a store whose dataset was retired reveals nothing at all.
	//
	// This asserted permission_denied until a review pointed out what the two
	// answers together amount to: a store that does not exist answers
	// unavailable, so refusing a store that does tells the caller which of the
	// two they named. That is the difference the rule exists to withhold, and
	// the comment here used to invoke the rule while contradicting it.
	for _, reach := range []struct {
		who store.Access
		id  string
	}{{other, firstInfo.ContentStoreID}, {owner, second.ContentStoreID}} {
		if _, err = s.ContentControl(t.Context(), reach.who, "inspect", reach.id, "i-absent", nil); p.ErrorCode(err) != "unavailable" {
			t.Fatal("a caller reached another dataset's content store", err)
		}
	}
	// And a store that names nothing at all answers the same way, which is the
	// property the two answers have to share to say nothing.
	if _, err = s.ContentControl(t.Context(), owner, "inspect", "c-"+strings.Repeat("a", 43), "i-absent", nil); p.ErrorCode(err) != "unavailable" {
		t.Fatal("a store identifier naming nothing was distinguishable from one naming somebody else", err)
	}
	// A listing holds one store, the caller's own.
	stores := func(a store.Access) []any {
		t.Helper()
		value, err := s.ContentControl(t.Context(), a, "list", "", "", nil)
		return contentMap(t, value, err)["stores"].([]any)
	}
	for _, c := range []struct {
		access store.Access
		id     string
	}{{owner, firstInfo.ContentStoreID}, {other, second.ContentStoreID}} {
		held := stores(c.access)
		if len(held) != 1 {
			t.Fatal("a content listing did not hold exactly the caller's store", held)
		}
		if held[0].(map[string]any)["id"] != c.id {
			t.Fatal("a content listing named another dataset's store", held[0])
		}
	}
	// Erasing one dataset retires its store and leaves the other's alone.
	if err = s.SetAuthorization(t.Context(), second.ID, other, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Erase(t.Context(), other, second.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	after, err := s.Info(t.Context(), owner, first)
	if err != nil || after.ContentStoreID != firstInfo.ContentStoreID {
		t.Fatal("erasing one dataset disturbed the other's content store", after.ContentStoreID, err)
	}
	if held := stores(owner); len(held) != 1 || held[0].(map[string]any)["id"] != firstInfo.ContentStoreID {
		t.Fatal("the surviving store left the listing", held)
	}
}

// heldReader stands in for a client that has opened a transfer and is sending
// its bytes slowly. It blocks on the first read until released, and reports
// when it has been entered so a test can be sure the transfer is underway.
type heldReader struct {
	entered chan struct{}
	release chan struct{}
	inner   io.Reader
	waited  bool
}

func (h *heldReader) Read(p []byte) (int, error) {
	if !h.waited {
		h.waited = true
		close(h.entered)
		<-h.release
	}
	return h.inner.Read(p)
}

// One slow uploader used to hold a process-wide mutex for as long as it chose
// to keep sending, and every other transfer on the server waited behind it —
// other datasets included. The lock belongs around the filesystem checkpoint,
// not around a read from the network.
func TestASlowUploadDoesNotStopEveryOtherTransfer(t *testing.T) {
	s, contentID, _ := contentFixture(t)
	begin := func(operation string) string {
		t.Helper()
		request := json.RawMessage(`{"operation_id":"` + operation + `","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":10}`)
		value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", request)
		return strings.TrimPrefix(contentMap(t, value, err)["url"].(string), "/v1/transfers/")
	}
	slow, quick := begin("slow"), begin("quick")

	held := &heldReader{entered: make(chan struct{}), release: make(chan struct{}), inner: bytes.NewReader([]byte("abc"))}
	stalled := make(chan error, 1)
	go func() {
		_, err := s.ContentTransfer(context.Background(), slow, "PATCH", "bytes 0-2/3", held)
		stalled <- err
	}()
	<-held.entered

	// The second transfer must land while the first is still sending. It is a
	// separate import, so nothing but the lock could make it wait.
	done := make(chan error, 1)
	go func() {
		_, err := s.ContentTransfer(context.Background(), quick, "PATCH", "bytes 0-2/3", bytes.NewReader([]byte("xyz")))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("the unrelated transfer failed", err)
		}
	case <-time.After(5 * time.Second):
		close(held.release)
		t.Fatal("an unrelated transfer waited on a client that had not finished sending")
	}
	close(held.release)
	if err := <-stalled; err != nil {
		t.Fatal("the slow transfer did not complete once its bytes arrived", err)
	}
}

// A media reference names a store and an item, and nothing stops one crossing
// a dataset boundary: a record in B may reference an item in A's store. The
// attachment rows that record writes are B's, and the visibility check read
// them without asking whose they were — so a reader sharing a type in A had
// that allowance applied to an object in B, and the bytes came back.
//
// Reported by review. My own first attempt put the item in B's store, which
// fails earlier and proves nothing about this.
func TestAShareOfOneDatasetIsNotAKeyToAnothersReferences(t *testing.T) {
	s, storeA, datasetA := contentFixture(t)
	other := store.Access{Principal: "other", Authorization: "other"}
	made, err := s.CreateDataset(t.Context(), other, "Somebody else", p.Limits{}, nil, "", [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	datasetB := made.DatasetID
	// The writer legitimately holds both: crossing the boundary on the way in
	// requires write and a full view of the store's dataset.
	if err = s.SetAuthorization(t.Context(), datasetA, other,
		store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true}, View: store.FullView()}); err != nil {
		t.Fatal(err)
	}

	// One item, in A's store.
	request := json.RawMessage(`{"operation_id":"crossing","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":10}`)
	value, err := s.ContentControl(t.Context(), owner, "begin", storeA, "", request)
	opened := contentMap(t, value, err)
	upload := opened["upload_id"].(string)
	if _, err = s.ContentTransfer(t.Context(), strings.TrimPrefix(opened["url"].(string), "/v1/transfers/"),
		"PATCH", "bytes 0-2/3", bytes.NewReader([]byte("key"))); err != nil {
		t.Fatal(err)
	}
	value, err = s.ContentControl(t.Context(), owner, "commit", storeA, upload, nil)
	item := contentMap(t, value, err)["item_id"].(string)

	// Referenced only from B. A holds no record referring to it at all.
	ref, _ := p.NewValue(p.MediaType, storeA+"//"+item)
	if err = s.Transact(t.Context(), other, datasetB, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: "test.media", Fields: []p.Field{{ID: "photo", MetaID: "test.media", Type: p.MediaType}}}); e != nil {
			return e
		}
		_, e := tx.Insert("test.media", map[string]p.Value{"photo": ref})
		return e
	}); err != nil {
		t.Fatal(err)
	}

	// A reader who shares a type in A, and holds nothing whatever in B.
	guest := store.Access{Principal: "guest", Authorization: "share"}
	if err = s.SetAuthorization(t.Context(), datasetA, guest,
		store.Authorization{Grants: store.Grants{Read: true}, View: store.SharedView([]string{"test.media"})}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Info(t.Context(), guest, datasetB); p.ErrorCode(err) != "unavailable" {
		t.Fatal("the fixture's second dataset is not actually out of the guest's reach", err)
	}
	for _, action := range []string{"inspect", "resolve"} {
		if _, err = s.ContentControl(t.Context(), guest, action, storeA, item,
			[]byte(`{"representation":"original"}`)); p.ErrorCode(err) != "unavailable" {
			t.Fatal("a share of one dataset reached an item held only by another's record", action, err)
		}
	}
}

// Changing one record used to rewrite every media association the dataset
// had: a delete of all of them and a row back for each, inside the write
// transaction. A record with no attachment paid for the attachments of every
// record beside it.
//
// Observed through SQLite's rowid, which changes when a row is reinserted and
// does not when the row is left alone. That distinguishes "wrote the same
// values again" from "did not write", which counting rows cannot.
func TestChangingOneRecordDoesNotRewriteEveryAssociation(t *testing.T) {
	s, contentID, dataset := contentFixture(t)
	item := func(operation string) string {
		t.Helper()
		request := json.RawMessage(`{"operation_id":"` + operation + `","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":10}`)
		value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", request)
		opened := contentMap(t, value, err)
		if _, err = s.ContentTransfer(t.Context(), strings.TrimPrefix(opened["url"].(string), "/v1/transfers/"),
			"PATCH", "bytes 0-2/3", bytes.NewReader([]byte("abc"))); err != nil {
			t.Fatal(err)
		}
		value, err = s.ContentControl(t.Context(), owner, "commit", contentID, opened["upload_id"].(string), nil)
		return contentMap(t, value, err)["item_id"].(string)
	}
	first, second := item("one"), item("two")

	var subject, neighbour string
	if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		if _, e := tx.CreateMetadata(p.Meta{ID: "test.media", Fields: []p.Field{
			{ID: "photo", MetaID: "test.media", Type: p.MediaType},
			{ID: "n", MetaID: "test.media", Type: p.IntegerType},
		}}); e != nil {
			return e
		}
		for who, itemID := range map[string]string{"subject": first, "neighbour": second} {
			ref, e := p.NewValue(p.MediaType, contentID+"//"+itemID)
			if e != nil {
				return e
			}
			count, e := p.NewValue(p.IntegerType, int64(1))
			if e != nil {
				return e
			}
			r, e := tx.Insert("test.media", map[string]p.Value{"photo": ref, "n": count})
			if e != nil {
				return e
			}
			if who == "subject" {
				subject = r.Records[0].ID.ID
			} else {
				neighbour = r.Records[0].ID.ID
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rowids := func() map[string]int64 {
		t.Helper()
		rows, err := s.reader.QueryContext(t.Context(), `SELECT "object_id",rowid FROM "tessera_attachments" WHERE "dataset_id"=$1`, dataset)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := map[string]int64{}
		for rows.Next() {
			var id string
			var at int64
			if err = rows.Scan(&id, &at); err != nil {
				t.Fatal(err)
			}
			found[id] = at
		}
		return found
	}
	// A row belonging to another dataset, parked at a high rowid. Without it
	// the table empties on a wholesale rebuild and SQLite hands the same
	// rowids straight back, so the rewrite would leave no trace to find.
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO "tessera_attachments" (rowid,"dataset_id","object_id","type_id","field_id","store_id","item_id") VALUES (1000,'s-elsewhere','o','t','f','c','i')`); err != nil {
		t.Fatal(err)
	}
	before := rowids()
	if len(before) != 2 {
		t.Fatal("fixture should hold two associations", before)
	}

	// An unrelated field on one record. Neither association changes.
	if err := s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		value, e := p.NewValue(p.IntegerType, int64(2))
		if e != nil {
			return e
		}
		return tx.Update("test.media", subject, map[string]p.Value{"n": value})
	}); err != nil {
		t.Fatal(err)
	}
	after := rowids()
	for id, at := range before {
		if after[id] != at {
			t.Fatal("changing one record rewrote an association it did not touch", id)
		}
	}

	// And the association still follows the record it belongs to: pointing
	// the subject at the other item moves its row and leaves the neighbour's.
	moved, err := p.NewValue(p.MediaType, contentID+"//"+second)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		return tx.Update("test.media", subject, map[string]p.Value{"photo": moved})
	}); err != nil {
		t.Fatal(err)
	}
	changed := rowids()
	if changed[subject] == after[subject] {
		t.Fatal("an association whose item changed was not rewritten")
	}
	if changed[neighbour] != after[neighbour] {
		t.Fatal("changing one association rewrote another")
	}
	var stored string
	if err = s.reader.QueryRowContext(t.Context(), `SELECT "item_id" FROM "tessera_attachments" WHERE "dataset_id"=$1 AND "object_id"=$2`, dataset, subject).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != second {
		t.Fatal("the rewritten association does not name the item the record now holds", stored)
	}

	// Deleting the record takes its association with it.
	if err = s.Transact(t.Context(), owner, dataset, func(tx store.Tx) error {
		return tx.Delete("test.media", subject)
	}); err != nil {
		t.Fatal(err)
	}
	if remaining := rowids(); len(remaining) != 1 || remaining[neighbour] == 0 {
		t.Fatal("deleting a record left or lost the wrong association", remaining)
	}
}

// Bytes erased since a backup was taken are not written back by restoring it,
// whether the content store was erased alone or with its dataset.
func TestRestoreDoesNotWriteBackErasedBytes(t *testing.T) {
	for _, whole := range []bool{false, true} {
		t.Run(map[bool]string{false: "content store", true: "dataset"}[whole], func(t *testing.T) {
			s, contentID, dataset := contentFixture(t)
			path := s.path
			request := json.RawMessage(`{"operation_id":"kept-bytes","content_type":"text/plain","byte_count":6,"maximum_bytes":6}`)
			value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", request)
			opened := contentMap(t, value, err)
			if _, err = s.ContentTransfer(t.Context(), strings.TrimPrefix(opened["url"].(string), "/v1/transfers/"), "PATCH", "bytes 0-5/6", bytes.NewReader([]byte("secret"))); err != nil {
				t.Fatal(err)
			}
			upload := opened["upload_id"].(string)
			if _, err = s.ContentControl(t.Context(), owner, "commit", contentID, upload, nil); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "backup.enc")
			key := bytes.Repeat([]byte{3}, 32)
			if err = s.Backup(t.Context(), archive, key); err != nil {
				t.Fatal(err)
			}
			if err = s.SetAuthorization(t.Context(), dataset, owner, store.Authorization{Grants: store.Grants{Read: true, Write: true, Schema: true, Erase: true}, View: store.FullView()}); err != nil {
				t.Fatal(err)
			}
			if whole {
				_, err = s.Erase(t.Context(), owner, dataset, dataset)
			} else {
				_, err = s.ContentControl(t.Context(), owner, "erase", contentID, "", json.RawMessage(`{"confirm":"`+contentID+`"}`))
			}
			if err != nil {
				t.Fatal(err)
			}
			contentDir := filepath.Join(t.TempDir(), "restored-bytes")
			if err = RestoreBackup(t.Context(), archive, filepath.Join(t.TempDir(), "restored.db"), contentDir, path+".retired", key); err != nil {
				t.Fatal("a backup from before an erasure could not be restored:", err)
			}
			if _, err = os.Stat(filepath.Join(contentDir, upload)); !os.IsNotExist(err) {
				t.Fatal("erased bytes were written back by a restore:", err)
			}
		})
	}
}

// An aborted import's bytes are removed once. Its row stays, as the tombstone
// a replay is answered from, but a later pass has nothing left to do for it;
// and a pass takes a bounded batch, leaving the rest to the next.
func TestAbortedImportsAreCleanedOnceInBatches(t *testing.T) {
	s, contentID, _ := contentFixture(t)
	previous := cleanupBatch
	cleanupBatch = 2
	t.Cleanup(func() { cleanupBatch = previous })
	var uploads []string
	for i := 0; i < 3; i++ {
		request := json.RawMessage(`{"operation_id":"abandoned-` + strconv.Itoa(i) + `","content_type":"text/plain","byte_count":4,"maximum_bytes":4}`)
		value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", request)
		opened := contentMap(t, value, err)
		if _, err = s.ContentTransfer(t.Context(), strings.TrimPrefix(opened["url"].(string), "/v1/transfers/"), "PATCH", "bytes 0-3/4", bytes.NewReader([]byte("abcd"))); err != nil {
			t.Fatal(err)
		}
		uploads = append(uploads, opened["upload_id"].(string))
	}
	if _, err := s.db.Exec(`UPDATE "tessera_imports" SET "state"='aborted'`); err != nil {
		t.Fatal(err)
	}
	uncleaned := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM "tessera_imports" WHERE "state"='aborted' AND "cleaned"=0`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := s.CleanupContent(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := uncleaned(); got != 1 {
		t.Fatal("a pass did not take a bounded batch:", got, "left")
	}
	if err := s.CleanupContent(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := uncleaned(); got != 0 {
		t.Fatal("aborted imports were not all marked cleaned:", got)
	}
	for _, upload := range uploads {
		if _, err := os.Stat(filepath.Join(s.byteDirectory(), upload)); !os.IsNotExist(err) {
			t.Fatal("an aborted import's bytes were not removed:", err)
		}
	}
	var tombstones int
	if err := s.db.QueryRow(`SELECT count(*) FROM "tessera_imports" WHERE "state"='aborted'`).Scan(&tombstones); err != nil || tombstones != 3 {
		t.Fatal("an aborted import's tombstone was removed:", tombstones, err)
	}
}
