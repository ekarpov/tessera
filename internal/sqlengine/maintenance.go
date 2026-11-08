package sqlengine

import (
	"context"
	"time"
)

// StartMaintenance owns periodic cleanup. Call once during host composition;
// Close cancels and joins it before closing SQL connections.
func (s *Store) StartMaintenance(ctx context.Context) {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if s.stopMaintenance != nil {
		return
	}
	worker, cancel := context.WithCancel(ctx)
	s.stopMaintenance = cancel
	s.maintenance.Add(1)
	go func() {
		defer s.maintenance.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			run, stop := context.WithTimeout(worker, 30*time.Second)
			_ = s.CleanupContent(run)
			_ = s.CheckpointCleanup(run)
			_ = s.PruneReceipts(run)
			// An executor can die at any time, not only before this one
			// started, so its claims are resolved on a schedule rather than
			// only at startup.
			_ = s.ResolveAbandonedClaims(run)
			stop()
			select {
			case <-worker.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
