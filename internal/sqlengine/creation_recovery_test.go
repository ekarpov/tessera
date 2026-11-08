package sqlengine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

// A creation's identity is recoverable by its owner for as long as the receipt
// lives, whatever happens to the dataset or to the owner's authority over it.
// That is the whole point of settling the response as an identity: nothing in
// it depends on current access, so nothing about it can stop being disclosable.
//
// Runs on both engines because the receipt machinery is shared, and the review
// that asked for these cases found creation proven only on SQLite.
func TestACreationIdentityOutlivesTheAccessThatMadeIt(t *testing.T) {
	for _, engine := range []struct {
		name   string
		open   func(*testing.T) *Store
		reopen func(*testing.T, *Store) *Store
	}{
		{
			name: "sqlite",
			open: func(t *testing.T) *Store { s, _, _ := fixture(t); return s },
		},
		{
			name: "postgres",
			open: func(t *testing.T) *Store { postgresAdmin(t); return postgresFixture(t) },
			reopen: func(t *testing.T, s *Store) *Store {
				s.Close()
				return postgresFixture(t)
			},
		},
	} {
		t.Run(engine.name, func(t *testing.T) {
			s := engine.open(t)
			creator := store.Access{Principal: "creator", Authorization: "app"}
			stranger := store.Access{Principal: "stranger", Authorization: "app"}
			const key = "7c19a4e8-0000-4000-8000-0000000000aa"
			digest := [32]byte{7}

			made, err := s.CreateDataset(t.Context(), creator, "Second quarter", p.Limits{}, nil, key, digest)
			if err != nil {
				t.Fatal(err)
			}
			if made.DatasetID == "" || made.Replayed {
				t.Fatal("a first creation reported itself as a replay", made)
			}

			identity := func(t *testing.T, s *Store, why string) {
				t.Helper()
				again, e := s.CreateDataset(t.Context(), creator, "Second quarter", p.Limits{}, nil, key, digest)
				if e != nil || again.DatasetID != made.DatasetID || !again.Replayed {
					t.Fatal("replay did not return the retained identity "+why, again, e)
				}
				r, e := s.Status(t.Context(), creator, store.ServerScope, key)
				if e != nil || r.State != "committed" || !strings.Contains(string(r.Result), made.DatasetID) {
					t.Fatal("status did not carry the identity "+why, r, e)
				}
				if r.ResultWithheld {
					t.Fatal("an identity outcome was withheld " + why)
				}
			}

			identity(t, s, "immediately after creating")

			// The creator loses its grant on what it made.
			if err = s.Revoke(t.Context(), made.DatasetID, creator); err != nil {
				t.Fatal(err)
			}
			identity(t, s, "after the creator's grant was withdrawn")

			// And a stranger still cannot have it: the receipt is owner-bound,
			// and nothing above loosened that.
			if _, err = s.Status(t.Context(), stranger, store.ServerScope, key); p.ErrorCode(err) != "permission_denied" {
				t.Fatal("another principal read the creation receipt", err)
			}

			// The same key for different work is a conflict, not a second answer.
			if _, err = s.CreateDataset(t.Context(), creator, "Third quarter", p.Limits{}, nil, key, [32]byte{9}); p.ErrorCode(err) != "conflict" {
				t.Fatal("a reused key answered different work", err)
			}

			// An unclaimed key is unknown rather than refused.
			unknown, err := s.Status(t.Context(), creator, store.ServerScope, "7c19a4e8-0000-4000-8000-0000000000bb")
			if err != nil || unknown.State != "unknown" {
				t.Fatal("an unclaimed key was not unknown", unknown, err)
			}

			if engine.reopen != nil {
				s = engine.reopen(t, s)
				identity(t, s, "across a restart")
			}
		})
	}
}

