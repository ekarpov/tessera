//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package sqlengine

import (
	"os"
	"syscall"
)

func lockDatabase(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func lockContended(err error) bool { return err == syscall.EWOULDBLOCK || err == syscall.EAGAIN }
