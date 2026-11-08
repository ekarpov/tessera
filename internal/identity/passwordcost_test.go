package identity

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Passwords hash at a test cost in this package; see LowerDerivationCostForTests.
func TestMain(m *testing.M) {
	LowerDerivationCostForTests(1000)
	os.Exit(m.Run())
}

// An authority opened without the test seam derives at the production cost,
// and that cost is PBKDF2-SHA256 at 600,000 rounds producing the stored form:
// a hash written by one release must verify on the next.
func TestProductionPasswordCost(t *testing.T) {
	if ProductionIterations != 600000 {
		t.Fatal("production derivation cost changed; every stored password would stop verifying")
	}
	atProductionCost(t)
	a, err := Open(t.Context(), filepath.Join(t.TempDir(), "identity.json"), "https://tessera.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.PasswordCost() != 600000 {
		t.Fatal("authority opened at", a.PasswordCost())
	}
	got, err := a.derive(t.Context(), "synthetic-password", "salt")
	if err != nil {
		t.Fatal(err)
	}
	key, err := pbkdf2.Key(sha256.New, "synthetic-password", []byte("salt"), 600000, 32)
	if err != nil || got != hex.EncodeToString(key) {
		t.Fatal("stored password form changed")
	}
}

func TestLoweredCostRefusesNonsense(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("zero iterations accepted")
		}
	}()
	LowerDerivationCostForTests(0)
}

// atProductionCost makes authorities this test opens derive at the production
// cost, for a test whose subject is how long a derivation takes.
func atProductionCost(t *testing.T) {
	t.Helper()
	previous := testIterations.Swap(0)
	t.Cleanup(func() { testIterations.Store(previous) })
}

// Removing an installation leaves nothing of it: no record, no token, no code.
func TestARemovedInstallationLeavesNoRecord(t *testing.T) {
	a, _, dataset := authFixture(t)
	bearer, err := a.AddAutomation(t.Context(), "person", dataset)
	if err != nil {
		t.Fatal(err)
	}
	access, err := a.Authenticate(t.Context(), bearer)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.RemoveOwnInstallation(t.Context(), access, access.Authorization); err != nil {
		t.Fatal(err)
	}
	if _, kept := a.data.Authorizations[access.Authorization]; kept {
		t.Fatal("a removed installation's record stayed")
	}
	for _, token := range a.data.Tokens {
		if token.Authorization == access.Authorization {
			t.Fatal("a removed installation's token stayed")
		}
	}
	if _, err = a.Authenticate(t.Context(), bearer); err == nil {
		t.Fatal("a removed installation's token still authenticates")
	}
}
