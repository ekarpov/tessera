package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Opt-in because Go's normal test environment need not have Playwright/WebKit.
// make browser-auth requires the runtime, rather than silently skipping it.
func TestWebKitAuthorizationCompletion(t *testing.T) {
	python := os.Getenv("TESSERA_WEBKIT_PYTHON")
	if python == "" {
		t.Skip("browser acceptance not run: set TESSERA_WEBKIT_PYTHON to a Python with Playwright and WebKit installed")
	}
	a, _, dataset := authFixture(t)
	server := httptest.NewUnstartedServer(a.Handler())
	// Set immutable authority configuration before HTTP goroutines can read it.
	// External browser traffic does not establish Go race-detector synchronization.
	a.issuer = "http://" + server.Listener.Addr().String()
	server.Start()
	defer server.Close()
	// Loopback-only synthetic fixture; production cookies remain Secure on HTTPS.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "testdata/oauth_webkit.py", server.URL, dataset)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("WebKit: %v\n%s", err, output)
	} else {
		t.Log(string(output))
		a.mu.Lock()
		defer a.mu.Unlock()
		if len(a.data.Authorizations) != 1 || len(a.data.Codes) != 0 {
			t.Fatal("browser recovery duplicated grants or reissued a code")
		}
	}
}

func TestWebKitIdentityBranding(t *testing.T) {
	python := os.Getenv("TESSERA_WEBKIT_PYTHON")
	if python == "" {
		t.Skip("browser acceptance not run: set TESSERA_WEBKIT_PYTHON to Python with Playwright/WebKit")
	}
	for _, width := range []string{"320", "1280"} {
		for _, theme := range []string{"light", "dark"} {
			for _, kind := range []string{"web", "native"} {
				t.Run(width+"/"+theme+"/"+kind, func(t *testing.T) {
					a, _, dataset := authFixture(t)
					server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/callback" {
							w.Header().Set("Content-Type", "text/plain")
							w.Write([]byte("Synthetic callback received"))
							return
						}
						a.Handler().ServeHTTP(w, r)
					}))
					defer server.Close()
					a.issuer = "https://" + server.Listener.Addr().String()
					callback := a.issuer + "/callback"
					if kind == "native" {
						callback = "com.example.synthetic:/oauth/callback"
					}
					a.data.Clients["console"] = Client{ID: "console", Name: "Synthetic Console", Redirects: []string{callback}}
					if err := a.SetBrand(&BrandConfig{Name: "Synthetic Home", LogoFile: brandLogoFixture(t), WebClientIDs: []string{"console"}, Light: &BrandPalette{Accent: "#174b91"}, Dark: &BrandPalette{Accent: "#9ec7ff"}}); err != nil {
						t.Fatal(err)
					}
					server.StartTLS()
					ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
					defer cancel()
					screenshots := os.Getenv("TESSERA_BRAND_SCREENSHOTS")
					cmd := exec.CommandContext(ctx, python, "testdata/branding_webkit.py", server.URL, dataset, callback, width, theme, kind, screenshots)
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("WebKit branding: %v\n%s", err, output)
					} else {
						t.Log(string(output))
					}
					a.mu.Lock()
					defer a.mu.Unlock()
					if len(a.data.Authorizations) != 1 || len(a.data.Codes) != 0 {
						t.Fatal("branding created unexpected grants or codes")
					}
					for _, auth := range a.data.Authorizations {
						if !auth.Grants.Read || !auth.Grants.Write || auth.Grants.Schema || auth.Grants.Erase || auth.Grants.Insert || auth.Administer || auth.CreateDataset {
							t.Fatal("browser branding changed consent grants")
						}
					}
				})
			}
		}
	}
}

func TestWebKitFirstPartyConsole(t *testing.T) {
	python := os.Getenv("TESSERA_WEBKIT_PYTHON")
	if python == "" {
		t.Skip("browser acceptance not run: set TESSERA_WEBKIT_PYTHON")
	}
	for _, width := range []string{"320", "1280"} {
		for _, theme := range []string{"light", "dark"} {
			t.Run(width+"/"+theme, func(t *testing.T) {
				a, _, dataset := authFixture(t)
				callbackServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					w.Write([]byte("Synthetic Console signed in"))
				}))
				defer callbackServer.Close()
				server := httptest.NewUnstartedServer(a.Handler())
				defer server.Close()
				a.issuer = "https://" + server.Listener.Addr().String()
				cfg := consoleConfig()
				cfg.Origin = callbackServer.URL
				cfg.RedirectURI = callbackServer.URL + "/callback"
				if err := a.SetConsoles([]ConsoleConfig{cfg}); err != nil {
					t.Fatal(err)
				}
				if err := a.SetBrand(&BrandConfig{Name: "Synthetic Home", LogoFile: brandLogoFixture(t)}); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				server.StartTLS()
				cmd := exec.CommandContext(ctx, python, "testdata/console_webkit.py", server.URL, dataset, callbackServer.URL, width, theme, os.Getenv("TESSERA_BRAND_SCREENSHOTS"))
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("WebKit Console: %v\n%s", err, output)
				} else {
					t.Log(string(output))
				}
			})
		}
	}
}

func TestWebKitNativeConsentCustomization(t *testing.T) {
	python := os.Getenv("TESSERA_WEBKIT_PYTHON")
	if python == "" {
		t.Skip("browser acceptance not run: set TESSERA_WEBKIT_PYTHON")
	}
	for _, width := range []string{"320", "1280"} {
		for _, theme := range []string{"light", "dark"} {
			t.Run(width+"/"+theme, func(t *testing.T) {
				a, _, dataset := authFixture(t)
				server := httptest.NewUnstartedServer(a.Handler())
				defer server.Close()
				a.issuer = "http://" + server.Listener.Addr().String()
				if err := a.SetBrand(&BrandConfig{Name: "Synthetic Home", LogoFile: brandLogoFixture(t)}); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				server.Start()
				cmd := exec.CommandContext(ctx, python, "testdata/native_consent_webkit.py", server.URL, dataset, width, theme, brandLogoFixture(t), os.Getenv("TESSERA_BRAND_SCREENSHOTS"))
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("WebKit native consent: %v\n%s", err, output)
				} else {
					t.Log(string(output))
				}
				a.mu.Lock()
				defer a.mu.Unlock()
				if len(a.data.Authorizations) != 3 || len(a.data.Codes) != 0 {
					t.Fatal("native reauthorization issued unexpected credentials")
				}
				for _, auth := range a.data.Authorizations {
					if !auth.Grants.Read || auth.Grants.Write || auth.Grants.Schema || auth.Grants.Erase || auth.Grants.Insert || auth.Administer || auth.Console != nil {
						t.Fatal("native token exceeded submitted/reused consent")
					}
				}
			})
		}
	}
}
