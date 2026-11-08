package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/ekarpov/tessera/pkg/store"
)

func segments(r *http.Request) ([]string, error) {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	if len(parts) < 4 {
		return nil, store.Refuse("not_found")
	}
	result := make([]string, 0, len(parts)-3)
	for _, part := range parts[3:] {
		s, err := url.PathUnescape(part)
		if err != nil {
			return nil, store.Refuse("invalid_identifier")
		}
		result = append(result, s)
	}
	return result, nil
}
func urlQuery(r *http.Request) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, store.Refuse("invalid_field")
	}
	for key := range values {
		if key != "fields" && key != "confirm" {
			return nil, store.Refuse("invalid_field")
		}
	}
	return values, nil
}
