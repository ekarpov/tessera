package security_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Bytes are served from the same origin as the sign-in, account and
// administration pages. An uploaded HTML file rendered there would run its
// script beside them, so a download is always an attachment, never sniffed,
// and sandboxed — whatever content type its uploader declared.
func TestADownloadCannotRunAsAPageOnThisOrigin(t *testing.T) {
	x := tenants(t)
	payload := []byte("<script>fetch('/admin/principals')</script>")
	begin := x.call(t, "POST", "/v1/stores/"+x.aliceStore+"/imports", x.aliceTok,
		`{"operation_id":"html-upload","content_type":"text/html","byte_count":`+itoa(len(payload))+`,"maximum_bytes":1024}`, nil)
	var opened struct {
		UploadID string `json:"upload_id"`
		URL      string `json:"url"`
	}
	if err := json.Unmarshal(begin.Body.Bytes(), &opened); err != nil || opened.URL == "" {
		t.Fatal("could not begin an upload:", begin.Code, begin.Body.String())
	}
	send := httptest.NewRequest("PATCH", strings.TrimPrefix(opened.URL, "https://tessera.example"), bytes.NewReader(payload))
	send.Header.Set("Content-Range", "bytes 0-"+itoa(len(payload)-1)+"/"+itoa(len(payload)))
	sent := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(sent, send)
	if sent.Code >= 400 {
		t.Fatal(sent.Code, sent.Body.String())
	}
	commit := x.call(t, "POST", "/v1/stores/"+x.aliceStore+"/imports/"+opened.UploadID+":commit", x.aliceTok, "", nil)
	var committed struct {
		ItemID string `json:"item_id"`
	}
	if err := json.Unmarshal(commit.Body.Bytes(), &committed); err != nil || committed.ItemID == "" {
		t.Fatal(commit.Code, commit.Body.String())
	}
	resolved := x.call(t, "POST", "/v1/stores/"+x.aliceStore+"/items/"+committed.ItemID+":resolve", x.aliceTok, `{"representation":"original"}`, nil)
	var read struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(resolved.Body.Bytes(), &read); err != nil || read.URL == "" {
		t.Fatal(resolved.Code, resolved.Body.String())
	}
	w := httptest.NewRecorder()
	x.runtime.Handler.ServeHTTP(w, httptest.NewRequest("GET", strings.TrimPrefix(read.URL, "https://tessera.example"), nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), payload) {
		t.Fatal("the download did not return the bytes:", w.Code)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Fatal("a download was not an attachment:", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatal("a download could be sniffed:", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") || !strings.Contains(got, "default-src 'none'") {
		t.Fatal("a download was not sandboxed:", got)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
