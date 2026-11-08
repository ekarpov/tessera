//go:build devfaults

package devfaults

import (
	"errors"
	"os"
)

// BackupRecovery injects an offline recovery interruption only in explicit
// devfaults builds. Production binaries contain no environment-triggered path.
func BackupRecovery(stage string) error {
	if os.Getenv("TESSERA_DEV_RECOVERY_INTERRUPT") == stage {
		return errors.New("synthetic backup recovery interruption")
	}
	return nil
}
