//go:build devfaults

package acquisition

import (
	"bytes"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestProducerRecoveryInterruptedEpochIsStable(t *testing.T) {
	for _, stage := range []string{"intent", "deployment.json", "settings.json", "complete"} {
		t.Run(stage, func(t *testing.T) {
			dir, e := recoveryFixture(t)
			t.Setenv("TESSERA_DEV_RECOVERY_INTERRUPT", stage)
			if err := RecoverProducer(t.Context(), dir, e.Deployment, e); err == nil {
				t.Fatal("injection did not interrupt")
			}
			root := filepath.Join(dir, ".acquisitions")
			marker := recoveryPending
			if stage == "complete" {
				marker = recoveryComplete
			}
			before, _, err := readRecoveryIntent(filepath.Join(root, marker))
			if err != nil {
				t.Fatal(err)
			}
			if stage != "complete" {
				var calls atomic.Int32
				if m, err := OpenNative(dir, bytes.Repeat([]byte{4}, 32), 24, 1, nil, testCapture(&calls)); err == nil {
					m.Close()
					t.Fatal("startup ignored pending transaction")
				}
			}
			t.Setenv("TESSERA_DEV_RECOVERY_INTERRUPT", "")
			if err := RecoverProducer(t.Context(), dir, e.Deployment, e); err != nil {
				t.Fatal(err)
			}
			after, _, err := readRecoveryIntent(filepath.Join(root, recoveryComplete))
			if err != nil || after.Revision != before.Revision || after.Scope != before.Scope || after.EffectiveAt != before.EffectiveAt {
				t.Fatal("retry selected another epoch/result")
			}
		})
	}
}
