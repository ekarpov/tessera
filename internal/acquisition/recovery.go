package acquisition

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekarpov/tessera/internal/devfaults"
	p "github.com/ekarpov/tessera/pkg/protocol"
	"github.com/ekarpov/tessera/pkg/store"
)

const recoveryPending = "recovery.pending"
const recoveryComplete = "recovery.complete"

type recoveryIntent struct {
	Version     int             `json:"version"`
	Expected    string          `json:"expected"`
	Export      json.RawMessage `json:"export"`
	Digest      string          `json:"digest"`
	Scope       string          `json:"scope"`
	Revision    string          `json:"revision"`
	EffectiveAt string          `json:"effective_at"`
}

func recoveryRefused() error { return store.Refuse("conflict") }

// privateRegular refuses symlinks and unusual filesystem objects before opening.
// The operator must control the private parent directory; this is not a boundary
// against a hostile local process able to replace entries while we own the lock.
func privateRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return recoveryRefused()
	}
	return nil
}
func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return recoveryRefused()
	}
	return nil
}
func recoveryDigest(expected string, raw []byte) string {
	sum := sha256.Sum256(append([]byte(expected+"\n"), raw...))
	return hex.EncodeToString(sum[:])
}
func readRecoveryIntent(dir string) (*recoveryIntent, *settingsDisk, error) {
	if err := recoveryDirectoryContents(dir); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, "intent.json")
	if err := privateRegular(path); err != nil {
		return nil, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 32769))
	file.Close()
	var intent recoveryIntent
	if err != nil || p.DecodeRequest(raw, &intent, 32768) != nil {
		return nil, nil, recoveryRefused()
	}
	exported, err := DecodeSettingsExport(intent.Export)
	if err != nil || !validSettingsExport(exported) || intent.Version != 1 || intent.Expected != exported.Deployment || !validToken(intent.Expected) {
		return nil, nil, recoveryRefused()
	}
	canonical, _ := json.Marshal(exported)
	if intent.Digest != recoveryDigest(intent.Expected, canonical) {
		return nil, nil, recoveryRefused()
	}
	parts := strings.Split(intent.Scope, ".")
	at, err := time.Parse(time.RFC3339Nano, intent.EffectiveAt)
	if err != nil || at.IsZero() || len(parts) != 2 || parts[0] != intent.Expected || !validToken(parts[1]) || !validRevision(intent.Revision, intent.Scope) {
		return nil, nil, recoveryRefused()
	}
	disk := &settingsDisk{Version: 1, Deployment: intent.Expected, Scope: intent.Scope, Effective: Settings{EveryHours: exported.EveryHours, Keep: exported.Keep, Revision: intent.Revision}, EffectiveAt: at, ClockFloor: at, Requests: map[string]SettingsOperation{}}
	return &intent, disk, nil
}
func recoveryBytes(v any) []byte { raw, _ := json.Marshal(v); return append(raw, '\n') }

