// Package acquisition owns bounded reference-server backup publication. It is
// deliberately separate from either protocol and knows no record identities.
package acquisition

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const Retention = 7 * 24 * time.Hour
const LeaseDuration = 5 * time.Minute
const ExecutionLimit = 10 * time.Minute
const maxStatuses = 1024
const MaxEvidence = 16 << 20
const maxArchive = (256 << 20) + 512
const MACDomain = "TESSERA-ACQUISITION-1\n"

type Artifact struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Pair struct {
	Version    int        `json:"version"`
	Deployment string     `json:"deployment"`
	S          string     `json:"s,omitempty"`
	R          string     `json:"r"`
	Artifacts  []Artifact `json:"artifacts"`
}

// Envelope authenticates the decoded payload bytes, avoiding JSON canonicalization
// requirements for other language implementations.
type Envelope struct {
	Payload []byte `json:"payload"`
	MAC     string `json:"mac"`
}
type Operation struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	State      string     `json:"state"`
	AcceptedAt time.Time  `json:"accepted_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	Failure    string     `json:"failure,omitempty"`
	Pair       *Pair      `json:"pair,omitempty"`
	Artifacts  []Artifact `json:"artifacts,omitempty"`
}
type entry struct {
	Operation
	Set           string    `json:"set"`
	PublishedAt   time.Time `json:"published_at,omitzero"`
	DeletePending bool      `json:"delete_pending,omitempty"`
	leases        map[string]time.Time
	active        int
	reserved      bool
	finishedAt    time.Time
	finishedState string
}

// Capture must create the archive exclusively, then capture evidence after its
// completed export. Evidence-only requests leave archivePath unused.
type Capture func(context.Context, string, string) (s, r string, evidence []byte, err error)
type Manager struct {
	mu                    sync.Mutex
	root                  string
	key                   []byte
	keep                  int
	capture               Capture
	deployment            string
	entries               map[string]*entry
	busy                  bool
	closed                bool
	cancel                context.CancelFunc
	wg                    sync.WaitGroup
	owner                 *os.File
	now                   func() time.Time
	backupPublished       func(time.Time)
	collectors            map[string]collectorRecord
	collectorsUnavailable bool
	// Tests pause publication without timing-dependent concurrency assertions.
	beforePublish           func()
	settingsMu              sync.Mutex
	settings                *settingsDisk
	settingsAdmission       bool
	settingsUnavailable     bool
	settingsWrite           func(*settingsDisk) error
	beforeSettingsEffective func()
	beforePrune             func()
	removeSet               func(string) error
	maintenanceActive       bool
	retentionFailure        string
	controlCtx              context.Context
	stopControl             context.CancelFunc
	controlWake             chan struct{}
	instance                string
	schedulerStarted        bool
	schedulerEverStarted    bool
	lastScheduled           *ScheduledAttempt
	nextScheduled           *time.Time
}

func token() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("entropy unavailable")
	}
	return hex.EncodeToString(b[:])
}
func NewID() string { return strconv.FormatInt(time.Now().Unix(), 10) + "-" + token() }
func validID(id string) (time.Time, bool) {
	seconds, random, ok := strings.Cut(id, "-")
	if !ok || len(random) != 32 {
		return time.Time{}, false
	}
	for _, c := range random {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return time.Time{}, false
		}
	}
	n, err := strconv.ParseInt(seconds, 10, 64)
	return time.Unix(n, 0), err == nil && strconv.FormatInt(n, 10) == seconds
}

// Open recovers interrupted publication/rotation before accepting work. The
// optional callback records backup publication after the complete journal is
// durable, before status exposes completion. It also receives the latest retained
// publication on restart, so it must be idempotent and must not call the manager.
// Evidence-only and unsuccessful operations never invoke it.
func Open(directory string, key []byte, keep int, capture Capture, onBackupPublished ...func(time.Time)) (*Manager, error) {
	return OpenNative(directory, key, 0, keep, nil, capture, onBackupPublished...)
}

// OpenNative loads effective settings before startup rotation. Static values are
// bootstrap only; recovery cannot override an existing journal or deployment.
func OpenNative(directory string, key []byte, everyHours, keep int, recovery *SettingsExport, capture Capture, onBackupPublished ...func(time.Time)) (*Manager, error) {
	if len(key) != 32 || !validSettings(everyHours, keep) || len(onBackupPublished) > 1 {
		return nil, store.Refuse("invalid_field")
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, store.Refuse("dataset_unavailable")
	}
	root := filepath.Join(directory, ".acquisitions")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	owner, err := lockOwner(filepath.Join(root, "owner.lock"))
	if err != nil {
		return nil, store.Refuse("conflict")
	}
	m := &Manager{root: root, key: append([]byte(nil), key...), keep: keep, capture: capture, entries: map[string]*entry{}, owner: owner, now: time.Now}
	if len(onBackupPublished) == 1 {
		m.backupPublished = onBackupPublished[0]
	}
	success := false
	defer func() {
		if !success {
			owner.Close()
		}
	}()
	if err = checkRecoveryState(root); err != nil {
		return nil, err
	}
	if err = readJSON(filepath.Join(root, "deployment.json"), &m.deployment); os.IsNotExist(err) {
		if recovery != nil {
			return nil, store.Refuse("invalid_field")
		}
		m.deployment = token()
		err = atomicJSON(root, "deployment.json", m.deployment)
	}
	if err != nil || len(m.deployment) != 32 {
		return nil, store.Refuse("dataset_unavailable")
	}
	if err = m.loadSettings(everyHours, keep, recovery); err != nil {
		return nil, err
	}
	if err = m.loadCollectors(); err != nil {
		return nil, err
	}
	if err = readJSON(filepath.Join(root, "operations.json"), &m.entries); err != nil && !os.IsNotExist(err) {
		return nil, store.Refuse("dataset_unavailable")
	}
	if m.entries == nil || len(m.entries) > maxStatuses {
		return nil, store.Refuse("dataset_unavailable")
	}
	for id, e := range m.entries {
		if e == nil || e.ID != id || e.Set != "set-"+strings.TrimPrefix(e.Set, "set-") || !validToken(strings.TrimPrefix(e.Set, "set-")) {
			return nil, store.Refuse("dataset_unavailable")
		}
		for _, a := range e.Artifacts {
			if !validToken(a.ID) {
				return nil, store.Refuse("dataset_unavailable")
			}
		}
		if e.State == "complete" {
			for _, a := range e.Artifacts {
				got, checkErr := describe(filepath.Join(root, e.Set, a.ID), a.ID, a.Kind, maxArchive)
				if checkErr != nil || got != a {
					e.State = "failed"
					e.Failure = "publication_unavailable"
					e.Pair = nil
					e.Artifacts = nil
					break
				}
			}
		}
		if e.State == "running" {
			e.State = "failed"
			e.Failure = "interrupted"
			e.PublishedAt = time.Time{}
			e.Pair = nil
			e.Artifacts = nil
		}
	}
	if err = m.reconcileRotation(); err != nil {
		return nil, err
	}
	// No job is resumed: a lost terminal publication is reconciled as failed,
	// never rerun. Only directories referenced by complete status are retained.
	contents, err := os.ReadDir(root)
	if err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	live := map[string]bool{}
	for _, e := range m.entries {
		if e.State == "complete" {
			live[e.Set] = true
		}
	}
	for _, f := range contents {
		if !f.IsDir() && strings.HasPrefix(f.Name(), ".journal-") {
			if os.Remove(filepath.Join(root, f.Name())) != nil {
				return nil, store.Refuse("dataset_unavailable")
			}
		}
		if f.IsDir() && (strings.HasPrefix(f.Name(), "stage-") || strings.HasPrefix(f.Name(), "set-")) && !live[f.Name()] {
			var pending *entry
			for _, e := range m.entries {
				if e.Set == f.Name() && e.DeletePending {
					pending = e
					break
				}
			}
			if err = os.RemoveAll(filepath.Join(root, f.Name())); err != nil {
				if pending == nil {
					return nil, store.Refuse("dataset_unavailable")
				}
				m.retentionFailure = "cleanup_failed"
			} else if pending != nil {
				pending.DeletePending = false
			}
		}
	}
	if err = m.save(); err != nil {
		return nil, err
	}
	if err = syncDir(directory); err != nil {
		return nil, store.Refuse("dataset_unavailable")
	}
	// Publication and the host diagnostic live in separate durable files. Replay
	// the latest success to repair a crash between their writes, without recapture.
	var latest time.Time
	for _, e := range m.entries {
		if e.Kind == "backup" && e.PublishedAt.After(latest) {
			latest = e.PublishedAt
		}
	}
	m.recordPublication(latest)
	m.startControl()
	success = true
	return m, nil
}

func (m *Manager) recordPublication(at time.Time) {
	if !at.IsZero() && m.backupPublished != nil {
		m.backupPublished(at)
	}
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 8<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return store.Refuse("invalid_field")
	}
	return nil
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func atomicJSON(dir, name string, v any) error {
	f, err := os.CreateTemp(dir, ".journal-")
	if err != nil {
		return store.Refuse("dataset_unavailable")
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(v)
	if err == nil {
		err = f.Sync()
	}
	e := f.Close()
	if err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err == nil {
		err = syncDir(dir)
	}
	if err != nil {
		return store.Refuse("dataset_unavailable")
	}
	return nil
}
func (m *Manager) save() error { return atomicJSON(m.root, "operations.json", m.entries) }
func copyOperation(e *entry) Operation {
	raw, _ := json.Marshal(e.Operation)
	var out Operation
	_ = json.Unmarshal(raw, &out)
	return out
}
func (m *Manager) expire() error {
	now := m.now()
	// Batch expiry must reserve a survivor even when several archives are
	// complete. The single-final-archive check becomes true only after mutation,
	// too late to protect a whole batch. Prefer newest acceptance, then ID.
	var last *entry
	for _, e := range m.entries {
		if e.State == "complete" && e.Kind == "backup" && (last == nil || e.AcceptedAt.After(last.AcceptedAt) || e.AcceptedAt.Equal(last.AcceptedAt) && e.ID > last.ID) {
			last = e
		}
	}
	changed := false
	for id, e := range m.entries {
		if !now.Before(e.ExpiresAt) && e.State != "running" && e != last && e.active == 0 && !protected(e, now) && !e.reserved {
			if e.State == "complete" {
				e.State = "expired"
				e.Pair = nil
				e.Artifacts = nil
				e.DeletePending = true
				changed = true
			} else if !e.DeletePending {
				delete(m.entries, id)
				changed = true
			}
		}
	}
	if changed {
		if err := m.save(); err != nil {
			m.closed = true
			m.retentionFailure = "journal_unavailable"
			return err
		}
		m.signalControl()
	}
	return nil
}
func (m *Manager) Status(id string) (Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return Operation{}, missing(id, m.now())
	}
	if !m.now().Before(e.ExpiresAt) {
		out := copyOperation(e)
		out.State = "expired"
		out.Pair = nil
		out.Artifacts = nil
		return out, nil
	}
	return copyOperation(e), nil
}
func missing(id string, now time.Time) error {
	issued, ok := validID(id)
	if !ok {
		return store.Refuse("invalid_field")
	}
	if now.Sub(issued) >= Retention {
		return store.Refuse("expired")
	}
	return store.Refuse("not_found")
}
func (m *Manager) Start(id, kind string) (Operation, error) {
	if kind != "backup" && kind != "evidence" {
		return Operation{}, store.Refuse("invalid_field")
	}
	issued, ok := validID(id)
	if !ok {
		return Operation{}, store.Refuse("invalid_field")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[id]; e != nil {
		if e.Kind != kind {
			return Operation{}, store.Refuse("conflict")
		}
		if !m.now().Before(e.ExpiresAt) {
			return Operation{}, store.Refuse("expired")
		}
		return copyOperation(e), nil
	}
	now := m.now()
	if issued.Before(now.Add(-5*time.Minute)) || issued.After(now.Add(5*time.Minute)) {
		return Operation{}, store.Refuse("expired")
	}
	if m.closed {
		return Operation{}, store.Refuse("dataset_unavailable")
	}
	if m.settingsUnavailable {
		return Operation{}, store.RefuseWith("dataset_unavailable", "settings_unavailable", nil)
	}
	if m.settingsAdmission || pendingSettings(m.settings) != "" {
		return Operation{}, store.RefuseWith("limit_exceeded", "settings_pending", nil)
	}
	if m.maintenanceActive {
		return Operation{}, store.RefuseWith("limit_exceeded", "retention_pending", nil)
	}
	if m.busy {
		return Operation{}, store.RefuseWith("limit_exceeded", "busy", nil)
	}
	if err := m.expire(); err != nil {
		return Operation{}, err
	}
	if m.retainedLocked() > m.keep {
		return Operation{}, store.RefuseWith("limit_exceeded", "retention_pending", nil)
	}
	for _, held := range m.entries {
		if held.DeletePending {
			return Operation{}, store.RefuseWith("limit_exceeded", "retention_pending", nil)
		}
	}
	if len(m.entries) >= maxStatuses {
		return Operation{}, store.Refuse("limit_exceeded")
	}
	// Reserve a rotation candidate before accepting work. A new lease cannot
	// consume capacity already promised to this job.
	var victim *entry
	lastArchive := m.finalArchiveLocked()
	count := 0
	for _, e := range m.entries {
		if e.State == "complete" {
			count++
			if !(kind == "evidence" && e == lastArchive) && e.active == 0 && !e.reserved && !protected(e, now) && (victim == nil || e.AcceptedAt.Before(victim.AcceptedAt)) {
				victim = e
			}
		}
	}
	if count > m.keep || count >= m.keep && victim == nil {
		if kind == "evidence" && lastArchive != nil {
			return Operation{}, store.RefuseWith("limit_exceeded", "last_recovery_pair", nil)
		}
		return Operation{}, store.Refuse("limit_exceeded")
	}
	if count < m.keep {
		victim = nil
	}
	if victim != nil {
		victim.reserved = true
	}
	e := &entry{Operation: Operation{ID: id, Kind: kind, State: "running", AcceptedAt: now.UTC(), ExpiresAt: now.Add(Retention).UTC()}, Set: "set-" + token()}
	m.entries[id] = e
	if err := m.save(); err != nil {
		e.State = "failed"
		e.Failure = "journal_unavailable"
		m.closed = true
		if victim != nil {
			victim.reserved = false
		}
		return Operation{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), ExecutionLimit)
	m.cancel = cancel
	m.busy = true
	m.wg.Add(1)
	go m.run(ctx, cancel, e, victim)
	return copyOperation(e), nil
}
func writeFile(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	e := f.Close()
	if err == nil {
		err = e
	}
	return err
}
func describe(path, id, kind string, maximum int64) (Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		return Artifact{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maximum+1))
	if err != nil || n > maximum {
		return Artifact{}, store.Refuse("limit_exceeded")
	}
	return Artifact{ID: id, Kind: kind, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, e, victim *entry) {
	defer m.wg.Done()
	defer cancel()
	stageName := "stage-" + strings.TrimPrefix(e.Set, "set-")
	stage := filepath.Join(m.root, stageName)
	err := os.Mkdir(stage, 0700)
	archiveID := token()
	archivePath := filepath.Join(stage, archiveID)
	var s, r string
	var evidence []byte
	if err == nil {
		s, r, evidence, err = m.capture(ctx, e.Kind, archivePath)
	}
	pair := &Pair{Version: 1, Deployment: m.deployment, S: s, R: r}
	var artifacts []Artifact
	if err == nil && (r == "" || e.Kind == "backup" && s == "") {
		err = store.Refuse("internal_error")
	}
	if err == nil && e.Kind == "backup" {
		var a Artifact
		a, err = describe(archivePath, archiveID, "archive", maxArchive)
		artifacts = append(artifacts, a)
	}
	evidenceID := token()
	if err == nil && len(evidence) > MaxEvidence {
		err = store.Refuse("limit_exceeded")
	}
	if err == nil {
		err = writeFile(filepath.Join(stage, evidenceID), evidence)
	}
	if err == nil {
		var a Artifact
		a, err = describe(filepath.Join(stage, evidenceID), evidenceID, "evidence", MaxEvidence)
		artifacts = append(artifacts, a)
	}
	if err == nil {
		pair.Artifacts = append([]Artifact(nil), artifacts...)
		payload, _ := json.Marshal(pair)
		mac := hmac.New(sha256.New, m.key)
		mac.Write([]byte(MACDomain))
		mac.Write(payload)
		raw, _ := json.Marshal(Envelope{Payload: payload, MAC: hex.EncodeToString(mac.Sum(nil))})
		id := token()
		err = writeFile(filepath.Join(stage, id), raw)
		if err == nil {
			var a Artifact
			a, err = describe(filepath.Join(stage, id), id, "metadata", 1<<20)
			artifacts = append(artifacts, a)
		}
	}
	if err == nil && m.beforePublish != nil {
		m.beforePublish()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = syncDir(stage)
	}
	if err == nil {
		err = os.Rename(stage, filepath.Join(m.root, e.Set))
	}
	if err == nil {
		err = syncDir(m.root)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil && ctx.Err() == nil {
		e.State = "complete"
		e.Pair = pair
		e.Artifacts = artifacts
		if e.Kind == "backup" {
			e.PublishedAt = m.now().UTC()
		}
		if err = m.save(); err == nil {
			m.recordPublication(e.PublishedAt)
			if victim != nil {
				// Admission reserved the victim, so it cannot gain a lease or reader.
				// A rotation failure cannot undo an already durable publication.
				if m.persistRetirement(victim) != nil {
					m.retentionFailure = "journal_unavailable"
					m.closed = true
				}
			}
		}
	}
	if err != nil || e.State != "complete" {
		e.State = "failed"
		e.Failure = "acquisition_failed"
		e.PublishedAt = time.Time{}
		switch p.ErrorCode(err) {
		case "not_supported", "query_result_limit_exceeded", "limit_exceeded", "store_unavailable", "dataset_unavailable":
			e.Failure = p.ErrorCode(err)
		}
		if ctx.Err() == context.DeadlineExceeded {
			e.Failure = "deadline_exceeded"
		}
		e.Pair = nil
		e.Artifacts = nil
		if ctx.Err() == context.Canceled {
			e.State = "cancelled"
			e.Failure = ""
		}
		if m.save() != nil {
			m.closed = true
		}
		if os.RemoveAll(stage) != nil {
			m.closed = true
		}
		if os.RemoveAll(filepath.Join(m.root, e.Set)) != nil {
			m.closed = true
		}
	}
	if victim != nil {
		victim.reserved = false
	}
	e.finishedAt = m.now().UTC()
	e.finishedState = e.State
	m.finishScheduledLocked(e)
	m.busy = false
	m.cancel = nil
	m.signalControl()
}
func (m *Manager) Cancel(id string) (Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		return Operation{}, missing(id, m.now())
	}
	if e.State == "running" && m.cancel != nil {
		m.cancel()
	}
	return copyOperation(e), nil
}
func protected(e *entry, now time.Time) bool {
	held := false
	for id, expiry := range e.leases {
		if !now.Before(expiry) {
			delete(e.leases, id)
		} else {
			held = true
		}
	}
	return held
}
func validToken(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (m *Manager) Lease(id, lease string, release bool) (time.Time, error) {
	if !validToken(lease) {
		return time.Time{}, store.Refuse("invalid_field")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		return time.Time{}, missing(id, m.now())
	}
	if e.State != "complete" || !m.now().Before(e.ExpiresAt) {
		return time.Time{}, store.Refuse("expired")
	}
	if release {
		delete(e.leases, lease)
		m.signalControl()
		return time.Time{}, nil
	}
	if e.reserved {
		return time.Time{}, store.Refuse("limit_exceeded")
	}
	now := m.now()
	count := 0
	for _, held := range m.entries {
		protected(held, now)
		count += len(held.leases)
	}
	if _, ok := e.leases[lease]; !ok && (len(e.leases) >= 8 || count >= 64) {
		return time.Time{}, store.Refuse("limit_exceeded")
	}
	expiry := now.Add(LeaseDuration)
	if e.ExpiresAt.Before(expiry) {
		expiry = e.ExpiresAt
	}
	if e.leases == nil {
		e.leases = map[string]time.Time{}
	}
	e.leases[lease] = expiry
	return expiry.UTC(), nil
}

type Read struct {
	*os.File
	once sync.Once
	done func()
}

func (r *Read) Close() error { err := r.File.Close(); r.once.Do(r.done); return err }
func (m *Manager) Read(id, artifact, lease string) (*Read, Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		return nil, Artifact{}, missing(id, m.now())
	}
	if e.State != "complete" || !m.now().Before(e.ExpiresAt) || !m.now().Before(e.leases[lease]) || e.reserved {
		return nil, Artifact{}, store.Refuse("expired")
	}
	active := 0
	for _, held := range m.entries {
		active += held.active
	}
	if active >= 8 {
		return nil, Artifact{}, store.Refuse("limit_exceeded")
	}
	for _, a := range e.Artifacts {
		if a.ID == artifact {
			f, err := os.Open(filepath.Join(m.root, e.Set, a.ID))
			if err != nil {
				return nil, Artifact{}, store.Refuse("dataset_unavailable")
			}
			e.active++
			return &Read{File: f, done: func() { m.mu.Lock(); e.active--; m.signalControl(); m.mu.Unlock() }}, a, nil
		}
	}
	return nil, Artifact{}, store.Refuse("not_found")
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	if m.stopControl != nil {
		m.stopControl()
	}
	m.wg.Wait()
	m.settingsMu.Lock()
	m.settingsMu.Unlock()
	m.owner.Close()
}

// ArchivePath is used only by the compatible local BackupNow adapter.
func (m *Manager) ArchivePath(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e != nil && e.State == "complete" {
		for _, a := range e.Artifacts {
			if a.Kind == "archive" {
				return filepath.Join(m.root, e.Set, a.ID), nil
			}
		}
	}
	return "", store.Refuse("dataset_unavailable")
}

// Verify authenticates an envelope with a separately retained key. Artifacts
// still need size/digest checks before restore; deployment must be expected.
func Verify(raw, key []byte, deployment string) (Pair, error) {
	var env Envelope
	var pair Pair
	if len(raw) > 1<<20 || json.Unmarshal(raw, &env) != nil {
		return pair, store.Refuse("invalid_field")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(MACDomain))
	mac.Write(env.Payload)
	provided, err := hex.DecodeString(env.MAC)
	if err != nil || len(key) != 32 || !hmac.Equal(provided, mac.Sum(nil)) || json.Unmarshal(env.Payload, &pair) != nil || pair.Version != 1 || pair.Deployment != deployment {
		return Pair{}, store.Refuse("invalid_field")
	}
	return pair, nil
}
