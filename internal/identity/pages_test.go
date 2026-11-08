package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var placeholder = regexp.MustCompile(`%[sd]`)

// Every language carries every message, with the same placeholders in the
// same order, and nothing English lacks: a missing one would fall back to
// English mid-page, and a wrong placeholder prints garbage.
func TestCatalogueIsComplete(t *testing.T) {
	if len(pageLanguages) != 1 || pageLanguages[0] != "en" || len(catalogue) != 1 {
		t.Fatal("v0.1.0 must carry only the English catalogue")
	}
	english := catalogue["en"]
	if len(english) == 0 {
		t.Fatal("the English catalogue is empty")
	}
	for _, lang := range pageLanguages {
		messages, ok := catalogue[lang]
		if !ok {
			t.Fatal("no catalogue for", lang)
		}
		for key, en := range english {
			got, ok := messages[key]
			if !ok || strings.TrimSpace(got) == "" {
				t.Errorf("%s lacks %s", lang, key)
				continue
			}
			if strings.Join(placeholder.FindAllString(got, -1), "") != strings.Join(placeholder.FindAllString(en, -1), "") {
				t.Errorf("%s %s has placeholders %q, English %q", lang, key, got, en)
			}
		}
		for key := range messages {
			if _, ok := english[key]; !ok {
				t.Errorf("%s has %s, which English does not", lang, key)
			}
		}
	}
}

func TestLanguageFollowsAcceptLanguage(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Name: "Notes", Redirects: []string{"https://client.example/callback"}}
	for header, want := range map[string]string{
		"":   "en",
		"de": "en", "fr": "en", "es": "en", "pt": "en", "fi": "en", "ru": "en",
		"de-DE,de;q=0.9,en;q=0.8":    "en",
		"fr;q=0.5, ru;q=0.9":         "en",
		"ja, pt-BR;q=0.7":            "en",
		"zh-CN":                      "en",
		"fi;q=0, es;q=0.3":           "en",
		"EN-gb":                      "en",
		"x-unknown;q=1, de;q=banana": "en",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Language", header)
		if got := language(r); got != want {
			t.Errorf("%q chose %s, want %s", header, got, want)
		}
		w := signInRequest(t, a, pageQuery(dataset), map[string]string{"Accept-Language": header})
		if w.Code != 200 || !strings.Contains(w.Body.String(), `<html lang="en">`) || w.Header().Get("Vary") != "Accept-Language" {
			t.Fatal("page language or cache variation changed", w.Code)
		}
	}
}

func signInRequest(t *testing.T, a *Authority, query url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/oauth/authorize?"+query.Encode(), nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func pageQuery(dataset string) url.Values {
	hash := sha256.Sum256([]byte(strings.Repeat("x", 43)))
	return url.Values{"client_id": {"client"}, "redirect_uri": {"https://client.example/callback"}, "response_type": {"code"},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "state": {"s"}, "dataset": {dataset}}
}

// A page says where the person is, in their language, laid out for a phone,
// and its one style sheet is the one its policy allows.
func TestTheSignInPageSaysWhereAndInWhichLanguage(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Name: "Notes", Redirects: []string{"https://client.example/callback"}}
	a.SetDisplayName("Example Home")
	w := signInRequest(t, a, pageQuery(dataset), map[string]string{"Accept-Language": "de-CH, en;q=0.5"})
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `<html lang="en">`) || !strings.Contains(body, html.EscapeString(catalogue["en"]["signin.title"])) {
		t.Fatal("not the English sign-in page", w.Code, body)
	}
	if !strings.Contains(body, "<strong>Example Home</strong>tessera.example") {
		t.Fatal("the page does not say where the person is")
	}
	if !strings.Contains(body, `name="viewport"`) || !strings.Contains(body, "prefers-color-scheme:dark") {
		t.Fatal("not laid out for a phone in light and dark")
	}
	if w.Header().Get("Vary") != "Accept-Language" {
		t.Fatal("a cache could serve one person's language to another")
	}
	style := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(body)
	sum := sha256.Sum256([]byte(style[1]))
	policy := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "style-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") ||
		!strings.Contains(policy, "default-src 'none'") || strings.Contains(policy, "script-src") {
		t.Fatal("the page's style is not exactly what its policy allows", policy)
	}
	if strings.Contains(body, "<script") {
		t.Fatal("a page carries a script")
	}
}

// A browser is shown a page saying what to do next; an application calling the
// same endpoint gets the OAuth error it always did.
func TestABrowserIsShownAPageAndAnAppTheError(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Name: "Notes", Redirects: []string{"https://client.example/callback"}}
	bad := pageQuery(dataset)
	bad.Set("redirect_uri", "https://attacker.example/")
	w := signInRequest(t, a, bad, map[string]string{"Accept": "text/html,application/xhtml+xml", "Accept-Language": "fi"})
	if w.Code != 400 || !strings.Contains(w.Body.String(), `data-page="error"`) || !strings.Contains(w.Body.String(), html.EscapeString(catalogue["en"]["error.link"])) {
		t.Fatal("a browser was not shown the error page", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "attacker.example") {
		t.Fatal("the error page repeated the unregistered address")
	}
	w = signInRequest(t, a, bad, nil)
	var reply map[string]string
	if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply["error"] != "invalid_request" {
		t.Fatal("an application did not get the OAuth error", w.Code, w.Body.String())
	}
}

// A mistyped password shows the same form again, on the same flow, with what
// went wrong; the right password then still signs in.
func TestAWrongPasswordShowsTheSignInAgain(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Name: "Notes", Redirects: []string{"https://client.example/callback"}}
	w := signInRequest(t, a, pageQuery(dataset), map[string]string{"Accept": "text/html"})
	var id string
	var f flow
	for id, f = range a.flows {
	}
	cookie := w.Result().Cookies()[0]
	post := func(password string) *httptest.ResponseRecorder {
		values := url.Values{"flow": {id}, "csrf": {f.CSRF}, "username": {"person"}, "password": {password}, "decision": {"continue"}}
		r := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Accept", "text/html")
		r.Header.Set("Accept-Language", "es")
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, r)
		return rec
	}
	wrong := post("not-the-password")
	body := wrong.Body.String()
	if wrong.Code != 401 || !strings.Contains(body, `data-page="signin"`) || !strings.Contains(body, html.EscapeString(catalogue["en"]["signin.wrong"])) ||
		!strings.Contains(body, `name="flow" value="`+id+`"`) {
		t.Fatal("a wrong password did not show the sign-in again", wrong.Code, body)
	}
	if right := post("synthetic-password-only"); right.Code != 200 || !strings.Contains(right.Body.String(), `data-page="consent"`) {
		t.Fatal("the right password after a wrong one did not sign in", right.Code)
	}
}

// The approval says in words what the app may do, with no protocol terms.
func TestTheApprovalSaysWhatTheAppMayDo(t *testing.T) {
	for _, lang := range pageLanguages {
		for _, key := range []string{"consent.read", "consent.write", "consent.schema", "consent.erase", "consent.insert", "consent.title"} {
			for _, term := range []string{"client", "scope", "grant", "dataset", "schema", "OAuth"} {
				if strings.Contains(strings.ToLower(catalogue[lang][key]), strings.ToLower(term)) {
					t.Errorf("%s %s says %q", lang, key, term)
				}
			}
		}
	}
}
