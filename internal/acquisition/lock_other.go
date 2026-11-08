//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package acquisition

import (
	"errors"
	"os"
)

func lockOwner(string) (*os.File, error) {
	return nil, errors.New("SQLite process locking is not supported on this platform")
}
