package protocol

import (
	"crypto/sha256"
	"encoding/binary"
	"regexp"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func ValidateOperationKey(key string) error {
	if !uuidPattern.MatchString(key) {
		return invalid("invalid_identifier")
	}
	return nil
}

// RequestDigest uses SHA-256 over three uint64-big-endian length-prefixed byte
// strings: method, request target (path and query), and decoded body. This is an
// implementation framing choice, not a wire-visible canonicalization. Callers
// must remove transfer framing and decode any accepted content encoding first.
// Authorization is checked separately on every attempt, including a replay.
func RequestDigest(method, target string, body []byte) [32]byte {
	h := sha256.New()
	var length [8]byte
	for _, part := range [][]byte{[]byte(method), []byte(target), body} {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(part)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