// RecoverProducer restores only a producer identity and effective settings into
// an unused private backup destination. It never opens a database or imports
// operational authority. A durable receipt makes lost success responses safe.
func RecoverProducer(ctx context.Context, directory, expected string, exported *SettingsExport) error {
	if !validToken(expected) || !validSettingsExport(exported) || exported.Deployment != expected {
		return store.Refuse("invalid_field")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := privateDirectory(directory); err != nil {
		return recoveryRefused()
	}
	root := filepath.Join(directory, ".acquisitions")
	if err := os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := privateDirectory(root); err != nil {
		return recoveryRefused()
	}
	if err := syncDir(directory); err != nil {
		return err
	}
	lock := filepath.Join(root, "owner.lock")
	if err := privateRegular(lock); err != nil && !os.IsNotExist(err) {
		return recoveryRefused()
	}
	owner, err := lockOwner(lock)
	if err != nil {
		return recoveryRefused()
	}
	defer owner.Close()
	if err := syncDir(root); err != nil {
		return err
	}
	canonical, _ := json.Marshal(exported)
	digest := recoveryDigest(expected, canonical)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	pending, complete := false, false
	for _, entry := range entries {
		switch entry.Name() {
		case "owner.lock", "deployment.json", "settings.json":
			if privateRegular(filepath.Join(root, entry.Name())) != nil {
				return recoveryRefused()
			}
		case recoveryPending:
			pending = true
		case recoveryComplete:
			complete = true
		default:
			return recoveryRefused()
		}
	}
	if pending && complete {
		return recoveryRefused()
	}
	intentDir := filepath.Join(root, recoveryPending)
	var intent *recoveryIntent
	var disk *settingsDisk
	if pending || complete {
		if complete {
			intentDir = filepath.Join(root, recoveryComplete)
		}
		if err := recoveryDirectoryContents(intentDir); err != nil {
			return recoveryRefused()
		}
		intent, disk, err = readRecoveryIntent(intentDir)
		if pending && os.IsNotExist(err) {
			contents, readErr := os.ReadDir(intentDir)
			// A marker durable before its intent has not selected any epoch or
			// installed any targets. The same explicit command can safely begin it.
			if readErr == nil && len(contents) == 0 {
				if _, statErr := os.Lstat(filepath.Join(root, "settings.json")); os.IsNotExist(statErr) {
					pending = false
				}
			}
		}
		if pending || complete {
			if err != nil || intent.Expected != expected || intent.Digest != digest {
				return recoveryRefused()
			}
		}
	}
	if !pending && !complete {
		if _, err := os.Lstat(filepath.Join(root, "settings.json")); !os.IsNotExist(err) {
			return recoveryRefused()
		}
		if raw, err := os.ReadFile(filepath.Join(root, "deployment.json")); err == nil {
			if !bytes.Equal(raw, recoveryBytes(expected)) {
				return recoveryRefused()
			}
		} else if !os.IsNotExist(err) {
			return recoveryRefused()
		}
		if err := os.Mkdir(intentDir, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		if err := syncDir(root); err != nil {
			return err
		}
		if err := devfaults.BackupRecovery("marker"); err != nil {
			return err
		}
		scope := expected + "." + token()
		intent = &recoveryIntent{Version: 1, Expected: expected, Export: canonical, Digest: digest, Scope: scope, Revision: scope + "." + token(), EffectiveAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if err := atomicJSON(intentDir, "intent.json", intent); err != nil {
			return err
		}
		intent, disk, err = readRecoveryIntent(intentDir)
		if err != nil {
			return recoveryRefused()
		}
	}
	if err := devfaults.BackupRecovery("intent"); err != nil {
		return err
	}
	for _, item := range []struct {
		name  string
		value any
	}{{"deployment.json", expected}, {"settings.json", disk}} {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(root, item.name)
		want := recoveryBytes(item.value)
		if err := privateRegular(target); err == nil {
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, want) {
				return recoveryRefused()
			}
		} else if !os.IsNotExist(err) || complete {
			return recoveryRefused()
		} else {
			// Stage under the owned pending marker. Link is exclusive: it cannot
			// replace a target, and complete content is durable before publication.
			if err := atomicJSON(intentDir, item.name, item.value); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(intentDir, item.name), target); err != nil {
				return recoveryRefused()
			}
		}
		if err := syncDir(root); err != nil {
			return err
		}
		if err := devfaults.BackupRecovery(item.name); err != nil {
			return err
		}
	}
	if !complete {
		if err := os.Rename(intentDir, filepath.Join(root, recoveryComplete)); err != nil {
			return err
		}
	}
	if err := syncDir(root); err != nil {
		return err
	}
	return devfaults.BackupRecovery("complete")
}

// Called under the producer owner lock before minting any identity or settings.
func checkRecoveryState(root string) error {
	if _, err := os.Lstat(filepath.Join(root, recoveryPending)); !os.IsNotExist(err) {
		return recoveryRefused()
	}
	receipt := filepath.Join(root, recoveryComplete)
	if _, err := os.Lstat(receipt); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return recoveryRefused()
	}
	intent, _, err := readRecoveryIntent(receipt)
	if err != nil {
		return recoveryRefused()
	}
	path := filepath.Join(root, "deployment.json")
	if privateRegular(path) != nil {
		return recoveryRefused()
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, recoveryBytes(intent.Expected)) {
		return recoveryRefused()
	}
	// Current settings may have legitimately changed after this receipt.
	if privateRegular(filepath.Join(root, "settings.json")) != nil {
		return recoveryRefused()
	}
	return nil
}

func recoveryDirectoryContents(dir string) error {
	if err := privateDirectory(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "intent.json", "deployment.json", "settings.json":
		default:
			if !strings.HasPrefix(entry.Name(), ".journal-") {
				return recoveryRefused()
			}
		}
		if privateRegular(filepath.Join(dir, entry.Name())) != nil {
			return recoveryRefused()
		}
	}
	return nil
}
