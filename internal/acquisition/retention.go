package acquisition

import (
	"os"
	"path/filepath"
	"sort"
)

type RetentionObservation struct {
	Retained int    `json:"retained"`
	Target   int    `json:"target"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
}

func (m *Manager) retainedLocked() int {
	n := 0
	for _, e := range m.entries {
		if e.State == "complete" || e.DeletePending {
			n++
		}
	}
	return n
}
func (m *Manager) finalArchiveLocked() *entry {
	var last *entry
	for _, e := range m.entries {
		if e.State == "complete" && e.Kind == "backup" {
			if last != nil {
				return nil
			}
			last = e
		}
	}
	return last
}
func (m *Manager) retentionLocked() RetentionObservation {
	out := RetentionObservation{Retained: m.retainedLocked(), Target: m.keep, State: "converged", Reason: "at_target"}
	if m.retentionFailure != "" {
		out.State = "degraded"
		out.Reason = m.retentionFailure
		return out
	}
	pending := false
	for _, e := range m.entries {
		if e.DeletePending {
			pending = true
		}
	}
	if out.Retained <= m.keep && !pending {
		return out
	}
	if m.busy {
		out.State = "pending"
		out.Reason = "acquisition_in_progress"
		return out
	}
	out.State = "pending"
	out.Reason = "pruning"
	if pending || m.maintenanceActive {
		return out
	}
	last := m.finalArchiveLocked()
	leases, transfers, reserved := false, false, false
	now := m.now()
	for _, e := range m.entries {
		if e.State != "complete" || e == last {
			continue
		}
		if e.active > 0 {
			transfers = true
			continue
		}
		if protected(e, now) {
			leases = true
			continue
		}
		if e.reserved {
			reserved = true
			continue
		}
		return out
	}
	out.State = "blocked"
	if transfers {
		out.Reason = "transfers"
	} else if leases {
		out.Reason = "leases"
	} else if reserved {
		out.Reason = "reserved"
	}
	return out
}

// persistRetirement is called with mu for live publication, and exclusively at
// startup. File deletion is a separate owned task, never under the retrieval lock.
func (m *Manager) persistRetirement(e *entry) error {
	e.State = "expired"
	e.Pair = nil
	e.Artifacts = nil
	e.DeletePending = true
	return m.save()
}
func (m *Manager) reconcileRotation() error {
	var complete []*entry
	for _, e := range m.entries {
		if e.State == "complete" {
			complete = append(complete, e)
		}
	}
	sort.Slice(complete, func(i, j int) bool {
		if complete[i].AcceptedAt.Equal(complete[j].AcceptedAt) {
			return complete[i].ID < complete[j].ID
		}
		return complete[i].AcceptedAt.Before(complete[j].AcceptedAt)
	})
	remaining := len(complete)
	for _, e := range complete {
		if remaining <= m.keep {
			break
		}
		if e == m.finalArchiveLocked() {
			continue
		}
		if err := m.persistRetirement(e); err != nil {
			return err
		}
		remaining--
	}
	return nil
}
func (m *Manager) pruneOne() bool {
	m.mu.Lock()
	if m.closed || m.busy || m.maintenanceActive || m.settingsUnavailable || m.settingsAdmission || pendingSettings(m.settings) != "" {
		m.mu.Unlock()
		return false
	}
	var victim *entry
	for _, e := range m.entries {
		if e.DeletePending {
			victim = e
			break
		}
	}
	if victim == nil && m.retainedLocked() > m.keep {
		last := m.finalArchiveLocked()
		now := m.now()
		for _, e := range m.entries {
			if e.State != "complete" || e == last || e.active > 0 || e.reserved || protected(e, now) {
				continue
			}
			if victim == nil || e.AcceptedAt.Before(victim.AcceptedAt) {
				victim = e
			}
		}
		if victim != nil {
			if err := m.persistRetirement(victim); err != nil {
				m.retentionFailure = "journal_unavailable"
				m.closed = true
				m.mu.Unlock()
				return false
			}
		}
	}
	if victim == nil {
		m.mu.Unlock()
		return false
	}
	m.maintenanceActive = true
	hook, remove := m.beforePrune, m.removeSet
	if remove == nil {
		remove = os.RemoveAll
	}
	path := filepath.Join(m.root, victim.Set)
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	err := remove(path)
	if err == nil {
		err = syncDir(m.root)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maintenanceActive = false
	if err != nil {
		m.retentionFailure = "cleanup_failed"
		return false
	}
	victim.DeletePending = false
	if err = m.save(); err != nil {
		m.retentionFailure = "journal_unavailable"
		m.closed = true
		return false
	}
	m.retentionFailure = ""
	return true
}
