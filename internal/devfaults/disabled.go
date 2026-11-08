//go:build !devfaults

// Package devfaults injects synthetic transport faults. This is the ordinary
// build, in which it injects nothing and carries no injection code at all: the
// faulting implementation is behind the devfaults build tag, so a release
// binary provably cannot reach it.
package devfaults

import "net/http"

const Header = "X-Tessera-Dev-Fault"

var Available = []string{}

func Enabled() bool { return false }

// Wrap returns the handler unchanged. The enabled argument is accepted so that
// a composition reads the same in both builds.
func Wrap(next http.Handler, _ bool) http.Handler { return next }
