package identity

import (
	"bytes"
	"errors"
	"image/png"
	"net/http"
	"net/url"
)

const maxClientLogoBytes = 16 << 10
const maxClientLogoPixels = 256
const maxStoredClientLogoBytes = 64 << 10
const maxClientLogosTotal = 8 << 20

func sanitizeClientLogo(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxClientLogoBytes {
		return nil, errors.New("client logo exceeds bound")
	}
	config, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > maxClientLogoPixels || config.Height > maxClientLogoPixels {
		return nil, errors.New("client logo must be a bounded PNG")
	}
	pixels, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("invalid client logo PNG")
	}
	var clean bytes.Buffer
	if err = png.Encode(&clean, pixels); err != nil || clean.Len() > maxStoredClientLogoBytes {
		return nil, errors.New("encoded client logo exceeds bound")
	}
	return clean.Bytes(), nil
}

func clientLogoPath(c Client) string {
	if len(c.Logo) == 0 {
		return ""
	}
	return "/oauth/client-logos/" + url.PathEscape(c.ID)
}

func (a *Authority) clientLogo(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	logo := a.data.Clients[r.PathValue("client")].Logo
	a.mu.Unlock()
	if len(logo) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	_, _ = w.Write(logo)
}

func (a *Authority) logoBudget() int {
	total := 0
	for _, c := range a.data.Clients {
		total += len(c.Logo)
	}
	return total
}
