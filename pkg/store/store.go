// Package store defines Tessera's authorized transaction boundary. Authentication
// belongs to the host; current authorization belongs beneath every operation.
//
// A Store serves datasets, plural: every operation names the one it acts on, and
// how many a given implementation holds is the implementation's business. The
// durable engines behind pkg/store/sqlite and pkg/store/postgres hold many in
// one file or database; pkg/store/memory holds one.
package store

import (
	"context"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"time"
)

// Access identifies both the principal and its particular application grant.
// It is supplied by trusted authentication code, never decoded from a request.
// Principal stays stable across authorizations and cannot be reassigned to
// another identity, including after an account is deleted.
type Access struct{ Principal, Authorization string }

func (a Access) Valid() bool { return a.Principal != "" && a.Authorization != "" }

type Grants struct {
	ExpiresAt *time.Time `json:"changes_at"`
	Read      bool       `json:"read"`
	Write     bool       `json:"write"`
	Schema    bool       `json:"schema"`
	Erase     bool       `json:"erase"`
	// Insert adds records within the types or collections the view names, and
	// nothing else: no read, no update, no delete, and no continuing authority
	// over what was added, which belongs to the dataset from the moment it
	// commits. It is separate from Write because it is the whole of what a
	// contributor holds, and a contributor is not an owner.
	Insert bool `json:"insert"`
}

type ObjectKey struct{ TypeID, ObjectID string }

// View carries exact type, object and collection scopes. The zero value is
// invalid. Scope descriptors contain data only.
type View struct {
	initialized, full bool
	objects           map[ObjectKey]struct{}
	types             map[string]struct{}
	// inserts are the concrete types an Insert grant may add to. They are
	// not part of what the view shows: adding to a type conveys no sight of
	// it, which is why they are kept apart from types rather than merged in.
	inserts                            map[string]struct{}
	sequenceCut                        uint64
	readCollections, insertCollections []CollectionScope
}

func FullView() View { return View{initialized: true, full: true} }
func ObjectView(keys ...ObjectKey) View {
	v := View{initialized: true, objects: make(map[ObjectKey]struct{}, len(keys))}
	for _, k := range keys {
		v.objects[k] = struct{}{}
	}
	return v
}
func (v View) Valid() bool { return v.initialized }
func (v View) Full() bool  { return v.initialized && v.full && v.sequenceCut == 0 }
func (v View) Allows(k ObjectKey) bool {
	if v.Full() || v.AllowsType(k.TypeID) {
		return true
	}
	_, ok := v.objects[k]
	return v.initialized && ok
}

// Grants must be the current intersection of account and application privileges.
// The host updates or revokes this entry when either privilege set changes.
type Authorization struct {
	Grants Grants
	View   View
}
type Info struct {
	ID string
	// DisplayName is for a person choosing between datasets. It is host-supplied
	// and never a record value, so it carries nothing a caller put in a record.
	DisplayName       string
	ReplacedDatasetID string
	Capabilities      []string
	ContentStoreID    string
	Access            Grants
	Watermark         string
	ViewVersion       string
	SchemaVersion     string
	Limits            p.Limits
}

// ServerScope is the receipt scope for an operation that belongs to the server
// rather than to any dataset. Creation is the only one: it has no dataset to be
// keyed by, and without a scope of its own a caller whose response was lost
// could not discover whether it made a dataset — which is the one thing this
// contract never asks of a caller.
//
// It is deliberately not a dataset identity and can never become one: the
// dataset grammar admits no leading "*", so no minted identity collides with it
// and no caller can address it as a dataset.
const ServerScope = "*server"

// Created is everything a creation reports: the identity of the dataset that
// now exists, and whether this answer came from a retained outcome rather than
// from work done now.
//
// It is deliberately not Info. A replay must be answerable to an owner with no
// grant on what it made — after the grant was withdrawn, after the dataset was
// erased — and building an Info would both require reading the dataset and
// disclose its current bounds, capabilities and versions to a caller whose
// access has ended. Current information comes from discovery instead.
type Created struct {
	DatasetID string
	Replayed  bool
}

// DefaultServerRetentionSeconds is the floor this implementation advertises for
// server-scoped receipts: one week. It is deliberately not a dataset's window.
// A creation's population is one claim per account plus retries, so storage is
// not the constraint; what a short window costs is every correct action, since
// past it :status answers unknown, the contract forbids inferring an outcome
// from elapsed time, and retrying the key makes a second dataset.
const DefaultServerRetentionSeconds = 604800

// ServerScoped is implemented by an engine that retains outcomes belonging to no
// dataset. The window is advertised as a floor and owed on every claim already
// made, so it is read from the engine rather than configured at the edge.
type ServerScoped interface {
	ServerRetentionSeconds() int
}

