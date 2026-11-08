package identity

import (
	"sync/atomic"
	"testing"
)

// ProductionIterations is the PBKDF2-SHA256 cost of every stored password.
// The stored hash does not record it, so it is a property of the deployment's
// registry: changing it would make every existing password fail to verify.
const ProductionIterations = 600000

// testIterations is the cost an authority opened in a test binary uses when a
// test lowered it. Zero means production.
var testIterations atomic.Int64

// LowerDerivationCostForTests makes every authority opened afterwards in this
// test binary derive passwords at iterations rounds, and returns what restores
// the production cost.
//
// The security suite checks passwords hundreds of times, and at the production
// cost under the race detector each check takes about a second. A test proves
// the same thing at a tenth of the cost: the rules about passwords do not
// depend on how many rounds hash them.
//
// It panics in any program that is not a test binary, so no flag,
// configuration or composition can reach it. A registry written at a lowered
// cost is useless outside the test that wrote it, and that is intended.
func LowerDerivationCostForTests(iterations int) (restore func()) {
	if !testing.Testing() {
		panic("identity: the derivation cost may only be lowered in a test binary")
	}
	if iterations < 1 {
		panic("identity: a derivation needs at least one iteration")
	}
	previous := testIterations.Swap(int64(iterations))
	return func() { testIterations.Store(previous) }
}

// derivationCost is the cost an authority opened now derives at.
func derivationCost() int {
	if lowered := testIterations.Load(); lowered != 0 && testing.Testing() {
		return int(lowered)
	}
	return ProductionIterations
}

// PasswordCost reports the derivation cost this authority was opened with.
func (a *Authority) PasswordCost() int { return a.iterations }
