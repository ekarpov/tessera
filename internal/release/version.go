// Package release reports what this binary was actually built from.
package release

import (
	"runtime/debug"
	"strings"
)

// Unknown is reported when a binary carries no build information at all, which
// happens when it was assembled by something other than the Go toolchain.
const Unknown = "0.0.0-unknown"

// Version prefers the module version, which a binary installed from a tag
// carries, and otherwise names the commit it was built from. A build from a
// dirty working tree says so.
//
// This is derived rather than declared on purpose. A constant edited by hand
// at release time is a constant that is wrong between releases, and a server
// that cannot say exactly what it is running is one nobody can reproduce a
// report against. Go embeds this automatically; nothing here needs a build
// flag, a code generator or a dependency.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Unknown
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	revision, modified := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return Unknown
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	version := "0.0.0-" + revision
	if modified {
		// An uncommitted change makes the revision a half-truth, and a
		// deployment running one should be visibly not a release.
		version += "-modified"
	}
	return version
}

// Released reports whether this binary came from a tagged module version
// rather than a working tree, so an operational surface does not present a
// development build as a release.
//
// The Go toolchain synthesizes a pseudo-version from the commit when there is
// no tag, and a pseudo-version begins with a version that looks entirely real:
// v0.0.0-20260919132919-d40db46d3bf7 is not a release, and testing only its
// prefix would say that it is.
func Released() bool {
	version := Version()
	return version != Unknown && !strings.Contains(version, "+dirty") && !pseudoVersion(version)
}

// pseudoVersion recognises the toolchain's derived form: a timestamp of
// fourteen digits followed by a twelve-character revision, as the last two
// hyphenated fields.
func pseudoVersion(version string) bool {
	fields := strings.Split(strings.TrimSuffix(version, "+dirty"), "-")
	if len(fields) < 3 {
		return false
	}
	timestamp, revision := fields[len(fields)-2], fields[len(fields)-1]
	// The form that follows an existing tag prefixes the timestamp with "0.",
	// as in v0.1.1-0.20260919132919-d40db46d3bf7.
	timestamp = strings.TrimPrefix(timestamp, "0.")
	if len(timestamp) != 14 || len(revision) != 12 {
		return false
	}
	for _, r := range timestamp {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range revision {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
