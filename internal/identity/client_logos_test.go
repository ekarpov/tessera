package identity

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestClientRegistrationLogoSafety(t *testing.T) {
	for _, scenario := range []string{"valid", "active_format", "oversized", "dimensions", "quota"} {
		t.Run(scenario, func(t *testing.T) {
			a, _, dataset := authFixture(t)
			logo, err := os.ReadFile(brandLogoFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "active_format":
				logo = []byte(`<svg onload="alert(1)"/>`)
			case "oversized":
				logo = make([]byte, maxClientLogoBytes+1)
			case "dimensions":
				var b bytes.Buffer
				png.Encode(&b, image.NewGray(image.Rect(0, 0, 257, 1)))
				logo = b.Bytes()
			case "quota":
				a.data.Clients["full"] = Client{Logo: make([]byte, maxClientLogosTotal)}
			}
			raw, err := json.Marshal(map[string]any{"client_name": "Notes <script>", "redirect_uris": []string{"com.example.notes:/callback"}, "logo_png": logo, "operator_console": true})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/oauth/register", bytes.NewReader(raw))
			request.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, request)
			if scenario != "valid" {
				if w.Code != 400 && w.Code != 429 {
					t.Fatal("unsafe client icon accepted", scenario, w.Code)
				}
				return
			}
			var registered struct {
				ID string `json:"client_id"`
			}
			if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &registered) != nil {
				t.Fatal("client logo registration failed", w.Code)
			}
			client := a.data.Clients[registered.ID]
			if client.OperatorConsole || bytes.Contains(client.Logo, []byte("discard this metadata")) {
				t.Fatal("client logo gained trust or retained trailing data")
			}
			q := pageQuery(dataset)
			q.Set("client_id", client.ID)
			q.Set("redirect_uri", client.Redirects[0])
			w = signInRequest(t, a, q, nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `class="client-icon"`) || strings.Contains(w.Body.String(), "<script>") || !strings.Contains(w.Header().Get("Content-Security-Policy"), a.issuer+"/oauth/client-logos/") {
				t.Fatal("unsafe client presentation")
			}
			w = httptest.NewRecorder()
			a.Handler().ServeHTTP(w, httptest.NewRequest("GET", clientLogoPath(client), nil))
			if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("unsafe icon endpoint")
			}
			decoded, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, alpha := decoded.At(0, 0).RGBA()
			if alpha != 0 {
				t.Fatal("client alpha lost")
			}
			reopened, err := Open(t.Context(), a.path, a.issuer, a.engine)
			if err != nil {
				t.Fatal(err)
			}
			if !reopened.hasClientLogos.Load() || len(reopened.data.Clients[client.ID].Logo) == 0 {
				t.Fatal("registration icon lost after restart")
			}
		})
	}
}
