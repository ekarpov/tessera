package identity

import (
	"context"
	"github.com/ekarpov/tessera/pkg/store"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func browserConsent(t *testing.T) (*Authority, url.Values, *http.Cookie, string) {
	t.Helper()
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Name: "Synthetic Notes", Redirects: []string{"com.example.synthetic:/oauth/callback"}}
	q := pageQuery(dataset)
	q.Set("redirect_uri", "com.example.synthetic:/oauth/callback")
	q.Set("shared_reads", "true")
	w := signInRequest(t, a, q, map[string]string{"Accept": "text/html"})
	var id string
	for id = range a.flows {
	}
	w = formCall(a, "/oauth/authorize", url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "decision": {"continue"}, "username": {"person"}, "password": {"synthetic-password-only"}}, w.Result().Cookies()[0])
	if w.Code != 200 {
		t.Fatal("sign-in failed", w.Code)
	}
	return a, url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "decision": {"allow"}, "read": {"yes"}}, w.Result().Cookies()[0], q.Encode()
}

func browserPost(a *Authority, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "text/html")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func pageLink(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	match := regexp.MustCompile(`href="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("missing link", w.Code, w.Body.String())
	}
	return html.UnescapeString(match[1])
}

func assertRestart(t *testing.T, a *Authority, w *httptest.ResponseRecorder, query string) {
	t.Helper()
	if w.Code != 400 || !strings.Contains(w.Body.String(), `data-page="restart"`) || w.Header().Get("Location") != "" || w.Header().Get("Content-Security-Policy") != pageSecurity {
		t.Fatal("no safe restart", w.Code, w.Body.String())
	}
	target, err := url.Parse(pageLink(t, w))
	if err != nil {
		t.Fatal(err)
	}
	if target.IsAbs() || target.Host != "" || target.Path != "/oauth/authorize" || target.RawQuery != query {
		t.Fatal("restart changed validated request", target)
	}
}

func TestConcurrentConsentRecovery(t *testing.T) {
	a, form, cookie, query := browserConsent(t)
	var results [2]*httptest.ResponseRecorder
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Go(func() { <-start; results[i] = browserPost(a, form, cookie) })
	}
	close(start)
	wg.Wait()
	var complete, restart *httptest.ResponseRecorder
	for _, w := range results {
		if w.Code == 200 {
			complete = w
		} else {
			restart = w
		}
	}
	if complete == nil || restart == nil {
		t.Fatal("concurrent submissions did not complete exactly once")
	}
	assertRestart(t, a, restart, query)
	if len(a.data.Authorizations) != 1 || len(a.data.Codes) != 1 || len(a.flows) != 0 {
		t.Fatal("duplicate consent created grants or codes")
	}
	// A further retry still only offers a fresh attempt, before redemption.
	assertRestart(t, a, browserPost(a, form, cookie), query)
	target, _ := url.Parse(pageLink(t, complete))
	exchange := url.Values{"grant_type": {"authorization_code"}, "client_id": {"client"}, "redirect_uri": {"com.example.synthetic:/oauth/callback"}, "code": {target.Query().Get("code")}, "code_verifier": {strings.Repeat("x", 43)}}
	if w := formCall(a, "/oauth/token", exchange, nil); w.Code != 200 {
		t.Fatal("original code did not redeem", w.Code)
	}
	assertRestart(t, a, browserPost(a, form, cookie), query)
	if w := formCall(a, "/oauth/token", exchange, nil); w.Code != 400 {
		t.Fatal("consumed code resurrected")
	}
	// Recovery GET creates only a fresh sign-in; no authentication or consent carries.
	r := httptest.NewRequest("GET", pageLink(t, restart), nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-page="signin"`) || len(a.data.Authorizations) != 1 || len(a.data.Codes) != 0 || len(a.flows) != 1 {
		t.Fatal("restart skipped sign-in or duplicated grants")
	}
	for _, f := range a.flows {
		if f.Principal != "" || f.CSRF == form.Get("csrf") {
			t.Fatal("restart reused authentication or CSRF")
		}
	}
}

func TestRecoveryRequiresProofAndRevalidatesRegistration(t *testing.T) {
	a, form, cookie, query := browserConsent(t)
	browserPost(a, form, cookie)
	bad := form.Clone()
	bad.Set("csrf", "wrong")
	bad.Set("redirect_uri", "https://attacker.example/")
	bad.Set("return_to", "//attacker.example/")
	for _, w := range []*httptest.ResponseRecorder{browserPost(a, bad, cookie), browserPost(a, form, nil)} {
		if w.Code != 400 || strings.Contains(w.Body.String(), "href=") {
			t.Fatal("unproved recovery exposed context")
		}
	}
	form.Set("redirect_uri", "https://attacker.example/")
	form.Set("return_to", "//attacker.example/")
	recovery := browserPost(a, form, cookie)
	assertRestart(t, a, recovery, query)
	client := a.data.Clients["client"]
	client.Redirects = []string{"org.example.changed:/callback"}
	a.data.Clients["client"] = client
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", pageLink(t, recovery), nil))
	if w.Code != 400 || w.Header().Get("Location") != "" || len(a.flows) != 0 || len(a.data.Authorizations) != 1 {
		t.Fatal("restart bypassed current registration")
	}
}

