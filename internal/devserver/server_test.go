package devserver

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevelopmentRestartKeepsCredentialAndDataset(t *testing.T) {
	dir := t.TempDir()
	runtime, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(runtime.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	request := func(runtime *Runtime, credential string) (int, string) {
		r := httptest.NewRequest("GET", "/v1/datasets", nil)
		r.Header.Set("Authorization", "Bearer "+credential)
		w := httptest.NewRecorder()
		runtime.Handler.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	status, first := request(runtime, strings.TrimSpace(string(token)))
	if status != 200 {
		t.Fatal(status)
	}
	if status, _ = request(runtime, "wrong"); status != 401 {
		t.Fatal("invalid token accepted")
	}
	runtime.Close()
	runtime, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	again, _ := os.ReadFile(filepath.Join(dir, "token"))
	status, second := request(runtime, strings.TrimSpace(string(again)))
	if string(again) != string(token) || status != 200 || first != second {
		t.Fatal("restart recreated identity, schema or credential")
	}
	info, _ := os.Stat(filepath.Join(dir, "token"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential file permissions")
	}
}
