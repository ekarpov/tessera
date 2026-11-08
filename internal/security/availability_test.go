package security_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// held is a request body that stops once it has been read from, standing in
// for a client that is sending slowly. It reports when it has been entered so
// a test can be sure the request is genuinely in flight.
type heldBody struct {
	entered, release chan struct{}
	inner            io.Reader
	waited           bool
}

func (h *heldBody) Read(p []byte) (int, error) {
	if !h.waited {
		h.waited = true
		close(h.entered)
		<-h.release
	}
	return h.inner.Read(p)
}

func newHeldBody(content string) *heldBody {
	return &heldBody{entered: make(chan struct{}), release: make(chan struct{}), inner: strings.NewReader(content)}
}

// Work an unauthenticated caller can start must not stop the server serving
// everybody else. Each case here was a way to do exactly that: a deliberate
// 600k-iteration password derivation and a request body read from the
// network, both under the one lock every API request takes to check its
// bearer token.
func TestUnauthenticatedWorkDoesNotStopTheServer(t *testing.T) {
	for _, stall := range []struct {
		name  string
		begin func(t *testing.T, x *tenancy) (entered <-chan struct{}, release func(), done <-chan struct{})
	}{
		{"a registration whose body has not finished arriving", func(t *testing.T, x *tenancy) (<-chan struct{}, func(), <-chan struct{}) {
			body := newHeldBody(`{"redirect_uris":["https://client.example/callback"]}`)
			r := httptest.NewRequest("POST", "/oauth/register", body)
			r.Header.Set("Content-Type", "application/json")
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				x.runtime.Handler.ServeHTTP(httptest.NewRecorder(), r)
			}()
			return body.entered, func() { close(body.release) }, finished
		}},
		{"an administration sign-in being verified", func(t *testing.T, x *tenancy) (<-chan struct{}, func(), <-chan struct{}) {
			// No body to hold, so the derivation itself is the stall: it is
			// started and the token check races it.
			entered, finished := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(finished)
				close(entered)
				x.runtime.Authority.AuthenticateAdministrator("alice", "synthetic-wrong-password")
			}()
			return entered, func() {}, finished
		}},
	} {
		t.Run(stall.name, func(t *testing.T) {
			x := tenants(t)
			entered, release, finished := stall.begin(t, x)
			<-entered
			served := make(chan int, 1)
			go func() {
				served <- x.call(t, "GET", "/v1/datasets", x.aliceTok, "", nil).Code
			}()
			select {
			case code := <-served:
				if code != 200 {
					t.Fatal("an ordinary request failed while the server was busy", code)
				}
			case <-time.After(5 * time.Second):
				release()
				t.Fatal("an ordinary request waited on work an unauthenticated caller started")
			}
			release()
			<-finished
		})
	}
}

// A content transfer is bytes arriving at whatever pace the client chooses.
// Reading them held a process-wide mutex, so one slow upload stopped every
// transfer on the server — other tenants included.
func TestASlowUploadDoesNotStopAnotherTenantsTransfer(t *testing.T) {
	x := tenants(t)
	open := func(token, dataset, storeID, operation string) string {
		t.Helper()
		w := x.call(t, "POST", "/v1/stores/"+storeID+"/imports", token,
			`{"operation_id":"`+operation+`","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":10}`, nil)
		if w.Code != 201 && w.Code != 200 {
			t.Fatal("could not begin an import", w.Code, w.Body.String())
		}
		var opened struct {
			URL string `json:"url"`
		}
		if err := jsonInto(w.Body.Bytes(), &opened); err != nil || opened.URL == "" {
			t.Fatal("no transfer url", w.Body.String())
		}
		return strings.TrimPrefix(opened.URL, "https://tessera.example")
	}
	slow := open(x.aliceTok, x.aliceSet, x.aliceStore, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	quick := open(x.bobTok, x.bobSet, x.bobStore, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	body := newHeldBody("abc")
	stalled := httptest.NewRequest("PATCH", slow, body)
	stalled.Header.Set("Content-Range", "bytes 0-2/3")
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		x.runtime.Handler.ServeHTTP(httptest.NewRecorder(), stalled)
	}()
	<-body.entered

	done := make(chan int, 1)
	go func() {
		other := httptest.NewRequest("PATCH", quick, strings.NewReader("xyz"))
		other.Header.Set("Content-Range", "bytes 0-2/3")
		w := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(w, other)
		done <- w.Code
	}()
	select {
	case code := <-done:
		if code >= 400 {
			t.Fatal("the other tenant's transfer failed", code)
		}
	case <-time.After(5 * time.Second):
		close(body.release)
		t.Fatal("one tenant's slow upload stopped another tenant's transfer")
	}
	close(body.release)
	<-finished
}

// The shared rate ceiling bounds what this server does. It counted arrivals
// it had already refused, so one address knocking past its own limit spent
// the budget for every other address.
func TestOneAddressCannotSpendTheSharedRateBudget(t *testing.T) {
	x := tenants(t)
	knock := func(ip string) int {
		r := httptest.NewRequest("GET", "/admin/authorizations", nil)
		r.RemoteAddr = ip + ":40000"
		r.SetBasicAuth("alice", "synthetic-wrong-password")
		w := httptest.NewRecorder()
		x.runtime.Handler.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 200; i++ {
		knock("198.51.100.7")
	}
	if code := knock("203.0.113.9"); code == 429 {
		t.Fatal("one address knocking past its own limit locked out every other address")
	}
}

func jsonInto(data []byte, into any) error { return json.Unmarshal(data, into) }
