package standalone

import (
	"os"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
)

// productionCost undoes the test cost TestMain sets.
var productionCost func()

// Passwords hash at a test cost here; see identity.LowerDerivationCostForTests.
func TestMain(m *testing.M) {
	productionCost = identity.LowerDerivationCostForTests(1000)
	os.Exit(m.Run())
}

// The composition a deployment runs derives at the production cost: nothing
// it configures or calls may lower it.
func TestProductionPasswordCost(t *testing.T) {
	productionCost()
	defer identity.LowerDerivationCostForTests(1000)
	config := Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite", EncryptedStorage: true}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if cost := runtime.Authority.PasswordCost(); cost != identity.ProductionIterations {
		t.Fatal("standalone server derives passwords at", cost)
	}
}

// A configuration may set a storage quota for new datasets. It was refused
// as an unknown limit, so a deployment asking for one could not start.
func TestAConfiguredStorageQuotaStarts(t *testing.T) {
	config := Config{Issuer: "https://tessera.example", DataDir: t.TempDir(), Engine: "sqlite", EncryptedStorage: true,
		Limits: []byte(`{"max_materialized_bytes": 268435456, "max_stored_bytes": 67108864}`)}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal("a configured storage quota stopped the server:", err)
	}
	runtime.Close()
}
