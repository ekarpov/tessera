package standalone

import (
	"context"
	"errors"
	"github.com/ekarpov/tessera/internal/acquisition"
	"os"
	"path/filepath"
	"time"
)

// BackupNow is the synchronous compatibility adapter over durable acquisition.
// The returned local archive path is part of a published pair. Remote callers
// should use operation IDs and leases to reconcile responses and protect copies.
func (r *Runtime) BackupNow(ctx context.Context) (string, error) {
	if r.Acquisitions == nil {
		return "", errors.New("backups are not configured")
	}
	id := acquisition.NewID()
	if _, err := r.Acquisitions.Start(id, "backup"); err != nil {
		return "", err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		op, err := r.Acquisitions.Status(id)
		if err != nil {
			return "", err
		}
		switch op.State {
		case "complete":
			return r.Acquisitions.ArchivePath(id)
		case "failed", "cancelled", "expired":
			return "", errors.New("backup failed")
		}
		select {
		case <-ctx.Done():
			_, _ = r.Acquisitions.Cancel(id)
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// StartBackups starts the native scheduler once, including while disabled so
// live enablement can schedule an immediate attempt. Acquisition owns its workers.
func (r *Runtime) StartBackups(ctx context.Context) {
	if r.Acquisitions != nil {
		r.Acquisitions.StartScheduler(r.backupContext)
	}
}

// RegistryFiles names the identity registry for a backup when there is one.
// The registry is written on its first change, so a server whose accounts
// have never changed has none — and asking a backup to include a file that
// does not exist failed the whole backup, offline as well as online.
func RegistryFiles(dataDir string) []string {
	path := filepath.Join(dataDir, "identity.json")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return []string{path}
}
