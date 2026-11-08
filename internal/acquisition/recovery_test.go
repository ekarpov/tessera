package acquisition

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T) (string, *SettingsExport) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	deployment := token()
	return dir, &SettingsExport{Version: 1, Deployment: deployment, EveryHours: 0, Keep: 7, Revision: deployment + "." + token() + "." + token(), ExportedAt: time.Now().UTC()}
}
func TestProducerRecoveryPermittedInitialStatesAndLiveUpdates(t *testing.T) {
	for _, state := range []string{"fresh", "lock-only", "identity-only"} {
		t.Run(state, func(t *testing.T) {
			dir, exported := recoveryFixture(t)
			root := filepath.Join(dir, ".acquisitions")
			if state != "fresh" {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if state == "lock-only" {
				f, err := lockOwner(filepath.Join(root, "owner.lock"))
				if err != nil {
					t.Fatal(err)
				}
				f.Close()
			}
			if state == "identity-only" {
				if err := atomicJSON(root, "deployment.json", exported.Deployment); err != nil {
					t.Fatal(err)
				}
			}
			if err := RecoverProducer(t.Context(), dir, exported.Deployment, exported); err != nil {
				t.Fatal(err)
			}
			intent, _, err := readRecoveryIntent(filepath.Join(root, recoveryComplete))
			if err != nil {
				t.Fatal(err)
			}
			if err := RecoverProducer(t.Context(), dir, exported.Deployment, exported); err != nil {
				t.Fatal("lost response replay", err)
			}
			again, _, err := readRecoveryIntent(filepath.Join(root, recoveryComplete))
			if err != nil || intent.Revision != again.Revision {
				t.Fatal("replay changed chosen epoch")
			}
			changed := *exported
			changed.ExportedAt = changed.ExportedAt.Add(time.Second)
			if err := RecoverProducer(t.Context(), dir, exported.Deployment, &changed); err == nil {
				t.Fatal("different full export replay accepted")
			}
			var calls atomic.Int32
			m, err := OpenNative(dir, bytes.Repeat([]byte{4}, 32), 24, 1, nil, testCapture(&calls))
			if err != nil {
				t.Fatal(err)
			}
			observed := m.SchedulerObservation()
			if observed.Settings.Keep != 7 || observed.Settings.EveryHours != 0 || observed.Settings.Revision == exported.Revision || observed.PendingRequestID != nil {
				t.Fatal("recovery outcome")
			}
			changeSettings(t, m, 0, 9)
			after := m.SchedulerObservation()
			m.Close()
			m, err = OpenNative(dir, bytes.Repeat([]byte{4}, 32), 24, 1, nil, testCapture(&calls))
			if err != nil {
				t.Fatal("receipt blocked legitimate later live update", err)
			}
			if *m.SchedulerObservation().Settings != *after.Settings {
				t.Fatal("live update changed after restart")
			}
			m.Close()
			if err := RecoverProducer(t.Context(), dir, exported.Deployment, exported); err == nil {
				t.Fatal("receipt replay replaced initialized live state")
			}
		})
	}
}

func TestProducerRecoveryRefusesIncompatibleStates(t *testing.T) {
	cases := map[string]func(string, *SettingsExport){
		"conflicting identity": func(root string, e *SettingsExport) { _ = atomicJSON(root, "deployment.json", token()) },
		"settings journal": func(root string, e *SettingsExport) {
			_ = os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{}`), 0600)
		},
		"corrupt identity": func(root string, e *SettingsExport) {
			_ = os.WriteFile(filepath.Join(root, "deployment.json"), []byte(`broken`), 0600)
		},
		"operations": func(root string, e *SettingsExport) {
			_ = os.WriteFile(filepath.Join(root, "operations.json"), []byte(`{}`), 0600)
		},
		"collectors": func(root string, e *SettingsExport) {
			_ = os.WriteFile(filepath.Join(root, "collectors.json"), []byte(`{}`), 0600)
		},
		"artifacts": func(root string, e *SettingsExport) { _ = os.Mkdir(filepath.Join(root, "set-"+token()), 0700) },
		"unknown":   func(root string, e *SettingsExport) { _ = os.WriteFile(filepath.Join(root, "unknown"), nil, 0600) },
		"identity symlink": func(root string, e *SettingsExport) {
			_ = os.Symlink("outside", filepath.Join(root, "deployment.json"))
		},
		"settings directory": func(root string, e *SettingsExport) { _ = os.Mkdir(filepath.Join(root, "settings.json"), 0700) },
		"lock symlink":       func(root string, e *SettingsExport) { _ = os.Symlink("outside", filepath.Join(root, "owner.lock")) },
		"pending symlink":    func(root string, e *SettingsExport) { _ = os.Symlink("outside", filepath.Join(root, recoveryPending)) },
		"receipt symlink":    func(root string, e *SettingsExport) { _ = os.Symlink("outside", filepath.Join(root, recoveryComplete)) },
		"missing nonempty intent": func(root string, e *SettingsExport) {
			d := filepath.Join(root, recoveryPending)
			_ = os.Mkdir(d, 0700)
			_ = os.WriteFile(filepath.Join(d, ".journal-orphan"), []byte("truncated"), 0600)
		},
		"corrupt intent": func(root string, e *SettingsExport) {
			d := filepath.Join(root, recoveryPending)
			_ = os.Mkdir(d, 0700)
			_ = os.WriteFile(filepath.Join(d, "intent.json"), []byte("truncated"), 0600)
		},
		"missing receipt intent": func(root string, e *SettingsExport) { _ = os.Mkdir(filepath.Join(root, recoveryComplete), 0700) },
		"world-readable identity": func(root string, e *SettingsExport) {
			path := filepath.Join(root, "deployment.json")
			_ = os.WriteFile(path, recoveryBytes(e.Deployment), 0600)
			_ = os.Chmod(path, 0644)
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			dir, e := recoveryFixture(t)
			root := filepath.Join(dir, ".acquisitions")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			prepare(root, e)
			if err := RecoverProducer(t.Context(), dir, e.Deployment, e); err == nil {
				t.Fatal("incompatible destination accepted")
			}
			if _, err := os.Lstat(filepath.Join(root, recoveryComplete)); !os.IsNotExist(err) && name != "receipt symlink" && name != "missing receipt intent" {
				t.Fatal("refusal published receipt")
			}
		})
	}
}

func TestProducerRecoveryValidatesBeforeInitializing(t *testing.T) {
	for _, change := range []func(*SettingsExport){func(e *SettingsExport) { e.Version = 2 }, func(e *SettingsExport) { e.Deployment = "invalid" }, func(e *SettingsExport) { e.EveryHours = -1 }, func(e *SettingsExport) { e.Keep = 1025 }, func(e *SettingsExport) { e.Revision = "invalid" }, func(e *SettingsExport) { e.ExportedAt = time.Time{} }} {
		dir, e := recoveryFixture(t)
		expected := e.Deployment
		change(e)
		if err := RecoverProducer(t.Context(), dir, expected, e); err == nil {
			t.Fatal("invalid export accepted")
		}
		if _, err := os.Stat(filepath.Join(dir, ".acquisitions")); !os.IsNotExist(err) {
			t.Fatal("invalid export initialized destination")
		}
	}
	dir, e := recoveryFixture(t)
	if err := RecoverProducer(t.Context(), dir, token(), e); err == nil {
		t.Fatal("unsigned export overruled independent expected identity")
	}
}

func TestProducerRecoveryOwnerExclusionAndCorruptionGuard(t *testing.T) {
	dir, e := recoveryFixture(t)
	root := filepath.Join(dir, ".acquisitions")
	_ = os.Mkdir(root, 0700)
	owner, err := lockOwner(filepath.Join(root, "owner.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RecoverProducer(t.Context(), dir, e.Deployment, e); err == nil {
		t.Fatal("recovery ignored live owner")
	}
	owner.Close()
	if err := RecoverProducer(t.Context(), dir, e.Deployment, e); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	m, err := OpenNative(dir, bytes.Repeat([]byte{4}, 32), 24, 1, nil, testCapture(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if err := RecoverProducer(t.Context(), dir, e.Deployment, e); err == nil {
		t.Fatal("recovery ignored running producer")
	}
	m.Close()
	// Negative fixtures deliberately corrupt private state; these are not an
	// operator recovery procedure or part of the successful CLI journey.
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenNative(dir, bytes.Repeat([]byte{4}, 32), 24, 1, nil, testCapture(&calls)); err == nil {
		opened.Close()
		t.Fatal("receipt bypassed settings corruption validation")
	}
	intentPath := filepath.Join(root, recoveryComplete, "intent.json")
	var intent map[string]any
	raw, _ := os.ReadFile(intentPath)
	_ = json.Unmarshal(raw, &intent)
	intent["digest"] = strings.Repeat("0", 64)
	raw, _ = json.Marshal(intent)
	_ = os.WriteFile(intentPath, raw, 0600)
	if err := checkRecoveryState(root); err == nil {
		t.Fatal("corrupt completed receipt accepted")
	}
}
