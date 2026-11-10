// Package recordstate evaluates bounded Tessera datasets for the memory and SQLite engines.
package recordstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

type object struct {
	sequence uint64
	typeID   string
	fields   map[string]p.Value
}
type state struct {
	types     map[string]p.Meta
	objects   map[string]object
	next      uint64
	watermark string
}
type Store struct {
	lock chan struct{}
	id   string
	// The evaluator holds one dataset and has no host to name it, so it carries
	// the name the listing has always reported for a lone dataset.
	displayName    string
	limits         p.Limits
	reads          map[string]bool
	authorizations map[store.Access]store.Authorization
	// source fetches one record by identity for an evaluation built without
	// them. Nil in a store that already holds the whole dataset.
	source func(id string) (Row, bool, error)
	// scan streams the records of the named types in identity order, for an
	// evaluation built without them. visit returns false to stop early. Nil
	// in a store that already holds the whole dataset.
	//
	// Identity order means byte order over the object identifier, which is
	// the order this evaluator sorts by. A source that yielded any other
	// order would make early termination return the wrong rows while every
	// row it returned was genuinely a match — a wrong answer that looks right.
	scan func(typeIDs []string, visit func(id string, row Row) (bool, error)) error
	// sorted streams the records of the named types in a result sort's order
	// rather than identity order. See SetSortedScanSource.
	sorted SortedScan
	// scanFrom is scan resuming after an identity, for a page that continues
	// a sequence. Optional: without it the scan starts at the beginning and
	// skips what precedes the position.
	scanFrom       func(typeIDs []string, after string, visit func(id string, row Row) (bool, error)) error
	state          state
	secret         [32]byte
	creatorSecret  [32]byte
	mediaAuthority func(string) error
	// sourcedWrites lets a write evaluate against records fetched one at a
	// time through source rather than against the whole dataset. What it
	// fetches is kept, with its original, so the caller can write back only
	// what changed. A scan streams from the storage with this write's changes
	// laid over it (scanRows). A schema change that rewrites records needs
	// the whole dataset and cannot be answered this way; it
	// sets neededImage and fails, and the caller evaluates again from a
	// complete image. See NeededImage.
	sourcedWrites bool
	neededImage   bool
	originals     map[string]Row
}

// EnableSourcedWrites makes writes fetch the records they touch through the
// object source instead of expecting the whole dataset in the image. The
// image the store was built from then holds the types and no records.
func (s *Store) EnableSourcedWrites() { s.sourcedWrites = true }

// NeededImage reports whether the last write asked for something only a
// complete image can answer. The write failed; evaluate it again from one.
func (s *Store) NeededImage() bool { return s.neededImage }

// Originals are the records the last sourced write fetched, as they were
// before it changed anything. With Image, which holds them as they are now,
// they are everything the write touched.
func (s *Store) Originals() map[string]Row { return s.originals }

// CheckReadCapabilities rejects a name this implementation does not provide, or
// a repeated one. Advertising what a server cannot do is worse than advertising
// nothing: a client that meets a missing capability loses a feature silently.
func CheckReadCapabilities(names []string) error {
	seen := map[string]bool{}
	for _, name := range names {
		known := false
		for _, retired := range p.RetiredReadCapabilities {
			if name == retired {
				// Named a capability that is now a required operation. A
				// deployment asking to withhold one is asking for something
				// this protocol no longer offers, and accepting the name
				// silently would leave the configuration claiming a choice
				// nobody has.
				return errors.New("read capability " + name + " is now a required operation and cannot be configured")
			}
		}
		for _, allowed := range p.OptionalReadCapabilities {
			known = known || name == allowed
		}
		if !known || seen[name] {
			return store.Refuse("invalid_field")
		}
		seen[name] = true
	}
	return nil
}

// EnableReads advertises optional read capabilities. A composition may withhold
// any of them, and a withheld capability is refused rather than approximated.
// Required operations are not listed here and cannot be disabled.
func (s *Store) EnableReads(names ...string) error {
	if err := CheckReadCapabilities(names); err != nil {
		return err
	}
	enabled := map[string]bool{}
	for _, name := range names {
		enabled[name] = true
	}
	s.reads = enabled
	return nil
}

// ReadCapabilities lists the advertised optional read capabilities in the order
// the protocol names them, so discovery does not depend on map iteration.
func (s *Store) ReadCapabilities() []string {
	advertised := []string{}
	for _, name := range p.OptionalReadCapabilities {
		if s.reads[name] {
			advertised = append(advertised, name)
		}
	}
	return advertised
}

func New(limits p.Limits) (*Store, error) {
	if limits.MaxRequestBytes <= 0 || limits.MaxProjectedFields <= 0 || limits.MaxTextBytes < 0 || limits.MaxInlineBlobBytes < 0 {
		return nil, store.Refuse("internal_error")
	}
	// A store built from limits that predate the group bound, or from a
	// partially filled struct, would otherwise refuse every grouped
	// aggregation at a bound of zero. Decoding stored limits defaults this
	// too; this covers the ones that were never encoded.
	if limits.MaxAggregateGroups <= 0 {
		limits.MaxAggregateGroups = p.DefaultAggregateGroups
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	s := &Store{lock: make(chan struct{}, 1), id: "s-" + hex.EncodeToString(id[:]), displayName: defaultDisplayName, limits: limits,
		authorizations: map[store.Access]store.Authorization{}, state: state{types: map[string]p.Meta{}, objects: map[string]object{}, next: 1, watermark: "w:" + hex.EncodeToString(id[:])}}
	if _, err := rand.Read(s.secret[:]); err != nil {
		return nil, err
	}
	s.creatorSecret = s.secret
	return s, nil
}

func (s *Store) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.lock <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Store) release() { <-s.lock }

// SetAuthorization is a trusted host administration operation, not an HTTP API.
// It serializes with complete transactions, so revocation cannot land between a
// decision and its write. Replacing an authorization takes effect on the next Tx.
// SetAuthorization installs a grant over one dataset. A grant has to be over
// something, so a dataset this evaluator does not hold is absence.
func (s *Store) SetAuthorization(ctx context.Context, dataset string, a store.Access, grant store.Authorization) error {
	if !a.Valid() || !grant.View.Valid() || (!grant.View.Full() && (grant.Grants.Write || grant.Grants.Schema || grant.Grants.Erase)) {
		return store.Refuse("permission_denied")
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if dataset != s.id {
		return store.Refuse("not_found")
	}
	if grant.Grants.ExpiresAt != nil {
		expiry := *grant.Grants.ExpiresAt
		grant.Grants.ExpiresAt = &expiry
	}
	s.authorizations[a] = grant
	return nil
}
func (s *Store) Revoke(ctx context.Context, dataset string, a store.Access) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if dataset != s.id {
		return store.Refuse("not_found")
	}
	delete(s.authorizations, a)
	return nil
}
func (s *Store) authorization(a store.Access) (store.Authorization, error) {
	grant, ok := s.authorizations[a]
	if !a.Valid() || !ok || !grant.View.Valid() || grant.Grants.ExpiresAt != nil && !time.Now().Before(*grant.Grants.ExpiresAt) {
		return store.Authorization{}, store.Refuse("permission_denied")
	}
	return grant, nil
}

