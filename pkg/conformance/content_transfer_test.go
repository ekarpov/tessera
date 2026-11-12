package conformance

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestContentCallKeepsControlCredentialOffTransferURL(t *testing.T) {
	auth := make(chan string, 2)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	call := newContentCall(t.Context(), endpoint.Client(), endpoint.URL, "fixture-token")
	if _, err := call("GET", "/v1/stores", "", nil, 204); err != nil {
		t.Fatal(err)
	}
	if _, err := call("PATCH", endpoint.URL+"/signed", "bytes 0-0/1", []byte("x"), 204); err != nil {
		t.Fatal(err)
	}
	if <-auth != "Bearer fixture-token" || <-auth != "" {
		t.Fatal("credential did not stay on the control request")
	}
}
