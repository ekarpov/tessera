package protocol

import (
	"errors"
	"time"
)

// ErrNoCommonProtocolVersion means negotiation cannot establish a connection.
var ErrNoCommonProtocolVersion = errors.New("no common protocol version")

// DeprecatedVersion announces a planned retirement, not permission to stop serving.
// Sunset is a UTC timestamp with seconds, for example 2030-01-01T00:00:00Z.
type DeprecatedVersion struct {
	Version string `json:"version"`
	Sunset  string `json:"sunset"`
}

// ValidateProtocolVersions checks a nonempty set of canonical positive decimal majors.
func ValidateProtocolVersions(versions []string) error {
	seen := map[string]bool{}
	if len(versions) == 0 {
		return errors.New("protocol_versions must be nonempty")
	}
	for _, v := range versions {
		if v == "" || v[0] < '1' || v[0] > '9' || seen[v] {
			return errors.New("invalid or duplicate protocol version")
		}
		for _, c := range v {
			if c < '0' || c > '9' {
				return errors.New("invalid protocol version")
			}
		}
		seen[v] = true
	}
	return nil
}

// SelectProtocolVersion returns the numerically greatest common major, independent
// of array order and machine integer width. No common major is a connection refusal.
func SelectProtocolVersion(advertised, supported []string) (string, error) {
	if err := ValidateProtocolVersions(advertised); err != nil {
		return "", err
	}
	if len(supported) == 0 {
		return "", ErrNoCommonProtocolVersion
	}
	if err := ValidateProtocolVersions(supported); err != nil {
		return "", err
	}
	known := map[string]bool{}
	for _, v := range supported {
		known[v] = true
	}
	best := ""
	for _, v := range advertised {
		if known[v] && (len(v) > len(best) || len(v) == len(best) && v > best) {
			best = v
		}
	}
	if best == "" {
		return "", ErrNoCommonProtocolVersion
	}
	return best, nil
}

// ValidateDeprecatedVersions checks advertised membership, uniqueness and timestamp
// syntax. Support periods and advance notice require independent operator evidence;
// neither this validator nor reaching Sunset authorizes retiring a version.
func ValidateDeprecatedVersions(versions []string, deprecated []DeprecatedVersion) error {
	if err := ValidateProtocolVersions(versions); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, v := range versions {
		known[v] = true
	}
	seen := map[string]bool{}
	const format = "2006-01-02T15:04:05Z"
	for _, d := range deprecated {
		if !known[d.Version] || seen[d.Version] {
			return errors.New("deprecated_versions must name distinct advertised versions")
		}
		date, err := time.Parse(format, d.Sunset)
		if err != nil || date.Format(format) != d.Sunset {
			return errors.New("deprecated_versions sunset must be UTC with seconds")
		}
		seen[d.Version] = true
	}
	return nil
}