// Erasure retires a dataset and scrubs the receipts that named it. A creation
// receipt names no dataset, so it survives — which matters because the identity
// it holds is the only record the caller has of what it made.
func TestErasureDoesNotTakeTheCreationReceiptWithIt(t *testing.T) {
	s, _, _ := fixture(t)
	creator := store.Access{Principal: "creator", Authorization: "app"}
	const key = "7c19a4e8-0000-4000-8000-0000000000cc"
	made, err := s.CreateDataset(t.Context(), creator, "Second quarter", p.Limits{}, nil, key, [32]byte{3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Erase(t.Context(), creator, made.DatasetID, made.DatasetID); err != nil {
		t.Fatal(err)
	}
	r, err := s.Status(t.Context(), creator, store.ServerScope, key)
	if err != nil || r.State != "committed" || !strings.Contains(string(r.Result), made.DatasetID) {
		t.Fatal("the creation receipt did not survive erasing what it made", r, err)
	}
	again, err := s.CreateDataset(t.Context(), creator, "Second quarter", p.Limits{}, nil, key, [32]byte{3})
	if err != nil || again.DatasetID != made.DatasetID || !again.Replayed {
		t.Fatal("replay after erasure did not return the retained identity", again, err)
	}
}

// A restart must not resolve a live claim as abandoned, and must not leave a
// crashed one unresolved forever. SQLite reopens the same file, which is what
// a restart is from the receipt's side.
func TestACreationSurvivesTheProcessThatMadeIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dataset.db")
	first, err := Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	creator := store.Access{Principal: "creator", Authorization: "app"}
	const key = "7c19a4e8-0000-4000-8000-0000000000dd"
	made, err := first.CreateDataset(t.Context(), creator, "Second quarter", p.Limits{}, nil, key, [32]byte{5})
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(t.Context(), path, p.DevelopmentLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	r, err := second.Status(t.Context(), creator, store.ServerScope, key)
	if err != nil || r.State != "committed" || !strings.Contains(string(r.Result), made.DatasetID) {
		t.Fatal("a committed creation was not recoverable after restart", r, err)
	}
	again, err := second.CreateDataset(t.Context(), creator, "Second quarter", p.Limits{}, nil, key, [32]byte{5})
	if err != nil || again.DatasetID != made.DatasetID || !again.Replayed {
		t.Fatal("a restart made the replay create a second dataset", again, err)
	}
	// And exactly one dataset came of it.
	held, err := second.Provisioned(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, d := range held {
		if d.ID == made.DatasetID {
			seen++
		}
	}
	if seen != 1 || len(held) != 2 {
		t.Fatal("the restart or the replay made a second dataset", held)
	}
}

// The advertised window is the one that governs. A server-scoped receipt used
// to fall into the sweep for receipts whose dataset is gone, which prunes by
// the storage's *dataset* window — so discovery promised a week while the
// receipt was deleted after a day. Promising a window and not keeping it is
// worse than promising none, because a client plans reconciliation against it.
func TestACreationReceiptKeepsTheWindowItWasPromised(t *testing.T) {
	// A storage whose dataset window is shorter than the server's. It is the
	// floor rather than a minute: a window below
	// p.MinimumIdempotencyRetentionSeconds is no longer a legal thing for a
	// deployment to promise, so a test that needed one was testing a dataset
	// nobody can create. Ages below move instead.
	limits := p.DevelopmentLimits()
	limits.IdempotencyRetentionSeconds = p.MinimumIdempotencyRetentionSeconds
	path := filepath.Join(t.TempDir(), "dataset.db")
	s, err := Open(t.Context(), path, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if s.ServerRetentionSeconds() <= limits.IdempotencyRetentionSeconds {
		t.Fatal("this test needs a server window longer than the dataset's")
	}
	creator := store.Access{Principal: "creator", Authorization: "app"}
	const key = "7c19a4e8-0000-4000-8000-0000000000ee"
	made, err := s.CreateDataset(t.Context(), creator, "Second quarter", p.Limits{}, nil, key, [32]byte{11})
	if err != nil {
		t.Fatal(err)
	}
	// Erase what it made, so the receipt names no live dataset — which is the
	// state that used to hand it to the wrong sweep.
	if _, err = s.Erase(t.Context(), creator, made.DatasetID, made.DatasetID); err != nil {
		t.Fatal(err)
	}
	// Age it well past the dataset window and nowhere near the server's.
	aged := time.Now().Add(-48 * time.Hour).UnixMilli()
	if _, err = s.db.ExecContext(t.Context(), `UPDATE "tessera_receipts" SET "claimed_at"=$1 WHERE "dataset_id"=$2`, aged, store.ServerScope); err != nil {
		t.Fatal(err)
	}
	if err = s.PruneReceipts(t.Context()); err != nil {
		t.Fatal(err)
	}
	r, err := s.Status(t.Context(), creator, store.ServerScope, key)
	if err != nil || r.State != "committed" {
		t.Fatal("a creation receipt was swept by the dataset window it was never under", r, err)
	}
	// And the promise is kept per receipt, so it does expire on its own terms.
	if _, err = s.db.ExecContext(t.Context(), `UPDATE "tessera_receipts" SET "retain_until"=$1 WHERE "dataset_id"=$2`, time.Now().Add(-time.Minute).UnixMilli(), store.ServerScope); err != nil {
		t.Fatal(err)
	}
	if err = s.PruneReceipts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, err = s.Status(t.Context(), creator, store.ServerScope, key); err != nil || r.State != "unknown" {
		t.Fatal("a receipt past its own deadline was retained anyway", r, err)
	}
}

// A cancelled request says this process stopped watching, not that nothing
// committed. Recording a definitive failure for it would give a caller a wrong
// answer it has no reason to doubt.
//
// This exercises the decision directly rather than through a creation. Reaching
// the interesting window end-to-end means cancelling after the claim commits
// and before the work does, which is not deterministically reachable from
// outside — and a test that cancels up front proves nothing, because the claim
// is never made and the answer is `unknown` either way. An earlier version of
// this test did exactly that and passed against the defect it was written for.
func TestUncertaintyIsNotRecordedAsFailure(t *testing.T) {
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	for _, c := range []struct {
		why      string
		ctx      context.Context
		failure  error
		definite bool
	}{
		{"a refusal this store produced", t.Context(), store.Refuse("invalid_field"), true},
		{"a conflict", t.Context(), store.Refuse("conflict"), true},
		{"a cancelled request", cancelled, store.Refuse("invalid_field"), false},
		{"a cancelled request carrying no refusal", cancelled, errSynthetic, false},
		{"an internal error, which says the store lost track", t.Context(), store.Refuse("internal_error"), false},
		{"a bare error from below", t.Context(), errSynthetic, false},
	} {
		if got := definitelyNothingCommitted(c.ctx, c.failure); got != c.definite {
			t.Fatal(c.why+": expected definite", c.definite, "got", got)
		}
	}
}

var errSynthetic = errors.New("synthetic")