// Provisioning is implemented by an engine that can make a dataset that was not
// there before. It is not part of Store: the contract describes datasets a
// server already holds and says nothing about how one comes to exist, so an
// engine may reasonably have no answer. The caller becomes the new dataset's
// administrator. Passing zero limits takes the storage's.
//
// A non-empty key makes the creation durably idempotent under ServerScope, and
// binds it to the digest so a replay of a different request is refused rather
// than answered. An empty key is a creation that cannot be recovered, which is
// only safe for composition, where there is no response to lose.
type Provisioning interface {
	// types, when given, are installed in the same transaction that creates
	// the dataset, so a dataset that exists is one that already has them.
	CreateDataset(ctx context.Context, a Access, displayName string, limits p.Limits, types []p.Meta, key string, digest [32]byte) (Created, error)
	// RecoverCreation answers from a retained claim without making one. It
	// exists so a caller can be answered before capability and permission are
	// consulted: a receipt's owner outlives its access, and a repeat is a
	// question about what already happened rather than a request to do more.
	// False means no claim, at which point admitting new work is the question.
	RecoverCreation(ctx context.Context, a Access, key string, digest [32]byte) (Created, bool, error)
}

// Dataset is a dataset as the composition that owns the storage sees it, with
// no caller in the picture. It carries no grants, because there is no caller to
// hold them.
type Dataset struct {
	ID          string
	DisplayName string
}

// Store owns transaction commit and rollback. A callback may compose multiple
// operations; any error or cancellation aborts all of them. Callbacks must not
// perform network I/O or reenter Store. Tx must not escape or be used concurrently.
// Durable engines must reserve receipts before entering this write boundary.
//
// Every operation names the dataset it is for. A server may hold more than one,
// and an identity naming none that this store serves is absence: not_found,
// never a report of what else the store holds.
//
// Datasets answers what the caller may use. A dataset the caller holds no grant
// on is absent from the listing rather than refused within it, so the listing
// reveals nothing about datasets that are not theirs. An empty listing is not
// an error.
type Store interface {
	Datasets(context.Context, Access) ([]Info, error)
	Info(context.Context, Access, string) (Info, error)
	Transact(context.Context, Access, string, func(Tx) error) error
}

type Tx interface {
	DatasetID() string
	Metadata(string) (p.Meta, error)
	ListMetadata() ([]p.Meta, error)
	CreateMetadata(p.Meta) (p.Meta, error)
	Query(string, p.Query, int) (p.RecordSet, error)
	Object(typeID, objectID string, fields []string) (p.RecordSet, error)
	Insert(typeID string, fields map[string]p.Value) (p.RecordSet, error)
	Update(typeID, objectID string, fields map[string]p.Value) error
	ExpectType(string, string) error
	Expect(typeID, objectID string, fields map[string]p.Value, version *string) error
	UpdateMetadata(p.Meta, string) (p.Meta, error)
	DeleteMetadata(string, bool) ([]string, int, error)
	Matching(string, *p.Predicate) ([]ObjectKey, error)
	// Aggregate and DistinctReferences are exact-type only and are refused with
	// not_supported unless the composition advertises their capability.
	Aggregate(typeID, field, groupBy string, filter *p.Predicate) ([]p.Aggregate, error)
	DistinctReferences(typeID, field string, maximum int, filter *p.Predicate) ([]p.Reference, error)
	Matches(string, string, string, *p.Predicate) (bool, error)
	Authorize(Grants) error
	ValidateFields(string, map[string]p.Value) error
	Watermark() string
	Delete(typeID, objectID string) error
}

func Refuse(code string) error {
	return &p.Error{Code: code, Message: "Operation could not be completed"}
}

// RefuseWith names which bound was exceeded. query_result_limit_exceeded covers
// several, and a client that cannot tell a sum overflow from a group limit
// cannot decide whether narrowing the query would help.
func RefuseWith(code, reason string, details map[string]any) error {
	carried := map[string]any{"reason": reason}
	for key, value := range details {
		carried[key] = value
	}
	return &p.Error{Code: code, Message: "Operation could not be completed", Details: carried}
}

func (g Grants) Current() bool { return g.ExpiresAt == nil || time.Now().Before(*g.ExpiresAt) }

