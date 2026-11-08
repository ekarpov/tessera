package protocol

import (
	"errors"
	"sort"
)

// Error contains only protocol-owned diagnostic text. Validation errors never
// interpolate input values, identifiers, JSON, SQL, or credentials.
type Error struct {
	OperationIndex *int           `json:"operation_index,omitempty"`
	Code           string         `json:"code"`
	Message        string         `json:"message"`
	Details        map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func invalid(code string) error {
	return &Error{Code: code, Message: "Request does not satisfy the protocol"}
}

// ErrorCode returns a stable error code, without exposing wrapped input errors.
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "internal_error"
}

// ErrorStatus maps protocol codes to HTTP status. Request-size errors override
// query_result_limit_exceeded with 413 at the transport boundary.
// errorStatus is every code this protocol defines and the status it carries.
// It is a table rather than a switch so that the set is enumerable: a
// specification that names a code this does not, or omits one this has, is a
// drift nothing could compare while the answer lived in control flow.
var errorStatus = map[string]int{
	"invalid_request":             400,
	"invalid_field":               400,
	"invalid_field_value":         400,
	"invalid_filter":              400,
	"invalid_identifier":          400,
	"invalid_type":                400,
	"invalid_name":                400,
	"meta_invalid_field":          400,
	"meta_schema_error":           400,
	"authentication_failed":       401,
	"permission_denied":           403,
	"not_supported":               403,
	"not_found":                   404,
	"meta_type_not_found":         404,
	"meta_field_not_found":        404,
	"unavailable":                 404,
	"conflict":                    409,
	"already_exists":              409,
	"meta_type_already_exists":    409,
	"expired":                     410,
	"invalid_cursor":              410,
	"cursor_expired":              410,
	"view_changed":                410,
	"limit_exceeded":              413,
	"unsupported_representation":  415,
	"unsupported_query":           422,
	"query_result_limit_exceeded": 422,
	"rate_limited":                429,
	"internal_error":              500,
	"not_implemented":             501,
	// A dataset that has outgrown what this server will materialise to
	// perform a write. 507 rather than a 4xx because the request was not the
	// caller's mistake — a one-record insert is a legal request — and rather
	// than 503 because nothing about waiting helps. It is the one refusal in
	// this protocol that no retry and no smaller request can clear.
	"dataset_full":        507,
	"dataset_unavailable": 503,
	"store_unavailable":   503,
}

func ErrorStatus(code string) int {
	if status, known := errorStatus[code]; known {
		return status
	}
	// An unrecognised code is this server failing to describe itself, which is
	// what 500 says.
	return 500
}

// ErrorCodes returns every code this protocol defines, sorted. It exists so a
// specification can be compared against the code rather than proofread against
// it.
func ErrorCodes() []string {
	codes := make([]string, 0, len(errorStatus))
	for code := range errorStatus {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}
