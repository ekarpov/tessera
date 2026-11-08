package main

import (
	"os"
	"testing"

	"github.com/ekarpov/tessera/internal/identity"
)

// Passwords hash at a test cost here; see identity.LowerDerivationCostForTests.
// The production cost is asserted by TestProductionPasswordCost in
// internal/standalone and internal/identity.
func TestMain(m *testing.M) {
	identity.LowerDerivationCostForTests(1000)
	os.Exit(m.Run())
}
