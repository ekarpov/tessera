package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"github.com/ekarpov/tessera/pkg/store"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func brandLogoFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logo.png")
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 24, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 24; x++ {
			img.Set(x, y, color.RGBA{R: 20, G: 60, B: 140, A: 255})
		}
	}
	img.Set(0, 0, color.RGBA{})
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b.Bytes(), []byte("discard this metadata")...), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBrandValidation(t *testing.T) {
	logo := brandLogoFixture(t)
	invalid := filepath.Join(t.TempDir(), "logo.svg")
	os.WriteFile(invalid, []byte(`<svg onload="alert(1)"/>`), 0600)
	large := filepath.Join(t.TempDir(), "large.png")
	os.WriteFile(large, make([]byte, maxBrandLogoBytes+1), 0600)
	wide := filepath.Join(t.TempDir(), "wide.png")
	var encoded bytes.Buffer
	png.Encode(&encoded, image.NewGray(image.Rect(0, 0, 513, 1)))
	os.WriteFile(wide, encoded.Bytes(), 0600)
	for _, tc := range []struct {
		name   string
		config BrandConfig
	}{
		{"empty_name", BrandConfig{}},
		{"long_name", BrandConfig{Name: strings.Repeat("x", 201)}},
		{"control", BrandConfig{Name: "Home\nOther"}},
		{"css", BrandConfig{Name: "Home", Light: &BrandPalette{Accent: "red;display:none"}}},
		{"low_text_contrast", BrandConfig{Name: "Home", Light: &BrandPalette{Text: "#ffffff"}}},
		{"low_accent_contrast", BrandConfig{Name: "Home", Light: &BrandPalette{Accent: "#eeeeee"}}},
		{"low_dark_contrast", BrandConfig{Name: "Home", Dark: &BrandPalette{Text: "#111111"}}},
		{"remote_logo", BrandConfig{Name: "Home", LogoFile: "https://example.com/logo.png"}},
		{"relative_logo", BrandConfig{Name: "Home", LogoFile: "logo.png"}},
		{"svg", BrandConfig{Name: "Home", LogoFile: invalid}},
		{"large", BrandConfig{Name: "Home", LogoFile: large}},
		{"wide", BrandConfig{Name: "Home", LogoFile: wide}},
		{"missing_logo", BrandConfig{Name: "Home", LogoFile: filepath.Join(t.TempDir(), "absent")}},
		{"directory", BrandConfig{Name: "Home", LogoFile: t.TempDir()}},
		{"duplicate_client", BrandConfig{Name: "Home", WebClientIDs: []string{"console", "console"}}},
		{"empty_client", BrandConfig{Name: "Home", WebClientIDs: []string{""}}},
		{"too_many_clients", BrandConfig{Name: "Home", WebClientIDs: make([]string, 65)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := authFixture(t)
			if err := a.SetBrand(&BrandConfig{Name: "Safe", LogoFile: logo}); err != nil {
				t.Fatal(err)
			}
			before := a.brand.Load()
			if err := a.SetBrand(&tc.config); err == nil {
				t.Fatal("invalid brand accepted")
			}
			if a.brand.Load() != before {
				t.Fatal("invalid brand replaced safe brand")
			}
		})
	}
}

func assertBrandPage(t *testing.T, a *Authority, w *httptest.ResponseRecorder) {
	t.Helper()
	b := a.brand.Load()
	body := w.Body.String()
	if !strings.Contains(body, `<strong>`+b.name+`</strong>`) || !strings.Contains(body, `src="/oauth/brand/logo"`) {
		t.Fatal("brand missing", w.Code, body)
	}
	style := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(body)
	if len(style) != 2 {
		t.Fatal("style missing")
	}
	sum := sha256.Sum256([]byte(style[1]))
	policy := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "style-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") || !strings.Contains(policy, "img-src "+a.issuer+"/oauth/brand/logo") || !strings.Contains(policy, "form-action 'self'") || !strings.Contains(policy, "default-src 'none'") || !strings.Contains(policy, "frame-ancestors 'none'") || strings.Contains(policy, "unsafe-inline") || strings.Contains(policy, "script-src") {
		t.Fatal("unsafe brand policy", policy)
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "ZgotmplZ") || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("unsafe brand page")
	}
}