// Reader is an engine that can evaluate a request which changes nothing
// without taking a writer's turn. It is optional: a composition that does not
// implement it is correct, only slower, and callers fall back to Transact.
//
// The separation is worth stating because it is not an optimisation detail. A
// read evaluated as a write takes the dataset's write lock, so ordinary reads
// queue behind each other and behind every writer; and it reads the state in
// order to hand back a copy nothing will look at again.
type Reader interface {
	Read(context.Context, Access, string, func(Tx) error) error
	// ReadSchema evaluates a request that reads the schema and no records. It
	// is a different question from Read and not a hint about the same one: an
	// engine that keeps records in storage need not fetch any of them to
	// answer it, and answering it through Read meant decoding every record in
	// the dataset to describe a type.
	ReadSchema(context.Context, Access, string, func(Tx) error) error
	// ReadRecord evaluates a request that reads records by identity and does
	// not scan for them. An engine that keeps records in storage fetches the
	// ones asked for and no others, so reading one record costs one record.
	ReadRecord(context.Context, Access, string, func(Tx) error) error
}

// ReadOnly wraps a transaction so that every operation which would change the
// dataset is refused. It is the backstop for the boundary above: the caller
// decides whether a request is a mutation, and this makes a wrong decision an
// error the caller sees rather than a write that silently disappears when the
// read evaluation declines to save its state.
type ReadOnly struct{ Tx }

func (ReadOnly) CreateMetadata(p.Meta) (p.Meta, error) { return p.Meta{}, readOnly() }
func (ReadOnly) UpdateMetadata(p.Meta, string) (p.Meta, error) {
	return p.Meta{}, readOnly()
}
func (ReadOnly) DeleteMetadata(string, bool) ([]string, int, error) { return nil, 0, readOnly() }
func (ReadOnly) Insert(string, map[string]p.Value) (p.RecordSet, error) {
	return p.RecordSet{}, readOnly()
}
func (ReadOnly) Update(string, string, map[string]p.Value) error { return readOnly() }
func (ReadOnly) Delete(string, string) error                     { return readOnly() }

// SchemaOnly wraps a transaction that may read the schema and nothing else.
// The records are not there to be read: a schema evaluation need not have
// fetched any. Refusing is the point — a record read that quietly answered
// "none" would be a wrong answer rather than an error, and the caller that
// routed it here is the one that has to hear about it.
type SchemaOnly struct{ ReadOnly }

func (SchemaOnly) Object(string, string, []string) (p.RecordSet, error) {
	return p.RecordSet{}, readOnly()
}
func (SchemaOnly) Query(string, p.Query, int) (p.RecordSet, error) {
	return p.RecordSet{}, readOnly()
}
func (SchemaOnly) Matching(string, *p.Predicate) ([]ObjectKey, error) { return nil, readOnly() }
func (SchemaOnly) Matches(string, string, string, *p.Predicate) (bool, error) {
	return false, readOnly()
}
func (SchemaOnly) Aggregate(string, string, string, *p.Predicate) ([]p.Aggregate, error) {
	return nil, readOnly()
}
func (SchemaOnly) DistinctReferences(string, string, int, *p.Predicate) ([]p.Reference, error) {
	return nil, readOnly()
}

// RecordOnly wraps a transaction that may read records by identity and may
// not scan for them. The records it was not asked for are not there, so a
// scan is refused for the reason a schema evaluation refuses a record read:
// answering "none" would be wrong rather than unavailable.
type RecordOnly struct{ ReadOnly }

func (RecordOnly) Query(string, p.Query, int) (p.RecordSet, error) {
	return p.RecordSet{}, readOnly()
}
func (RecordOnly) Matching(string, *p.Predicate) ([]ObjectKey, error) { return nil, readOnly() }
func (RecordOnly) Matches(string, string, string, *p.Predicate) (bool, error) {
	return false, readOnly()
}
func (RecordOnly) Aggregate(string, string, string, *p.Predicate) ([]p.Aggregate, error) {
	return nil, readOnly()
}
func (RecordOnly) DistinctReferences(string, string, int, *p.Predicate) ([]p.Reference, error) {
	return nil, readOnly()
}

// An operation reaching an evaluation that cannot serve it is the
// composition's mistake and not the caller's, so it is reported as one rather
// than as a refusal the caller could act on.
func readOnly() error { return Refuse("internal_error") }

// Usage is how much a dataset holds against the bound that stops it accepting
// writes, so an administrator can see a dataset filling before it is full
// rather than learning from a refused write.
type Usage struct {
	Records int64 `json:"records"`
	Bytes   int64 `json:"bytes"`
	// MaxBytes is the dataset's storage quota, absent when it has none: a
	// client shows usage alone then, never a dataset nearly full.
	MaxBytes *int64 `json:"max_bytes,omitempty"`
}

// UsageReader measures a dataset for its administrators. Composition only: it
// takes no access, and the surface calling it decides who may see the answer.
type UsageReader interface {
	DatasetUsage(ctx context.Context, dataset string) (Usage, error)
}
