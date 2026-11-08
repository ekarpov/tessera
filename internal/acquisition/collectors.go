package acquisition

import (
	"crypto/sha256"
	"crypto/subtle"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

const CollectorPrefix = "tessera-collector."
const MaxCollectorLifetime = 365 * 24 * time.Hour
const maxCollectors = 64

type Collector struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
type collectorRecord struct {
	Collector
	Digest [32]byte `json:"digest"`
}
type CollectorCredential struct {
	Collector
	Token string `json:"token"`
}

func (m *Manager) loadCollectors() error {
	m.collectors = map[string]collectorRecord{}
	err := readJSON(filepath.Join(m.root, "collectors.json"), &m.collectors)
	if err != nil && !os.IsNotExist(err) {
		return store.Refuse("dataset_unavailable")
	}
	if m.collectors == nil || len(m.collectors) > maxCollectors {
		return store.Refuse("dataset_unavailable")
	}
	for id, c := range m.collectors {
		if !validToken(id) || c.ID != id || !c.ExpiresAt.After(c.CreatedAt) {
			return store.Refuse("dataset_unavailable")
		}
	}
	return nil
}
func (m *Manager) saveCollectors() error {
	if err := atomicJSON(m.root, "collectors.json", m.collectors); err != nil {
		// Credential persistence failure must not stop native backup production.
		// Refuse collector authentication until restart/operator recovery instead.
		m.collectorsUnavailable = true
		return err
	}
	return nil
}
func (m *Manager) ProvisionCollector(id string, lifetime time.Duration) (CollectorCredential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validToken(id) || lifetime < time.Minute || lifetime > MaxCollectorLifetime {
		return CollectorCredential{}, store.Refuse("invalid_field")
	}
	if m.closed || m.collectorsUnavailable {
		return CollectorCredential{}, store.Refuse("dataset_unavailable")
	}
	now := m.now().UTC()
	if _, exists := m.collectors[id]; exists {
		return CollectorCredential{}, store.Refuse("conflict")
	}
	for id, c := range m.collectors {
		if !now.Before(c.ExpiresAt) {
			delete(m.collectors, id)
		}
	}
	if len(m.collectors) >= maxCollectors {
		return CollectorCredential{}, store.Refuse("limit_exceeded")
	}
	credential := CollectorCredential{Collector: Collector{ID: id, CreatedAt: now, ExpiresAt: now.Add(lifetime)}, Token: CollectorPrefix + id + "." + token() + token()}
	m.collectors[id] = collectorRecord{Collector: credential.Collector, Digest: sha256.Sum256([]byte(credential.Token))}
	if err := m.saveCollectors(); err != nil {
		return CollectorCredential{}, err
	}
	return credential, nil
}
func (m *Manager) Collectors() ([]Collector, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.collectorsUnavailable {
		return nil, store.Refuse("dataset_unavailable")
	}
	result := []Collector{}
	for _, c := range m.collectors {
		result = append(result, c.Collector)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
func (m *Manager) RevokeCollector(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validToken(id) {
		return store.Refuse("invalid_field")
	}
	if m.closed || m.collectorsUnavailable {
		return store.Refuse("dataset_unavailable")
	}
	delete(m.collectors, id)
	return m.saveCollectors()
}

// AuthenticateCollector admits only a retrieval request. It does not produce a
// record authorization, identity account, service token or administrator standing.
func (m *Manager) AuthenticateCollector(bearer string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.collectorsUnavailable {
		return store.Refuse("authentication_failed")
	}
	id, secret, ok := strings.Cut(strings.TrimPrefix(bearer, CollectorPrefix), ".")
	if !strings.HasPrefix(bearer, CollectorPrefix) || !ok || !validToken(id) || len(secret) != 64 {
		return store.Refuse("authentication_failed")
	}
	c, exists := m.collectors[id]
	digest := sha256.Sum256([]byte(bearer))
	if !exists || subtle.ConstantTimeCompare(digest[:], c.Digest[:]) != 1 || !m.now().Before(c.ExpiresAt) {
		return store.Refuse("authentication_failed")
	}
	return nil
}