func TestBrandedAuthorizationPages(t *testing.T) {
	for _, web := range []bool{false, true} {
		for _, decision := range []string{"allow", "cancel", "deny"} {
			t.Run(map[bool]string{true: "web", false: "native"}[web]+"/"+decision, func(t *testing.T) {
				a, engine, dataset := authFixture(t)
				callback := "com.example.synthetic:/oauth/callback"
				if web {
					callback = "https://client.example/callback"
				}
				a.data.Clients["client"] = Client{ID: "client", Name: `Console <script>`, Redirects: []string{callback}}
				if err := a.SetBrand(&BrandConfig{Name: "Synthetic Home", LogoFile: brandLogoFixture(t), WebClientIDs: []string{"client"}}); err != nil {
					t.Fatal(err)
				}
				q := pageQuery(dataset)
				q.Set("redirect_uri", callback)
				q.Set("scope", "read write")
				w := signInRequest(t, a, q, map[string]string{"Accept": "text/html"})
				assertBrandPage(t, a, w)
				if !strings.Contains(w.Body.String(), "Sign in to Synthetic Home") || strings.Contains(w.Body.String(), "designated by the server operator") != web {
					t.Fatal("wrong identity wording")
				}
				var id string
				for id = range a.flows {
				}
				cookie := w.Result().Cookies()[0]
				if decision != "cancel" {
					w = browserPost(a, url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "decision": {"continue"}, "username": {"person"}, "password": {"synthetic-password-only"}}, cookie)
					assertBrandPage(t, a, w)
					if !strings.Contains(w.Body.String(), `data-page="consent"`) {
						t.Fatal("no consent")
					}
					cookie = w.Result().Cookies()[0]
				}
				action := "deny"
				if decision == "allow" {
					action = "allow"
				}
				form := url.Values{"flow": {id}, "csrf": {a.flows[id].CSRF}, "decision": {action}, "read": {"yes"}, "write": {"yes"}}
				w = browserPost(a, form, cookie)
				assertBrandPage(t, a, w)
				if w.Code != 200 || strings.Contains(w.Body.String(), "Return to website") != web {
					t.Fatal("completion changed")
				}
				target, _ := url.Parse(pageLink(t, w))
				if decision == "allow" {
					token := formCall(a, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"client"}, "redirect_uri": {callback}, "code": {target.Query().Get("code")}, "code_verifier": {strings.Repeat("x", 43)}}, nil)
					if token.Code != 200 {
						t.Fatal("callback exchange failed", token.Code)
					}
					for _, auth := range a.data.Authorizations {
						info, err := engine.Info(t.Context(), store.Access{Principal: auth.Principal, Authorization: auth.ID}, dataset)
						if err != nil || !info.Access.Read || !info.Access.Write || info.Access.Insert || info.Access.Schema || info.Access.Erase {
							t.Fatal("branding changed grants", err, info.Access)
						}
					}
				} else if target.Query().Get("error") != "access_denied" || len(a.data.Authorizations) != 0 {
					t.Fatal("denial granted access")
				}
				w = browserPost(a, form, cookie)
				assertBrandPage(t, a, w)
				if w.Code != 400 || !strings.Contains(w.Body.String(), `data-page="restart"`) {
					t.Fatal("no branded recovery")
				}
				bad := q.Clone()
				bad.Set("redirect_uri", "https://attacker.example/")
				w = signInRequest(t, a, bad, map[string]string{"Accept": "text/html"})
				assertBrandPage(t, a, w)
				if w.Code != 400 || strings.Contains(w.Body.String(), "attacker") {
					t.Fatal("unsafe branded error")
				}
			})
		}
	}
}