// Datasets lists the one dataset this evaluator holds, and lists it only for a
// caller who may use it. An absent grant is an empty listing, not a refusal.
func (s *Store) Datasets(ctx context.Context, a store.Access) ([]store.Info, error) {
	info, err := s.Info(ctx, a, s.id)
	switch p.ErrorCode(err) {
	case "permission_denied", "unavailable":
		return []store.Info{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []store.Info{info}, nil
}
func (s *Store) Info(ctx context.Context, a store.Access, dataset string) (store.Info, error) {
	if err := s.acquire(ctx); err != nil {
		return store.Info{}, err
	}
	defer s.release()
	// not_found means proven retirement. This evaluator holds one dataset and
	// keeps no retirement ledger, so it can never prove one: an identity it
	// does not hold is concealed, and so is a caller holding no usable grant,
	// which would otherwise confirm the dataset exists.
	if dataset != s.id {
		return store.Info{}, store.Refuse("unavailable")
	}
	g, err := s.authorization(a)
	if err != nil {
		if p.ErrorCode(err) == "permission_denied" {
			return store.Info{}, store.Refuse("unavailable")
		}
		return store.Info{}, err
	}
	if !g.Grants.Read && !g.Grants.Write && !g.Grants.Schema && !g.Grants.Erase && !g.Grants.Insert {
		return store.Info{}, store.Refuse("unavailable")
	}
	schema := make(map[string]p.Meta, len(s.state.types))
	for id := range s.state.types {
		if !g.View.AllowsMetadata(id) {
			continue
		}
		m, e := p.EffectiveMetadata(s.state.types, id)
		if e != nil {
			return store.Info{}, e
		}
		schema[id] = m
	}
	raw, e := p.MarshalWire(schema)
	if e != nil {
		return store.Info{}, e
	}
	access := g.Grants
	if access.ExpiresAt != nil {
		expiry := *access.ExpiresAt
		access.ExpiresAt = &expiry
	}
	capabilities := s.ReadCapabilities()
	if s.limits.MaxTextBytes >= p.CreatorIDBytes {
		capabilities = append(capabilities, p.CreatorAttributionCapability)
	}
	return store.Info{ID: s.id, DisplayName: s.displayName, Access: access, Capabilities: capabilities, Limits: s.limits, Watermark: s.state.watermark, ViewVersion: s.digest(&s.secret, "view", viewBytes(g.View)), SchemaVersion: s.digest(&s.secret, "schema", raw)}, nil
}

// ReadSchema evaluates a request that reads the schema and no records. This
// evaluator holds the dataset in memory either way, so there is nothing here
// to avoid fetching; the boundary exists so that a composition routing a
// request to it is answering the same question on every engine.
func (s *Store) ReadSchema(ctx context.Context, a store.Access, dataset string, fn func(store.Tx) error) error {
	return s.read(ctx, a, dataset, func(tx store.Tx) error { return fn(store.SchemaOnly{ReadOnly: store.ReadOnly{Tx: tx}}) })
}

// ReadRecord evaluates a request that reads named records and does not scan.
// The evaluator may have been built without any records at all, in which case
// each one asked for is fetched through the source below.
func (s *Store) ReadRecord(ctx context.Context, a store.Access, dataset string, fn func(store.Tx) error) error {
	return s.read(ctx, a, dataset, func(tx store.Tx) error {
		return fn(store.RecordOnly{ReadOnly: store.ReadOnly{Tx: tx}})
	})
}

// SetObjectSource installs a way to fetch one record by identity. A store
// built from a complete image needs none; one built from a dataset row and
// its types uses it to answer for a single record without reading the rest.
func (s *Store) SetObjectSource(source func(id string) (Row, bool, error)) { s.source = source }

// SetScanSource installs a way to stream records of given types in identity
// order. With it, an evaluation built without the records can answer a query
// by reading only as far as the answer needs; without it, such an evaluation
// refuses to scan at all rather than answering from the empty image it holds.
func (s *Store) SetScanSource(scan func(typeIDs []string, visit func(id string, row Row) (bool, error)) error) {
	s.scan = scan
}

// SortKey is one term of a result sort, with the type every value of that
// field has — declared once, so the same in every type of the scope.
type SortKey struct {
	FieldID   string
	Type      p.Type
	Ascending bool
}

// SortedScan streams the records of the named types ordered by keys, each in
// protocol order — nulls first ascending and last descending — then by object
// identifier ascending, byte for byte. Before the first row it calls admit
// with how many records the types hold, so a scan bound refuses exactly what
// reading the whole scope would have refused. It answers false, having read
// nothing, when it cannot order by these keys; the evaluator then sorts as it
// always did.
type SortedScan func(typeIDs []string, keys []SortKey, from *SortFrom, admit func(scope int) error, visit func(id string, row Row) (bool, error)) (bool, error)

// SortFrom is the row a page of a sorted sequence resumes after: its values
// for the sort's fields, in the sort's order, and its identity. A source
// yields only rows that follow it in the sort's order.
type SortFrom struct {
	Values []p.Value
	ID     string
}

// SetSortedScanSource installs a way to read a sorted query's rows already in
// its order, so the query stops once it holds its limit instead of reading
// and sorting the whole scope. Only reads use it: a write sees its own changes
// laid over storage, which a source ordering storage alone cannot place.
func (s *Store) SetSortedScanSource(sorted SortedScan) { s.sorted = sorted }

// SetScanFromSource installs a scan that starts after an identity, so a page
// continuing a sequence reads from where the last one ended instead of from
// the beginning.
func (s *Store) SetScanFromSource(scan func(typeIDs []string, after string, visit func(id string, row Row) (bool, error)) error) {
	s.scanFrom = scan
}

// Read evaluates without copying the dataset and without writing anything
// back. A write transaction clones every type and every object so that a
// failure can be dropped; a read has nothing to drop, and the clone was the
// largest single cost of answering one.
func (s *Store) Read(ctx context.Context, a store.Access, dataset string, fn func(store.Tx) error) error {
	return s.read(ctx, a, dataset, func(tx store.Tx) error { return fn(store.ReadOnly{Tx: tx}) })
}

// Evaluate runs a prepared read against this evaluator. The wrapper the
// caller supplies decides what the transaction will answer, which is how the
// SQL engine applies the same boundaries to a store it built itself.
func (s *Store) Evaluate(ctx context.Context, a store.Access, dataset string, fn func(store.Tx) error) error {
	return s.read(ctx, a, dataset, fn)
}

func (s *Store) read(ctx context.Context, a store.Access, dataset string, fn func(store.Tx) error) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if dataset != s.id {
		return store.Refuse("unavailable")
	}
	grant, err := s.authorization(a)
	if err != nil {
		if p.ErrorCode(err) == "permission_denied" {
			return store.Refuse("unavailable")
		}
		return err
	}
	// The state is shared rather than copied, which is only sound because the
	// wrapper refuses every operation that would write to it.
	t := &transaction{ctx: ctx, engine: s, auth: grant, state: s.state, active: true}
	defer func() { t.active = false }()
	if err = fn(t); err != nil {
		return err
	}
	if t.failed != nil {
		return t.failed
	}
	return ctx.Err()
}

func (s *Store) Transact(ctx context.Context, a store.Access, dataset string, fn func(store.Tx) error) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if dataset != s.id {
		return store.Refuse("unavailable")
	}
	grant, err := s.authorization(a)
	if err != nil {
		if p.ErrorCode(err) == "permission_denied" {
			return store.Refuse("unavailable")
		}
		return err
	}
	s.neededImage, s.originals = false, nil
	t := &transaction{ctx: ctx, engine: s, auth: grant, principal: a.Principal, state: cloneState(s.state), active: true}
	if s.sourcedWrites {
		t.originals = map[string]object{}
	}
	defer func() { t.active = false }()
	if err = fn(t); err != nil {
		return err
	}
	if t.failed != nil {
		return t.failed
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	t.state.watermark = t.Watermark()
	s.state = t.state
	if t.originals != nil {
		s.originals = make(map[string]Row, len(t.originals))
		for id, o := range t.originals {
			s.originals[id] = Row{Sequence: o.sequence, TypeID: o.typeID, Fields: o.fields}
		}
	}
	return nil
}

type transaction struct {
	ctx       context.Context
	engine    *Store
	auth      store.Authorization
	principal string
	state     state
	active    bool
	failed    error
	dirty     bool
	// streamed holds the records this evaluation read through the scan
	// source. They live here and never in state: the image a read evaluates
	// against may be shared rather than copied, so writing into it would be a
	// race — which is the reason lookup does not keep what it fetches either.
	streamed map[string]object
	// originals, in a sourced write, holds each record fetched as it was when
	// fetched, and so is the "before" of everything this write touched. A
	// record fetched and then deleted stays here while leaving state. Nil in
	// any other evaluation.
	originals map[string]object
	// inserted, in a sourced write, names the records this write created.
	inserted map[string]bool
	// page, while PageAfter is answering, is where the page resumes and
	// which rows the sequence already returned. Nil otherwise.
	page *pageState
}

// needImage is how a sourced write gives up on an operation that needs the
// whole dataset: the caller sees NeededImage and evaluates again from it.
func (t *transaction) needImage() error {
	t.engine.neededImage = true
	return store.Refuse("internal_error")
}

// held is a record this evaluation has seen, from the image or streamed for
// this request. Every read that follows a scan goes through here, so a
// streamed row is found wherever an image row would have been.
func (t *transaction) held(id string) object {
	if o, ok := t.state.objects[id]; ok {
		return o
	}
	return t.streamed[id]
}

func (t *transaction) check(granted bool) error {
	if !t.active {
		return store.Refuse("internal_error")
	}
	if t.failed != nil {
		return t.failed
	}
	if err := t.ctx.Err(); err != nil {
		return err
	}
	if !granted {
		return store.Refuse("permission_denied")
	}
	return nil
}

// Every mutator poisons the unit of work on failure, even if a host accidentally
// swallows its error. Reads may be probed without aborting the write transaction.
func (t *transaction) fail(err error) error {
	if err != nil && t.failed == nil {
		t.failed = err
	}
	return err
}

func cloneFields(fields map[string]p.Value) map[string]p.Value {
	result := make(map[string]p.Value, len(fields))
	for k, v := range fields {
		result[k] = v
	}
	return result
}
func cloneMeta(m p.Meta) p.Meta {
	m.Fields = append([]p.Field{}, m.Fields...)
	m.Roles = append([]string(nil), m.Roles...)
	m.Annotations = cloneAnnotations(m.Annotations)
	for i := range m.Fields {
		m.Fields[i].Roles = append([]string(nil), m.Fields[i].Roles...)
		m.Fields[i].Annotations = cloneAnnotations(m.Fields[i].Annotations)
	}
	if m.ParentID != nil {
		parent := *m.ParentID
		m.ParentID = &parent
	}
	return m
}
func cloneState(s state) state {
	r := state{types: make(map[string]p.Meta, len(s.types)), objects: make(map[string]object, len(s.objects)), next: s.next, watermark: s.watermark}
	for k, v := range s.types {
		r.types[k] = cloneMeta(v)
	}
	for k, v := range s.objects {
		r.objects[k] = object{v.sequence, v.typeID, cloneFields(v.fields)}
	}
	return r
}
func (t *transaction) Metadata(id string) (p.Meta, error) {
	if err := t.check(t.auth.Grants.Read || t.auth.Grants.Insert); err != nil {
		return p.Meta{}, err
	}
	if err := p.ValidateIdentifier(p.MetaIdentifier, id); err != nil {
		return p.Meta{}, err
	}
	if !t.auth.View.AllowsMetadata(id) {
		return p.Meta{}, store.Refuse("unavailable")
	}
	return t.effective(id)
}
func (t *transaction) ListMetadata() ([]p.Meta, error) {
	if err := t.check(t.auth.Grants.Read || t.auth.Grants.Insert); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(t.state.types))
	for id := range t.state.types {
		if !t.auth.View.AllowsMetadata(id) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]p.Meta, 0, len(ids))
	for _, id := range ids {
		m, err := t.effective(id)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, nil
}
func (t *transaction) CreateMetadata(m p.Meta) (result p.Meta, err error) {
	defer func() { t.fail(err) }()
	if err = t.check(t.auth.Grants.Schema && t.auth.View.Full()); err != nil {
		return result, err
	}
	if err = m.ValidateSemantics(t.engine.limits); err != nil {
		return result, err
	}
	if err = m.ValidateDeclared(); err != nil {
		return result, err
	}
	if _, ok := t.state.types[m.ID]; ok {
		return result, store.Refuse("meta_type_already_exists")
	}
	if err = t.touch(); err != nil {
		return result, err
	}
	t.state.types[m.ID] = cloneMeta(m)
	result, err = t.effective(m.ID)
	return
}
func (t *transaction) lookup(typeID, id string) (object, error) {
	if err := p.ValidateIdentifier(p.MetaIdentifier, typeID); err != nil {
		return object{}, err
	}
	if err := p.ValidateIdentifier(p.ObjectIdentifier, id); err != nil {
		return object{}, err
	}
	// No schema or object existence check may precede the view decision.
	if !t.auth.View.Allows(store.ObjectKey{TypeID: typeID, ObjectID: id}) {
		return object{}, store.Refuse("unavailable")
	}
	if _, ok := t.state.types[typeID]; !ok && t.auth.View.SequenceCut() == 0 {
		return object{}, store.Refuse("meta_type_not_found")
	}
	o, ok := t.state.objects[id]
	if !ok && t.originals != nil {
		if _, seen := t.originals[id]; seen {
			// Fetched earlier in this write and deleted since.
			return object{}, store.Refuse("not_found")
		}
	}
	if !ok && t.originals != nil {
		// A sourced write that already streamed this record, matching it
		// before changing it, has it as the storage holds it: fetching it
		// again was a round trip for every record a bulk update touched.
		// Streamed rows the write itself changed are in state and found above;
		// one it inserted was streamed from state, not from the storage, and
		// if it is gone from state it was deleted, not waiting to be fetched.
		if streamed, seen := t.streamed[id]; seen && !t.inserted[id] {
			t.originals[id] = object{sequence: streamed.sequence, typeID: streamed.typeID, fields: cloneFields(streamed.fields)}
			t.state.objects[id] = object{sequence: streamed.sequence, typeID: streamed.typeID, fields: cloneFields(streamed.fields)}
			o, ok = t.state.objects[id], true
		}
	}
	if !ok && t.engine.source != nil {
		// An evaluation built without the records fetches the one it was
		// asked for. The result is deliberately not kept: the state a read
		// evaluates against is shared rather than copied, so writing into it
		// would be a race, and one record is one fetch either way.
		var row Row
		var err error
		if row, ok, err = t.engine.source(id); err != nil {
			return object{}, err
		}
		o = object{sequence: row.Sequence, typeID: row.TypeID, fields: row.Fields}
		// A write's state is its own copy, so there it is kept: a later
		// operation in the same write must see this one's changes, and the
		// original is what the write is compared against.
		if ok && t.originals != nil {
			t.originals[id] = object{sequence: o.sequence, typeID: o.typeID, fields: cloneFields(o.fields)}
			t.state.objects[id] = object{sequence: o.sequence, typeID: o.typeID, fields: cloneFields(o.fields)}
			o = t.state.objects[id]
		}
	}
	if t.auth.View.SequenceCut() != 0 {
		sequence := o.sequence
		if !ok {
			// Minted identities encode this engine's never-reused sequence.
			// A deleted identity retains its place below the cut; arbitrary
			// identities cannot establish that they were ever inside it.
			var err error
			sequence, err = strconv.ParseUint(id, 10, 64)
			if err != nil || sequence == 0 {
				return object{}, store.Refuse("unavailable")
			}
		}
		if !t.auth.View.AllowsSequence(store.ObjectKey{TypeID: typeID, ObjectID: id}, sequence) {
			return object{}, store.Refuse("unavailable")
		}
	}
	if !ok || o.typeID != typeID {
		return object{}, store.Refuse("not_found")
	}
	return o, nil
}
func (t *transaction) Object(typeID, id string, fields []string) (p.RecordSet, error) {
	if err := t.check(t.auth.Grants.Read); err != nil {
		return p.RecordSet{}, err
	}
	o, err := t.lookup(typeID, id)
	if err != nil {
		return p.RecordSet{}, err
	}
	return t.record(typeID, id, o, fields, true)
}
func (t *transaction) record(typeID, id string, o object, fields []string, includeVersion bool) (p.RecordSet, error) {
	meta, err := t.effective(typeID)
	if err != nil {
		return p.RecordSet{}, err
	}
	if fields != nil {
		if len(fields) > t.engine.limits.MaxProjectedFields {
			return p.RecordSet{}, store.Refuse("query_result_limit_exceeded")
		}
		byID := map[string]p.Field{}
		for _, f := range meta.Fields {
			byID[f.ID] = f
		}
		selected := make([]p.Field, 0, len(fields))
		seen := map[string]bool{}
		for _, id := range fields {
			f, ok := byID[id]
			if !ok || seen[id] {
				return p.RecordSet{}, store.Refuse("invalid_field")
			}
			seen[id] = true
			selected = append(selected, f)
		}
		meta.Fields = selected
	}
	if len(meta.Fields) > t.engine.limits.MaxProjectedFields {
		return p.RecordSet{}, store.Refuse("query_result_limit_exceeded")
	}
	values := make([]p.Value, len(meta.Fields))
	for i, f := range meta.Fields {
		values[i] = o.fields[f.ID]
	}
	version := ""
	if includeVersion {
		version = t.version(o)
	}
	return p.RecordSet{Watermark: t.Watermark(), Meta: meta, Records: []p.Record{{ID: p.ObjectID{ID: id, DatasetID: t.engine.id}, MetaID: o.typeID, Values: values, Version: version}}}, nil
}

