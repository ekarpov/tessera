package acquisition

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/ekarpov/tessera/pkg/store"
)

const CatalogLifetime = 15 * time.Minute

type Catalog struct {
	Results []Operation `json:"results"`
	Next    string      `json:"next_cursor,omitempty"`
}
type catalogCursor struct {
	Deployment string `json:"deployment"`
	After      string `json:"after"`
	Through    string `json:"through"`
	Limit      int    `json:"limit"`
	Expires    int64  `json:"expires"`
}

// Catalog walks the live retained set in stable ID order, with a fixed upper ID
// bound. Publication behind the cursor is found by the next full rescan; rotation
// may remove unseen entries. It never pins artifacts or promises event history.
func (m *Manager) Catalog(limit int, cursor string) (Catalog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := Catalog{Results: []Operation{}}
	now := m.now()
	c := catalogCursor{Deployment: m.deployment, Limit: limit, Expires: now.Add(CatalogLifetime).Unix()}
	if cursor != "" {
		if len(cursor) > 2048 {
			return result, store.Refuse("invalid_cursor")
		}
		payload, signature, ok := strings.Cut(cursor, ".")
		raw, e1 := base64.RawURLEncoding.DecodeString(payload)
		mac, e2 := base64.RawURLEncoding.DecodeString(signature)
		h := hmac.New(sha256.New, m.key)
		h.Write([]byte("TESSERA-CATALOG-1\n"))
		h.Write(raw)
		if !ok || e1 != nil || e2 != nil || !hmac.Equal(mac, h.Sum(nil)) || json.Unmarshal(raw, &c) != nil || c.Deployment != m.deployment || c.Limit < 1 || c.Limit > 100 || (limit != 0 && limit != c.Limit) {
			return result, store.Refuse("invalid_cursor")
		}
		if now.Unix() >= c.Expires {
			return result, store.Refuse("cursor_expired")
		}
	} else {
		if c.Limit == 0 {
			c.Limit = 50
		}
		if c.Limit < 1 || c.Limit > 100 {
			return result, store.Refuse("invalid_field")
		}
		for id, e := range m.entries {
			if e.State == "complete" && now.Before(e.ExpiresAt) && id > c.Through {
				c.Through = id
			}
		}
	}
	ids := []string{}
	for id, e := range m.entries {
		if e.State == "complete" && now.Before(e.ExpiresAt) && id > c.After && id <= c.Through {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for i, id := range ids {
		if i == c.Limit {
			c.After = ids[i-1]
			raw, _ := json.Marshal(c)
			h := hmac.New(sha256.New, m.key)
			h.Write([]byte("TESSERA-CATALOG-1\n"))
			h.Write(raw)
			result.Next = base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
			break
		}
		result.Results = append(result.Results, copyOperation(m.entries[id]))
	}
	return result, nil
}

// CompletedStatus gives retrieval-only callers no access to in-flight jobs.
func (m *Manager) CompletedStatus(id string) (Operation, error) {
	op, err := m.Status(id)
	if err != nil {
		return Operation{}, err
	}
	if op.State == "expired" {
		return Operation{}, store.Refuse("expired")
	}
	if op.State != "complete" {
		return Operation{}, store.Refuse("unavailable")
	}
	return op, nil
}