func TestBrandLogoAndUntrustedClient(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Name: "Operator-looking website", Redirects: []string{"https://client.example/callback"}}
	if err := a.SetBrand(&BrandConfig{Name: "Trusted Home", LogoFile: brandLogoFixture(t), WebClientIDs: []string{"different-client"}}); err != nil {
		t.Fatal(err)
	}
	w := signInRequest(t, a, pageQuery(dataset), nil)
	if strings.Contains(w.Body.String(), "designated by the server operator") {
		t.Fatal("public client gained trusted website designation")
	}
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/oauth/brand/logo", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff" || bytes.Contains(w.Body.Bytes(), []byte("discard this metadata")) {
		t.Fatal("unsafe logo response")
	}
	decoded, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, alpha := decoded.At(0, 0).RGBA()
	if alpha != 0 {
		t.Fatal("transparent logo lost alpha")
	}
	if err := a.SetBrand(nil); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/oauth/brand/logo", nil))
	if w.Code != 404 {
		t.Fatal("unbranded deployment exposes logo")
	}
}

func TestRegistrationCannotDesignateWebsite(t *testing.T) {
	a, _, dataset := authFixture(t)
	if err := a.SetBrand(&BrandConfig{Name: "Operator Home"}); err != nil {
		t.Fatal(err)
	}
	body := `{"client_name":"Pretend first party","redirect_uris":["https://client.example/callback"],"application_type":"web","logo_uri":"https://attacker.invalid/logo.svg","identity_brand":{"name":"Spoof Home","web_client_ids":["client"]}}`
	r := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	var registered struct {
		ID string `json:"client_id"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &registered) != nil {
		t.Fatal("registration failed", w.Code)
	}
	q := pageQuery(dataset)
	q.Set("client_id", registered.ID)
	w = signInRequest(t, a, q, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "Spoof Home") || strings.Contains(w.Body.String(), "designated by the server operator") || strings.Contains(w.Body.String(), "attacker.invalid") || !strings.Contains(w.Body.String(), "<strong>Operator Home</strong>") {
		t.Fatal("registration changed trusted branding")
	}
}

func TestBrandedExpiredPageAndAccountPages(t *testing.T) {
	a, _, dataset := authFixture(t)
	a.data.Clients["client"] = Client{ID: "client", Name: "Console", Redirects: []string{"https://client.example/callback"}}
	if err := a.SetBrand(&BrandConfig{Name: "Operator Home", LogoFile: brandLogoFixture(t), WebClientIDs: []string{"client"}}); err != nil {
		t.Fatal(err)
	}
	q := pageQuery(dataset)
	q.Set("scope", "read write")
	w := signInRequest(t, a, q, map[string]string{"Accept": "text/html"})
	var id string
	for id = range a.flows {
	}
	f := a.flows[id]
	f.Expires = time.Now().Add(-time.Minute)
	a.flows[id] = f
	a.recoveries[id] = f
	w = browserPost(a, url.Values{"flow": {id}, "csrf": {f.CSRF}, "decision": {"continue"}}, w.Result().Cookies()[0])
	assertBrandPage(t, a, w)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `data-page="restart"`) {
		t.Fatal("expired page did not recover")
	}
	restart, _ := url.Parse(pageLink(t, w))
	if restart.Query().Get("scope") != "read write" {
		t.Fatal("branded expiry lost hints")
	}
	for _, route := range []string{"/oauth/invitation?code=invalid", "/oauth/account/delete", "/oauth/account/password", "/oauth/account/recovery", "/oauth/account/recover"} {
		w = httptest.NewRecorder()
		a.Handler().ServeHTTP(w, httptest.NewRequest("GET", route, nil))
		assertBrandPage(t, a, w)
	}
}