func TestExpiredConsentRecovery(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "completed"}[completed], func(t *testing.T) {
			a, form, cookie, query := browserConsent(t)
			if completed {
				browserPost(a, form, cookie)
			}
			id := form.Get("flow")
			f := a.recoveries[id]
			f.Expires = time.Now().Add(-time.Minute)
			a.recoveries[id] = f
			if !completed {
				a.flows[id] = f
			}
			assertRestart(t, a, browserPost(a, form, cookie), query)
			before := len(a.data.Authorizations)
			if !completed && before != 0 {
				t.Fatal("expired flow granted access")
			}
			f.Expires = time.Now().Add(-authorizationRecoveryWindow - time.Minute)
			a.recoveries[id] = f
			w := browserPost(a, form, cookie)
			if w.Code != 400 || strings.Contains(w.Body.String(), "href=") || len(a.data.Authorizations) != before {
				t.Fatal("recovery retention not enforced")
			}
		})
	}
}

func TestRecoveryStorageBound(t *testing.T) {
	a, _, _ := authFixture(t)
	for i := 0; i < maxAuthorizationRecoveries+1; i++ {
		a.rememberAuthorization(string(rune(i+1)), flow{Client: "client", Principal: "person", Expires: time.Now().Add(time.Minute)})
	}
	if len(a.recoveries) != maxAuthorizationRecoveries {
		t.Fatal("recovery cache unbounded")
	}
	if _, exists := a.recoveries[string(rune(1))]; exists {
		t.Fatal("oldest recovery not evicted")
	}
	for id, f := range a.recoveries {
		if f.Principal != "" {
			t.Fatal("recovery retained authentication")
		}
		f.Expires = time.Now().Add(-authorizationRecoveryWindow - time.Minute)
		a.recoveries[id] = f
	}
	a.rememberAuthorization("fresh", flow{Client: "client", Expires: time.Now().Add(time.Minute)})
	if len(a.recoveries) != 1 {
		t.Fatal("expired recovery not swept")
	}
}

func TestConcurrentConflictingConsent(t *testing.T) {
	a, allow, cookie, query := browserConsent(t)
	deny := allow.Clone()
	deny.Set("decision", "deny")
	var results [2]*httptest.ResponseRecorder
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, form := range []url.Values{allow, deny} {
		wg.Go(func() { <-start; results[i] = browserPost(a, form, cookie) })
	}
	close(start)
	wg.Wait()
	var completion *httptest.ResponseRecorder
	for _, w := range results {
		if w.Code == 200 {
			if completion != nil {
				t.Fatal("both decisions completed")
			}
			completion = w
		} else {
			assertRestart(t, a, w, query)
		}
	}
	if completion == nil {
		t.Fatal("neither decision completed")
	}
	target, _ := url.Parse(pageLink(t, completion))
	expected := 0
	if target.Query().Get("code") != "" {
		expected = 1
	} else if target.Query().Get("error") != "access_denied" {
		t.Fatal("invalid completion")
	}
	if len(a.data.Codes) != expected || len(a.data.Authorizations) != expected {
		t.Fatal("conflicting decision duplicated or changed grants")
	}
}

// A cancelled response can race a second submission after grant setup has
// started. An engine may have committed a side effect before returning an error.
// The same flow must never enter that setup a second time, even on failure.
type failedConsentEngine struct {
	Engine
	calls int
}

func (e *failedConsentEngine) SetAuthorization(ctx context.Context, dataset string, access store.Access, auth store.Authorization) error {
	e.calls++
	if err := e.Engine.SetAuthorization(ctx, dataset, access, auth); err != nil {
		return err
	}
	return context.Canceled
}

func TestFailedConsentCannotRepeatGrantSetup(t *testing.T) {
	for _, sharedOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "dataset", true: "shared_only"}[sharedOnly], func(t *testing.T) {
			a, form, cookie, query := browserConsent(t)
			if sharedOnly {
				id := form.Get("flow")
				f := a.flows[id]
				f.Dataset = ""
				a.flows[id] = f
				a.rememberAuthorization(id, f)
				original, _ := url.ParseQuery(query)
				original.Del("dataset")
				query = original.Encode()
				principal := a.data.Principals["person"]
				for dataset := range principal.Datasets {
					principal.Datasets[dataset] = store.Grants{Read: true}
				}
				a.data.Principals["person"] = principal
				form.Set("shared_reads", "yes")
			}
			engine := &failedConsentEngine{Engine: a.engine}
			a.engine = engine
			first := browserPost(a, form, cookie)
			if first.Code != 503 || engine.calls != 1 {
				t.Fatal("failed setup not exercised", first.Code, engine.calls)
			}
			authorizations := len(a.data.Authorizations)
			assertRestart(t, a, browserPost(a, form, cookie), query)
			if engine.calls != 1 || len(a.data.Codes) != 0 || len(a.flows) != 0 || len(a.data.Authorizations) != authorizations {
				t.Fatal("failed consent repeated grant side effects")
			}
		})
	}
}
