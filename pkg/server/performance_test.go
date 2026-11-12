package server_test

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Compare the same stored value size through the HTTP handler and SQLite.
func BenchmarkCreatorHTTPInsert(b *testing.B) {
	for _, variant := range []struct {
		name, option, body string
	}{
		{"explicit", "", `{"fields":{"created_by":{"type":"text","value":"00112233445566778899aabbccddeeff"}}}`},
		{"attributed", `,"creator":true`, `{"fields":{}}`},
	} {
		b.Run(variant.name, func(b *testing.B) {
			_, h, base := sqliteFixture(b)
			wireCall(b, h, "POST", base+"/meta", "owner",
				`{"type":{"id":"bench.attributed","fields":[{"id":"created_by","meta_id":"bench.attributed","type":"text"`+variant.option+`}]}}`, 201)
			path := base + "/objects/bench.attributed"
			b.ReportAllocs()
			for b.Loop() {
				req := httptest.NewRequest("POST", path, strings.NewReader(variant.body))
				req.Header.Set("Authorization", "Bearer owner")
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != 201 {
					b.Fatal("benchmark write refused", w.Code)
				}
			}
		})
	}
}
