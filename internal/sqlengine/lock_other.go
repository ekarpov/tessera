//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package sqlengine

import (
	"errors"
	"os"
)

func lockDatabase(string) (*os.File, error) {
	return nil, errors.New("SQLite process locking is not supported on this platform")
}

func lockContended(error) bool { return false }
