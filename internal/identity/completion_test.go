package identity

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// These are handler tests, not evidence of browser CSP enforcement.
func TestAuthorizationCompletion(t *testing.T) {
	for _, redirect := range []string{"com.example.notes:/oauth/callback?keep=one", "org.example.other:/callback", "https://client.example/callback", "http://127.0.0.1:49152/callback"} {
		for _, decision := range []string{"allow", "cancel", "deny"} {
			t.Run(redirect+"/"+decision, func(t *testing.T) {
				a, _, dataset := authFixture(t)
				a.data.Clients["client"] = Client{ID: "client", Name: `Notes <script>alert(1)</script>`, Redirects: []string{redirect}}
				query := pageQuery(dataset)
				query.Set("redirect_uri", redirect)
				query.Set("state", `state<&"=+`)
				w := signInRequest(t, a, query, map[string]string{"Accept": "text/html"})
				var id string
				for id = range a.flows {
				}
				cookie := w.Result().Cookies()[0]
				post := func(values url.Values, c *http.Cookie) *httptest.ResponseRecorder {
					r := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(values.Encode()))
					r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					r.Header.Set("Accept", "text/html")
					if c != nil {
						r.AddCookie(c)
					}
					w := httptest.NewRecorder()
					a.Handler().ServeHTTP(w, r)
					return w
				}
				first := a.flows[id]
				signIn := url.Values{"flow": {id}, "csrf": {first.CSRF}, "username": {"person"}, "password": {"synthetic-password-only"}, "decision": {"continue"}}
				if decision != "cancel" {
					w = post(signIn, cookie)
					if w.Code != 200 || !strings.Contains(w.Body.String(), `data-page="consent"`) {
						t.Fatal("no consent page", w.Code)
					}
					cookie = w.Result().Cookies()[0]
					// Both the sign-in CSRF and a missing cookie must still fail.
					if bad := post(url.Values{"flow": {id}, "csrf": {first.CSRF}, "decision": {"allow"}}, cookie); bad.Code != 400 {
						t.Fatal("stale CSRF accepted")
					}
				}
				values := url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "decision": {"deny"}, "read": {"yes"}, "redirect_uri": {"org.attacker.app:/steal"}, "return_to": {"https://attacker.example/"}}
				if decision == "allow" {
					values.Set("decision", "allow")
				}
				if bad := post(values, nil); bad.Code != 400 {
					t.Fatal("missing cookie accepted")
				}
				w = post(values, cookie)
				if w.Code != 200 || w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), `data-page="complete"`) {
					t.Fatal("no completion page", w.Code, w.Body.String())
				}
				if w.Header().Get("Content-Security-Policy") != pageSecurity || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
					t.Fatal("completion weakened page protections")
				}
				if strings.Contains(w.Body.String(), "<script") || strings.Contains(w.Body.String(), "attacker") || strings.Contains(w.Body.String(), "ZgotmplZ") || strings.Contains(w.Body.String(), "<form") {
					t.Fatal("unsafe completion markup")
				}
				match := regexp.MustCompile(`href="([^"]+)"`).FindStringSubmatch(w.Body.String())
				if len(match) != 2 {
					t.Fatal("missing return link")
				}
				target, err := url.Parse(html.UnescapeString(match[1]))
				if err != nil {
					t.Fatal(err)
				}
				original, _ := url.Parse(redirect)
				if target.Scheme != original.Scheme || target.Host != original.Host || target.Path != original.Path || target.Query().Get("keep") != original.Query().Get("keep") || target.Query().Get("state") != query.Get("state") || target.Query().Get("iss") != a.issuer {
					t.Fatal("callback not bound to flow")
				}
				if replay := post(values, cookie); replay.Code != 400 {
					t.Fatal("completed form replay accepted")
				}
				if decision != "allow" {
					if target.Query().Get("error") != "access_denied" || target.Query().Get("code") != "" || len(a.data.Codes) != 0 || len(a.data.Authorizations) != 0 {
						t.Fatal("denial authorized access")
					}
					return
				}
				code := target.Query().Get("code")
				if code == "" || len(a.data.Codes) != 1 {
					t.Fatal("missing code")
				}
				exchange := url.Values{"grant_type": {"authorization_code"}, "client_id": {"client"}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {strings.Repeat("y", 43)}}
				if bad := formCall(a, "/oauth/token", exchange, nil); bad.Code != 400 {
					t.Fatal("wrong PKCE accepted")
				}
				exchange.Set("code_verifier", strings.Repeat("x", 43))
				exchange.Set("redirect_uri", redirect+"/different")
				if bad := formCall(a, "/oauth/token", exchange, nil); bad.Code != 400 {
					t.Fatal("wrong redirect accepted")
				}
				exchange.Set("redirect_uri", redirect)
				heldCode := a.data.Codes[digest(code)]
				expiredCode := heldCode
				expiredCode.Expires = time.Now().Add(-time.Minute).Unix()
				a.data.Codes[digest(code)] = expiredCode
				if expired := formCall(a, "/oauth/token", exchange, nil); expired.Code != 400 {
					t.Fatal("expired code accepted")
				}
				a.data.Codes[digest(code)] = heldCode
				if good := formCall(a, "/oauth/token", exchange, nil); good.Code != 200 {
					t.Fatal("exchange failed", good.Code)
				}
				if replay := formCall(a, "/oauth/token", exchange, nil); replay.Code != 400 {
					t.Fatal("code replay accepted")
				}
			})
		}
	}
}

func TestUnregisteredRedirectNeverGetsCompletion(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Redirects: []string{"com.example.notes:/oauth/callback"}}
	for _, bad := range []string{"com.example.notes:/oauth/other", "com.example.notes:/oauth/callback?extra=1", "org.attacker.app:/callback", "javascript:alert(1)", "com.example.notes:/oauth/callback#fragment"} {
		q := pageQuery(dataset)
		q.Set("redirect_uri", bad)
		w := signInRequest(t, a, q, map[string]string{"Accept": "text/html"})
		if w.Code != 400 || w.Header().Get("Location") != "" || strings.Contains(w.Body.String(), "href=") || w.Header().Get("Content-Security-Policy") != pageSecurity || len(a.flows) != 0 {
			t.Fatal("unregistered redirect accepted", bad)
		}
	}
}

func TestExpiredFlowCannotComplete(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Redirects: []string{"https://client.example/callback"}}
	w := signInRequest(t, a, pageQuery(dataset), map[string]string{"Accept": "text/html"})
	var id string
	for id = range a.flows {
	}
	f := a.flows[id]
	f.Expires = time.Now().Add(-time.Minute)
	a.flows[id] = f
	r := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(url.Values{"flow": {id}, "csrf": {f.CSRF}, "decision": {"deny"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "text/html")
	r.AddCookie(w.Result().Cookies()[0])
	result := httptest.NewRecorder()
	a.Handler().ServeHTTP(result, r)
	if result.Code != 400 || result.Header().Get("Location") != "" || !strings.Contains(result.Body.String(), `data-page="restart"`) || len(a.data.Codes) != 0 {
		t.Fatal("expired flow completed")
	}
}
