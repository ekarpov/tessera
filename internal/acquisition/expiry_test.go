package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

func TestBatchExpiryPreservesRecoveryPairThroughPruningAndFailedReplacement(t *testing.T) {
	for _, tied := range []bool{false, true} {
		name := "newest_accepted"
		if tied {
			name = "acceptance_time_tie"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			var fail atomic.Bool
			m := nativeFixture(t, 0, 2, func(ctx context.Context, kind, path string) (string, string, []byte, error) {
				if fail.Load() {
					return "", "", nil, store.Refuse("store_unavailable")
				}
				return testCapture(&calls)(ctx, kind, path)
			})
			first := publishSet(t, m, "backup")
			second := publishSet(t, m, "backup")
			// Stop only the control worker so expiry and pruning can be stepped without
			// racing the assertions. Acquisition workers remain available.
			m.stopControl()
			m.wg.Wait()
			base := time.Now().UTC()
			m.mu.Lock()
			m.entries[first.ID].AcceptedAt = base
			m.entries[second.ID].AcceptedAt = base.Add(time.Minute)
			survivor := second
			retired := first
			if tied {
				m.entries[second.ID].AcceptedAt = base
				if first.ID > second.ID {
					survivor, retired = first, second
				}
			}
			survivorSet := m.entries[survivor.ID].Set
			retiredSet := m.entries[retired.ID].Set
			later := m.entries[second.ID].ExpiresAt.Add(time.Hour)
			m.mu.Unlock()
			advanceClock(m, later)
			// Admission first expires old statuses, then refuses until cleanup finishes.
			if _, err := m.Start(fmtID(later.Unix()), "backup"); p.ErrorCode(err) != "limit_exceeded" {
				t.Fatal("expected deferred-cleanup refusal", err)
			}
			assertSurvivor := func(pending int) {
				t.Helper()
				m.mu.Lock()
				complete, deleting := 0, 0
				for _, e := range m.entries {
					if e.Kind == "backup" && e.State == "complete" {
						complete++
					}
					if e.DeletePending {
						deleting++
					}
				}
				kept := m.entries[survivor.ID]
				intact := kept != nil && kept.State == "complete" && !kept.DeletePending && kept.Pair != nil
				m.mu.Unlock()
				if complete != 1 || deleting != pending || !intact {
					t.Fatalf("recovery inventory: %d complete archives, %d pending deletions, expected survivor intact=%v", complete, deleting, intact)
				}
				for _, artifact := range survivor.Artifacts {
					got, err := describe(filepath.Join(m.root, survivorSet, artifact.ID), artifact.ID, artifact.Kind, maxArchive)
					if err != nil || got != artifact {
						t.Fatal("surviving recovery artifact changed", artifact.Kind, err)
					}
				}
			}
			assertSurvivor(1)
			var disk map[string]*entry
			if err := readJSON(filepath.Join(m.root, "operations.json"), &disk); err != nil {
				t.Fatal(err)
			}
			if disk[survivor.ID].State != "complete" || disk[survivor.ID].DeletePending || !disk[retired.ID].DeletePending {
				t.Fatal("expiry did not durably preserve the deterministic survivor")
			}
			if !m.pruneOne() {
				t.Fatal("expired archive was not pruned")
			}
			assertSurvivor(0)
			if _, err := os.Stat(filepath.Join(m.root, retiredSet)); !os.IsNotExist(err) {
				t.Fatal("retired archive directory remains", err)
			}

			// At keep=1, a failed replacement reserves this very archive as its victim.
			// It must release that reservation without retiring the last usable pair.
			settingsID, payload := settingsRequest(m, 0, 1)
			if _, err := m.SubmitSettings(settingsID, payload); err != nil {
				t.Fatal(err)
			}
			if !m.applySettings() {
				t.Fatal("retention policy did not become effective")
			}
			fail.Store(true)
			replacement := fmtID(later.Unix())
			if _, err := m.Start(replacement, "backup"); err != nil {
				t.Fatal(err)
			}
			if op := waitTerminal(t, m, replacement); op.State != "failed" {
				t.Fatal("replacement did not fail", op)
			}
			if m.pruneOne() {
				t.Fatal("failed replacement scheduled recovery-pair pruning")
			}
			assertSurvivor(0)
			m.mu.Lock()
			reserved := m.entries[survivor.ID].reserved
			m.mu.Unlock()
			if reserved {
				t.Fatal("failed replacement retained its victim reservation")
			}
		})
	}
}
