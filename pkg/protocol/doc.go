// Package protocol implements the Tessera protocol's value and
// metadata contracts. It is independent of HTTP handlers and storage engines.
//
// DecodeRequest is the strict request boundary: it rejects duplicate members,
// unknown fields, malformed Unicode, trailing JSON, and oversized input. Value
// decoding is forward-compatible with additional response envelope members;
// DecodeRequest rejects those members when they occur in a request instead.
//
// This package and its published vectors are provisional. They do not certify a
// server's conformance or replace the specification in docs/.
package protocol