// convertDeclared fills creator fields on insert and reports which supplied
// fields are immutable for updates.
func (t *transaction) convertDeclared(typeID string, fields map[string]p.Value, inserting bool) (map[string]p.Value, map[string]bool, error) {
	if !t.auth.View.AllowsType(typeID) && !t.auth.View.AllowsInsert(typeID) {
		return nil, nil, store.Refuse("permission_denied")
	}
	meta, err := t.effective(typeID)
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]p.Field{}
	result := make(map[string]p.Value, len(fields))
	var creator p.Value
	for _, f := range meta.Fields {
		byID[f.ID] = f
		if inserting && f.Creator {
			if t.engine.limits.MaxTextBytes < p.CreatorIDBytes {
				return nil, nil, store.Refuse("not_supported")
			}
			if creator.Type() == p.NullType {
				// Bind to the dataset and principal, never the installation or
				// a caller-supplied identity. Keep the principal off the wire.
				digest := t.engine.digest(&t.engine.creatorSecret, "record_creator", []byte(t.principal))
				creator, err = p.NewValue(p.TextType, digest[2:2+p.CreatorIDBytes])
				if err != nil {
					return nil, nil, err
				}
			}
			result[f.ID] = creator
		}
	}
	var immutable map[string]bool
	for id, v := range fields {
		declared, ok := byID[id]
		if !ok || declared.Creator {
			return nil, nil, store.Refuse("invalid_field")
		}
		kind := declared.Type
		if kind == p.MediaType && v.Type() != p.NullType && t.engine.mediaAuthority == nil {
			return nil, nil, store.Refuse("not_supported")
		}
		converted, err := v.Convert(kind, t.engine.limits)
		if err != nil {
			return nil, nil, err
		}
		// A value with more fraction digits than the field declares would have
		// to be rounded to be stored, and this store does not round. Refusing
		// is what makes the declared scale a promise about every value in the
		// field rather than about most of them.
		if scale, declaredScale := declared.FixedScaleOf(); declaredScale && converted.Type() == p.FixedType {
			if p.FixedScale(converted.Payload().(string)) > scale {
				return nil, nil, store.Refuse("invalid_field_value")
			}
		}
		if converted.Type() == p.MediaType {
			if err = t.engine.mediaAuthority(converted.Payload().(string)); err != nil {
				return nil, nil, err
			}
		}
		result[id] = converted
		if declared.Immutable {
			if immutable == nil {
				immutable = map[string]bool{}
			}
			immutable[id] = true
		}
	}
	return result, immutable, nil
}
func (t *transaction) Insert(typeID string, fields map[string]p.Value) (result p.RecordSet, err error) {
	defer func() { t.fail(err) }()
	if err = t.check(t.auth.Grants.Write || t.auth.Grants.Insert); err != nil {
		return result, err
	}
	if err = p.ValidateIdentifier(p.MetaIdentifier, typeID); err != nil {
		return result, err
	}
	// An owner inserts through Write over the whole dataset. Anyone else
	// inserts only through an Insert grant naming this concrete type — never
	// a descendant, for the reason a type grant never covers one. Identity-set
	// views still cannot authorize a newly minted identity on their own.
	owner := t.auth.Grants.Write && t.auth.View.Full()
	if !owner && !(t.auth.Grants.Insert && t.auth.View.AllowsInsert(typeID)) {
		return result, store.Refuse("permission_denied")
	}
	converted, _, err := t.convertDeclared(typeID, fields, true)
	if err != nil {
		return result, err
	}
	if t.state.next == math.MaxUint64 {
		return result, store.Refuse("query_result_limit_exceeded")
	}
	if err = t.touch(); err != nil {
		return result, err
	}
	id := strconv.FormatUint(t.state.next, 10)
	t.state.next++
	for k, v := range converted {
		if v.Type() == p.NullType {
			delete(converted, k)
		}
	}
	o := object{t.state.next - 1, typeID, converted}
	t.state.objects[id] = o
	if t.originals != nil {
		if t.inserted == nil {
			t.inserted = map[string]bool{}
		}
		t.inserted[id] = true
	}
	return t.record(typeID, id, o, nil, false)
}
func (t *transaction) Delete(typeID, id string) (err error) {
	defer func() { t.fail(err) }()
	if err = t.check(t.auth.Grants.Write); err != nil {
		return err
	}
	if _, err = t.lookup(typeID, id); err != nil {
		return err
	}
	if err = t.touch(); err != nil {
		return err
	}
	delete(t.state.objects, id)
	return nil
}

