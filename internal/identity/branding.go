package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// BrandConfig is local operator configuration, never OAuth registration metadata.
// Configure before serving requests. Nil preserves the default pages.
type BrandConfig struct {
	Name         string        `json:"name"`
	LogoFile     string        `json:"logo_file,omitempty"`
	Light        *BrandPalette `json:"light,omitempty"`
	Dark         *BrandPalette `json:"dark,omitempty"`
	WebClientIDs []string      `json:"web_client_ids,omitempty"`
}

type BrandPalette struct {
	Accent  string `json:"accent,omitempty"`
	Surface string `json:"surface,omitempty"`
	Text    string `json:"text,omitempty"`
}

type pageBrand struct {
	name, css, policy string
	logo              []byte
	webClients        map[string]bool
}

const maxBrandLogoBytes = 256 << 10
const maxBrandLogoDimension = 512

func brandText(s string) bool {
	return len(s) <= 200 && utf8.ValidString(s) && strings.TrimSpace(s) != "" && !strings.ContainsFunc(s, unicode.IsControl)
}

var brandColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

func luminance(color string) float64 {
	total := 0.0
	for i, weight := range []float64{0.2126, 0.7152, 0.0722} {
		n, _ := strconv.ParseUint(color[1+i*2:3+i*2], 16, 8)
		c := float64(n) / 255
		if c <= 0.04045 {
			c /= 12.92
		} else {
			c = math.Pow((c+0.055)/1.055, 2.4)
		}
		total += c * weight
	}
	return total
}

func contrast(a, b string) float64 {
	x, y := luminance(a), luminance(b)
	return (math.Max(x, y) + 0.05) / (math.Min(x, y) + 0.05)
}

func paletteCSS(p *BrandPalette, defaults BrandPalette) (string, error) {
	if p != nil {
		if p.Accent != "" {
			defaults.Accent = p.Accent
		}
		if p.Surface != "" {
			defaults.Surface = p.Surface
		}
		if p.Text != "" {
			defaults.Text = p.Text
		}
	}
	for _, c := range []string{defaults.Accent, defaults.Surface, defaults.Text} {
		if !brandColor.MatchString(c) {
			return "", errors.New("identity_brand colors must use #RRGGBB")
		}
	}
	if contrast(defaults.Text, defaults.Surface) < 4.5 || contrast(defaults.Accent, defaults.Surface) < 4.5 {
		return "", errors.New("identity_brand text and accent must each contrast with surface by at least 4.5:1")
	}
	buttonText := "#ffffff"
	if contrast(defaults.Accent, "#000000") > contrast(defaults.Accent, buttonText) {
		buttonText = "#000000"
	}
	return fmt.Sprintf("--bg:%s;--fg:%s;--muted:%s;--line:%s;--field:%s;--accent:%s;--accent-fg:%s;--alert:%s;--alert-bg:%s;", defaults.Surface, defaults.Text, defaults.Text, defaults.Text, defaults.Surface, defaults.Accent, buttonText, defaults.Text, defaults.Surface), nil
}

func loadBrandLogo(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("identity_brand logo_file must be an absolute local PNG path")
	}
	// Refuse special files before opening: a FIFO must not stall startup.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBrandLogoBytes {
		return nil, errors.New("identity_brand logo_file must be a readable regular PNG file of at most 256 KiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read identity_brand logo_file")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBrandLogoBytes {
		return nil, errors.New("identity_brand logo_file must be a regular PNG file of at most 256 KiB")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBrandLogoBytes+1))
	if err != nil || len(raw) > maxBrandLogoBytes {
		return nil, errors.New("cannot read bounded identity_brand logo_file")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxBrandLogoDimension || cfg.Height > maxBrandLogoDimension {
		return nil, errors.New("identity_brand logo must be PNG, at most 512 by 512 pixels")
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("invalid identity_brand PNG")
	}
	// Re-encode pixels: no original metadata, trailing bytes or active formats
	// are ever served. Dimensions bound decoded and encoded resource use.
	var clean bytes.Buffer
	if png.Encode(&clean, img) != nil {
		return nil, errors.New("cannot encode identity_brand PNG")
	}
	return clean.Bytes(), nil
}

func compileBrand(config *BrandConfig, issuer string) (*pageBrand, error) {
	if config == nil {
		return nil, nil
	}
	if !brandText(config.Name) {
		return nil, errors.New("identity_brand name must be valid text of 1 to 200 bytes without control characters")
	}
	if len(config.WebClientIDs) > 64 {
		return nil, errors.New("identity_brand supports at most 64 web_client_ids")
	}
	b := &pageBrand{name: config.Name, webClients: map[string]bool{}}
	for _, id := range config.WebClientIDs {
		if !brandText(id) || strings.TrimSpace(id) != id || b.webClients[id] {
			return nil, errors.New("identity_brand web_client_ids must be distinct nonempty client IDs of at most 200 bytes")
		}
		b.webClients[id] = true
	}
	light, err := paletteCSS(config.Light, BrandPalette{Accent: "#0a64d8", Surface: "#ffffff", Text: "#1c1c1e"})
	if err != nil {
		return nil, err
	}
	dark, err := paletteCSS(config.Dark, BrandPalette{Accent: "#4c9bff", Surface: "#000000", Text: "#f2f2f7"})
	if err != nil {
		return nil, err
	}
	b.css = pageCSS + "\n:root{" + light + "}\n@media (prefers-color-scheme:dark){:root{" + dark + "}}\nheader img{display:block;max-width:160px;max-height:64px;width:auto;height:auto;margin-bottom:8px}"
	sum := sha256.Sum256([]byte(b.css))
	b.policy = "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; form-action 'self'; frame-ancestors 'none'"
	if config.LogoFile != "" {
		b.logo, err = loadBrandLogo(config.LogoFile)
		if err != nil {
			return nil, err
		}
		origin, err := url.Parse(issuer)
		if err != nil || origin.Host == "" || (origin.Scheme != "https" && origin.Scheme != "http") || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" {
			return nil, errors.New("identity_brand requires an HTTP(S) issuer origin")
		}
		b.policy += "; img-src " + origin.String() + "/oauth/brand/logo"
	}
	return b, nil
}

// SetBrand validates and loads an immutable operator brand. Errors leave the
// previous configuration intact. Call before serving, not as a live theme API.
func (a *Authority) SetBrand(config *BrandConfig) error {
	b, err := compileBrand(config, a.issuer)
	if err != nil {
		return err
	}
	a.brand.Store(b)
	return nil
}

func (a *Authority) brandLogo(w http.ResponseWriter, r *http.Request) {
	b := a.brand.Load()
	if b == nil || len(b.logo) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	_, _ = w.Write(b.logo)
}

func (a *Authority) webConsole(id, redirect string) bool {
	b := a.brand.Load()
	if b == nil || !b.webClients[id] {
		return false
	}
	u, err := url.Parse(redirect)
	return err == nil && u.Scheme == "https" && u.Host != ""
}