// Update sets named fields; an empty PATCH is a checked no-op.
func (t *transaction) Update(typeID, id string, fields map[string]p.Value) (err error) {
	defer func() { t.fail(err) }()
	if err = t.check(t.auth.Grants.Write); err != nil {
		return err
	}
	o, err := t.lookup(typeID, id)
	if err != nil {
		return err
	}
	converted, immutable, err := t.convertDeclared(typeID, fields, false)
	if err != nil {
		return err
	}
	if err = fixedOnceWritten(id, o, converted, immutable); err != nil {
		return err
	}
	if len(converted) > 0 {
		if err = t.touch(); err != nil {
			return err
		}
	}
	for k, v := range converted {
		if v.Type() == p.NullType {
			delete(o.fields, k)
		} else {
			o.fields[k] = v
		}
	}
	t.state.objects[id] = o
	return nil
}

// fixedOnceWritten refuses an update that would change an immutable field
// already holding a value. Null counts as never written, so the first value may
// arrive by update as well as by insert; restating the stored value is not a
// change. "The stored value" is the stored representation, not protocol
// equality: -0 and 0 are equal and are not the same value, and accepting one
// over the other as "unchanged" would change what the record returns.
func fixedOnceWritten(id string, o object, converted map[string]p.Value, immutable map[string]bool) error {
	changed := []string{}
	for field := range immutable {
		stored, written := o.fields[field]
		if !written {
			continue
		}
		if !sameStored(stored, converted[field]) {
			changed = append(changed, field)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)
	return &p.Error{Code: "conflict", Message: "An immutable field already holds a value",
		Details: map[string]any{"reason": "immutable_field", "object_id": id, "fields": changed}}
}

// sameStored reports whether two values of one declared type are the same
// stored value. A float compares by its bits, so the sign of zero counts, which
// is the only place protocol equality and stored identity part.
func sameStored(a, b p.Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	if a.Type() == p.FloatType {
		return math.Float64bits(a.Payload().(float64)) == math.Float64bits(b.Payload().(float64))
	}
	return a.Payload() == b.Payload()
}

func cloneAnnotations(a map[string]json.RawMessage) map[string]json.RawMessage {
	if a == nil {
		return nil
	}
	r := make(map[string]json.RawMessage, len(a))
	for k, v := range a {
		r[k] = append(json.RawMessage(nil), v...)
	}
	return r
}
func (t *transaction) DatasetID() string { return t.engine.id }
func (t *transaction) Watermark() string {
	if !t.dirty {
		return t.state.watermark
	}
	original := t.engine.state
	if t.originals != nil {
		return t.sourcedWatermark()
	}
	if t.state.next != original.next || len(t.state.types) != len(original.types) || len(t.state.objects) != len(original.objects) {
		return t.state.watermark
	}
	equal := func(a, b any) bool {
		x, e := p.MarshalWire(a)
		y, f := p.MarshalWire(b)
		return e == nil && f == nil && bytes.Equal(x, y)
	}
	for id, m := range t.state.types {
		old, ok := original.types[id]
		if !ok || !equal(m, old) {
			return t.state.watermark
		}
	}
	for id, o := range t.state.objects {
		old, ok := original.objects[id]
		if !ok || o.typeID != old.typeID || !equal(o.fields, old.fields) {
			return t.state.watermark
		}
	}
	return original.watermark
}
func (t *transaction) touch() error {
	if t.dirty {
		return nil
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	h := hmac.New(sha256.New, t.engine.secret[:])
	h.Write([]byte(t.engine.id))
	h.Write(nonce[:])
	t.state.watermark = "w:" + hex.EncodeToString(h.Sum(nil))
	t.dirty = true
	return nil
}
func (t *transaction) version(o object) string {
	// The immutable stored values, including unprojected child fields. Absent
	// fields are omitted, so schema-only additions never become hash input.
	b, _ := json.Marshal(o.fields)
	h := hmac.New(sha256.New, t.engine.secret[:])
	h.Write([]byte(t.engine.id))
	h.Write(b)
	return "v1:" + hex.EncodeToString(h.Sum(nil))
}
func (t *transaction) Authorize(g store.Grants) error {
	return t.check((!g.Read || t.auth.Grants.Read) && (!g.Write || t.auth.Grants.Write) && (!g.Schema || t.auth.Grants.Schema) && (!g.Erase || t.auth.Grants.Erase) &&
		(!g.Insert || t.auth.Grants.Write || t.auth.Grants.Insert))
}
func (t *transaction) Expect(typeID, id string, fields map[string]p.Value, version *string) (err error) {
	defer func() { t.fail(err) }()
	if err = t.check(t.auth.Grants.Read); err != nil {
		return err
	}
	o, err := t.lookup(typeID, id)
	if err != nil {
		return err
	}
	if version != nil {
		if *version != t.version(o) {
			return &p.Error{Code: "conflict", Message: "Record version did not match", Details: map[string]any{"object_id": id, "reason": "version_mismatch"}}
		}
		return nil
	}
	meta, err := t.effective(typeID)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, f := range meta.Fields {
		known[f.ID] = true
	}
	mismatch := []string{}
	for k, v := range fields {
		if !known[k] {
			return store.Refuse("invalid_field")
		}
		if err = v.Validate(t.engine.limits); err != nil {
			return err
		}
		equal, e := p.Equal(o.fields[k], v)
		if e != nil {
			return e
		}
		if !equal {
			mismatch = append(mismatch, k)
		}
	}
	if len(mismatch) > 0 {
		sort.Strings(mismatch)
		return &p.Error{Code: "conflict", Message: "Expected values did not match", Details: map[string]any{"object_id": id, "mismatched_fields": mismatch}}
	}
	return nil
}
func (t *transaction) descendant(child, parent string) bool {
	for {
		if child == parent {
			return true
		}
		m, ok := t.state.types[child]
		if !ok || m.ParentID == nil {
			return false
		}
		child = *m.ParentID
	}
}
func (t *transaction) Matching(typeID string, predicate *p.Predicate) ([]store.ObjectKey, error) {
	return t.matching(typeID, predicate, true)
}

// matching answers the same question, ordered or not. Identity order is part
// of what Matching promises and what a query relies on; an aggregate sorts
// its own groups afterwards and never looks at the order it scanned in, so
// sorting for it is a full sort thrown away.
func (t *transaction) matching(typeID string, predicate *p.Predicate, ordered bool) ([]store.ObjectKey, error) {
	return t.scanFor(typeID, predicate, ordered, reach{})
}

// reach narrows what a scan reads and says when it may stop.
type reach struct {
	// types are the concrete types to read. Nil means every descendant of
	// the queried type, which is what an unqualified query covers.
	types map[string]bool
	// stopAfter ends the scan once this many matches are held. It is only
	// correct when the caller's final order is identity order, because the
	// rows it keeps are the first in that order and no others.
	stopAfter int
}

// scanFor finds the records of typeID's scope that satisfy predicate.
//
// Three cases, and the third is the one that matters. A complete image is
// ranged in memory, as it always was. A partial image with a scan source
// streams from storage in identity order and reads only as far as the answer
// needs. A partial image **without** one refuses: it holds no records, and
// answering from it would report a narrowed result — nothing matched — that
// looks exactly like a complete one. That was possible before this and was
// prevented only by the caller's wrapper refusing to scan.
// readable checks a scan may run and names the concrete types it may read:
// descendants of typeID, narrowed to the caller's set when it gave one, and
// always intersected with the view. Type grants bound the query before
// predicates, grouping and limits.
func (t *transaction) readable(typeID string, predicate *p.Predicate, sc reach) (map[string]bool, error) {
	if err := t.check(t.auth.Grants.Read || t.auth.Grants.Write); err != nil {
		return nil, err
	}
	if !t.auth.View.AllowsType(typeID) {
		return nil, store.Refuse("permission_denied")
	}
	meta, err := t.effective(typeID)
	if err != nil {
		return nil, err
	}
	if err = predicate.Validate(meta, true); err != nil {
		return nil, err
	}
	readable := map[string]bool{}
	for id := range t.state.types {
		if !t.descendant(id, typeID) {
			continue
		}
		if sc.types != nil && !sc.types[id] {
			continue
		}
		if !t.auth.View.Full() && !t.auth.View.AllowsType(id) {
			continue
		}
		readable[id] = true
	}
	return readable, nil
}

func (t *transaction) scanFor(typeID string, predicate *p.Predicate, ordered bool, sc reach) ([]store.ObjectKey, error) {
	readable, err := t.readable(typeID, predicate, sc)
	if err != nil {
		return nil, err
	}
	result := []store.ObjectKey{}
	scanned := 0
	// consider charges one row against the scan bound and reports whether it
	// matched. Stopping is the caller's decision, because only the streaming
	// path reads in the order that makes stopping correct.
	consider := func(id string, o object) (bool, error) {
		if !t.auth.View.AllowsSequence(store.ObjectKey{TypeID: o.typeID, ObjectID: id}, o.sequence) {
			return false, nil
		}
		scanned++
		if scanned > t.engine.limits.MaxScannedRows {
			return false, store.Refuse("query_result_limit_exceeded")
		}
		if err := t.ctx.Err(); err != nil {
			return false, err
		}
		match, e := predicate.Match(id, o.fields)
		if e != nil {
			return false, e
		}
		if match {
			result = append(result, store.ObjectKey{TypeID: o.typeID, ObjectID: id})
		}
		return match, nil
	}

	switch {
	case t.engine.scan != nil:
		types := make([]string, 0, len(readable))
		for id := range readable {
			types = append(types, id)
		}
		sort.Strings(types)
		if t.streamed == nil {
			t.streamed = map[string]object{}
		}
		previous := ""
		first := true
		held := 0
		scan := t.scanRows
		after := ""
		if t.page != nil {
			after = t.page.from.After
		}
		if after != "" && t.originals == nil && t.engine.scanFrom != nil {
			scan = func(types []string, visit func(string, Row) (bool, error)) error {
				return t.engine.scanFrom(types, after, visit)
			}
		}
		err = scan(types, func(id string, row Row) (bool, error) {
			// A page continuing a sequence begins after the last identity it
			// returned; a source that cannot start there is read past it.
			if after != "" && id <= after {
				return true, nil
			}
			// Checked as rows arrive, and the limit of the check matters as
			// much as the check. A source yielding another order than this
			// evaluator's makes early termination return rows that are each
			// genuine matches but not the first ones — a wrong answer that
			// looks right. This refuses the request when it sees two rows out
			// of order.
			//
			// **It cannot see a read that stops after one row**, because there
			// is no second row to compare. Seeding a descending source proved
			// it: at limit 1 the wrong row came back and this never fired. So
			// it is a net, not the guarantee. The guarantee is the source's own
			// COLLATE "C" and the PostgreSQL test that exercises a single-row
			// read with identifiers a locale collation orders differently.
			if !first && id <= previous {
				return false, store.Refuse("internal_error")
			}
			first, previous = false, id
			if !readable[row.TypeID] {
				return false, store.Refuse("internal_error")
			}
			o := object{sequence: row.Sequence, typeID: row.TypeID, fields: row.Fields}
			matched, e := consider(id, o)
			if e != nil || !matched {
				// A row that does not match is read, charged to the scan
				// bound and dropped. Keeping it would make a selective filter
				// over a large type hold the type, which is the cost this
				// path exists to avoid.
				return e == nil, e
			}
			held += row.Bytes
			if held > t.engine.limits.MaxMaterializedBytes {
				return false, store.RefuseWith("query_result_limit_exceeded", "materialization_limit",
					map[string]any{"maximum": t.engine.limits.MaxMaterializedBytes})
			}
			t.streamed[id] = o
			return sc.stopAfter <= 0 || len(result) < sc.stopAfter, nil
		})
		if err != nil {
			return nil, err
		}
		// Already in identity order, and verified to be.
		return result, nil
	case t.engine.source != nil:
		if t.originals != nil {
			return nil, t.needImage()
		}
		return nil, store.Refuse("internal_error")
	}

	// The complete image ignores stopAfter entirely and returns every match,
	// leaving ordering and truncation to the caller as it always did.
	//
	// That is not an oversight. It used to honour stopAfter too — sort by
	// identity, truncate — which was harmless when the caller was right and
	// fatal to testing it: the stop decision is made by the caller and handed
	// to both paths, so a wrong decision produced the same wrong answer on
	// both, and a test comparing streaming against the whole image agreed with
	// itself. Seeding "stop early under a result sort" passed that test. This
	// path is the reference behaviour, and a reference that shares the logic
	// under test is not one.
	for id, o := range t.state.objects {
		if !readable[o.typeID] {
			continue
		}
		if t.page != nil && t.page.from.After != "" && id <= t.page.from.After {
			continue
		}
		if _, err = consider(id, o); err != nil {
			return nil, err
		}
	}
	if ordered {
		sort.Slice(result, func(i, j int) bool { return result[i].ObjectID < result[j].ObjectID })
	}
	return result, nil
}
func (t *transaction) Matches(typeID, id, parent string, predicate *p.Predicate) (bool, error) {
	if !t.descendant(typeID, parent) {
		return false, store.Refuse("invalid_filter")
	}
	o, err := t.lookup(typeID, id)
	if err != nil {
		return false, err
	}
	meta, err := p.EffectiveMetadata(t.state.types, parent)
	if err != nil {
		return false, err
	}
	if err = predicate.Validate(meta, true); err != nil {
		return false, err
	}
	return predicate.Match(id, o.fields)
}
func (t *transaction) UpdateMetadata(m p.Meta, confirm string) (result p.Meta, err error) {
	defer func() { t.fail(err) }()
	// A field removed from a type is removed from every record of it.
	if t.originals != nil {
		return result, t.needImage()
	}
	if err = t.check(t.auth.Grants.Schema && t.auth.View.Full()); err != nil {
		return result, err
	}
	previous, ok := t.state.types[m.ID]
	if !ok {
		return result, store.Refuse("meta_type_not_found")
	}
	if err = m.ValidateDeclared(); err != nil {
		return result, err
	}
	if err = m.ValidateSemantics(t.engine.limits); err != nil {
		return result, err
	}
	if (previous.ParentID == nil) != (m.ParentID == nil) || (m.ParentID != nil && *m.ParentID != *previous.ParentID) {
		return result, store.Refuse("meta_schema_error")
	}
	current := map[string]p.Field{}
	for _, f := range m.Fields {
		current[f.ID] = f
	}
	removed := []string{}
	for _, f := range previous.Fields {
		now, ok := current[f.ID]
		if !ok {
			removed = append(removed, f.ID)
			continue
		}
		if now.Type != f.Type || now.Creator != f.Creator {
			return result, store.Refuse("meta_schema_error")
		}
		// A scale may be widened and never narrowed. Widening costs nothing
		// because a canonical fixed value carries no trailing zero, so every
		// value already stored is valid at any larger scale and no row is
		// rewritten. Narrowing would invalidate values already written, and
		// the only ways to accept it are to rewrite them — rounding, which
		// this store does not do — or to leave them in a field whose declared
		// bound they exceed.
		was, hadScale := f.FixedScaleOf()
		if scale, hasScale := now.FixedScaleOf(); hadScale && hasScale && scale < was {
			return result, store.Refuse("meta_schema_error")
		}
	}
	if len(removed) > 0 && confirm != m.ID {
		return result, &p.Error{Code: "meta_schema_error", Message: "Field removal requires confirmation", Details: map[string]any{"reason": "confirmation_required", "fields": removed}}
	}
	t.state.types[m.ID] = cloneMeta(m)
	for id := range t.state.types {
		if _, err = p.EffectiveMetadata(t.state.types, id); err != nil {
			return result, err
		}
	}
	if err = t.touch(); err != nil {
		return result, err
	}
	for id, o := range t.state.objects {
		if t.descendant(o.typeID, m.ID) {
			for _, f := range removed {
				delete(o.fields, f)
			}
			t.state.objects[id] = o
		}
	}
	return t.effective(m.ID)
}
func (t *transaction) DeleteMetadata(typeID string, cascade bool) (types []string, count int, err error) {
	defer func() { t.fail(err) }()
	// Deleting a type means knowing, and possibly deleting, its records.
	if t.originals != nil {
		return nil, 0, t.needImage()
	}
	if err = t.check(t.auth.Grants.Schema && t.auth.View.Full()); err != nil {
		return nil, 0, err
	}
	if _, ok := t.state.types[typeID]; !ok {
		return nil, 0, store.Refuse("meta_type_not_found")
	}
	for id := range t.state.types {
		if t.descendant(id, typeID) {
			types = append(types, id)
		}
	}
	sort.Strings(types)
	ids := []string{}
	for id, o := range t.state.objects {
		if t.descendant(o.typeID, typeID) {
			ids = append(ids, id)
		}
	}
	if !cascade && (len(types) > 1 || len(ids) > 0) {
		return nil, 0, &p.Error{Code: "conflict", Message: "Type is not empty", Details: map[string]any{"reason": "type_not_empty"}}
	}
	if err = t.touch(); err != nil {
		return nil, 0, err
	}
	for _, id := range types {
		delete(t.state.types, id)
	}
	for _, id := range ids {
		delete(t.state.objects, id)
	}
	return types, len(ids), nil
}

func (t *transaction) ValidateFields(typeID string, fields map[string]p.Value) error {
	if err := t.check(t.auth.Grants.Write); err != nil {
		return err
	}
	_, _, err := t.convertDeclared(typeID, fields, false)
	return err
}

func (s *Store) digest(key *[32]byte, purpose string, raw []byte) string {
	h := hmac.New(sha256.New, key[:])
	h.Write([]byte(purpose))
	h.Write([]byte{0})
	h.Write([]byte(s.id))
	h.Write([]byte{0})
	h.Write(raw)
	return "s:" + hex.EncodeToString(h.Sum(nil))
}
func (t *transaction) effective(id string) (p.Meta, error) {
	m, err := p.EffectiveMetadata(t.state.types, id)
	if err != nil {
		return m, err
	}
	m.SchemaVersion = ""
	raw, err := p.MarshalWire(m)
	if err != nil {
		return m, err
	}
	m.SchemaVersion = t.engine.digest(&t.engine.secret, "type", raw)
	return m, nil
}
func (t *transaction) ExpectType(id, version string) (err error) {
	defer func() { t.fail(err) }()
	m, err := t.Metadata(id)
	if err != nil {
		return err
	}
	if m.SchemaVersion != version {
		return &p.Error{Code: "conflict", Message: "Schema version did not match", Details: map[string]any{"reason": "schema_version_mismatch"}}
	}
	return nil
}

// SetMediaAuthority installs a validator tied to the enclosing SQL transaction.
func (s *Store) SetMediaAuthority(validate func(string) error) { s.mediaAuthority = validate }

// Provisioned names the datasets this evaluator holds, for composition. There
// is always exactly one. It takes no access, because this is the call that
// names the dataset a first grant is installed on, and no grant could name it.
// Nothing routes a request here — what a caller may see is Datasets.
// Reach names the datasets this authorization holds access on. This evaluator
// holds one dataset and cannot create another, so the answer is that one or
// nothing at all.
func (s *Store) Reach(ctx context.Context, a store.Access) ([]string, error) {
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	if _, held := s.authorizations[a]; !held {
		return nil, nil
	}
	return []string{s.id}, nil
}

func (s *Store) Provisioned(ctx context.Context) ([]store.Dataset, error) {
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	return []store.Dataset{{ID: s.id, DisplayName: s.displayName}}, nil
}

// SoleDataset names the one dataset this evaluator holds.
func (s *Store) SoleDataset(ctx context.Context) (store.Dataset, error) {
	held, err := s.Provisioned(ctx)
	if err != nil {
		return store.Dataset{}, err
	}
	return held[0], nil
}

// sourcedWatermark is Watermark for a sourced write, which holds only the
// records it touched: a write that changed nothing keeps the dataset's
// watermark, decided against the originals it fetched rather than a whole
// image it does not have.
func (t *transaction) sourcedWatermark() string {
	original := t.engine.state
	if t.state.next != original.next || len(t.state.types) != len(original.types) {
		return t.state.watermark
	}
	equal := func(a, b any) bool {
		x, e := p.MarshalWire(a)
		y, f := p.MarshalWire(b)
		return e == nil && f == nil && bytes.Equal(x, y)
	}
	for id, m := range t.state.types {
		old, ok := original.types[id]
		if !ok || !equal(m, old) {
			return t.state.watermark
		}
	}
	for id, old := range t.originals {
		now, ok := t.state.objects[id]
		if !ok || now.typeID != old.typeID || !equal(now.fields, old.fields) {
			return t.state.watermark
		}
	}
	for id := range t.state.objects {
		if _, fetched := t.originals[id]; !fetched {
			return t.state.watermark
		}
	}
	return original.watermark
}

// scanRows streams the records of the given types in identity order. In a
// sourced write the storage has not seen this write's changes, so they are
// laid over what it streams: a record this write changed is visited as it is
// now, one it deleted is not visited, and one it inserted is visited in its
// place in identity order. Without that, a query inside a write would answer
// about the dataset as it was before the write began.
func (t *transaction) scanRows(types []string, visit func(id string, row Row) (bool, error)) error {
	if t.originals == nil {
		return t.engine.scan(types, visit)
	}
	readable := make(map[string]bool, len(types))
	for _, id := range types {
		readable[id] = true
	}
	var added []string
	for id, o := range t.state.objects {
		if _, fetched := t.originals[id]; !fetched && readable[o.typeID] {
			added = append(added, id)
		}
	}
	sort.Strings(added)
	stopped := false
	call := func(id string, row Row) (bool, error) {
		more, err := visit(id, row)
		if err != nil || !more {
			stopped = true
		}
		return more, err
	}
	current := func(id string) (bool, error) {
		o := t.state.objects[id]
		raw, err := p.MarshalWire(o.fields)
		if err != nil {
			return false, err
		}
		return call(id, Row{Sequence: o.sequence, TypeID: o.typeID, Fields: o.fields, Bytes: len(raw)})
	}
	err := t.engine.scan(types, func(id string, row Row) (bool, error) {
		for len(added) > 0 && added[0] < id {
			next := added[0]
			added = added[1:]
			if more, err := current(next); err != nil || !more {
				return false, err
			}
		}
		if o, changed := t.state.objects[id]; changed {
			if !readable[o.typeID] {
				return true, nil
			}
			return current(id)
		}
		if _, fetched := t.originals[id]; fetched {
			// Deleted by this write.
			return true, nil
		}
		return call(id, row)
	})
	if err != nil || stopped {
		return err
	}
	for _, id := range added {
		if more, err := current(id); err != nil || !more {
			return err
		}
	}
	return nil
}

func viewBytes(view store.View) []byte {
	raw, _ := json.Marshal(view)
	return raw
}
