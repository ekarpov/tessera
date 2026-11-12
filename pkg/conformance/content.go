package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// CheckContent exercises the required original/offset surface on a disposable
// dataset whose token has read/write and whose content capability is enabled.
// contentCall is the transfer and control request one check hands another.
type contentCall func(method, target, contentRange string, body []byte, status int) (map[string]any, error)

func newContentCall(ctx context.Context, client *http.Client, baseURL, token string) contentCall {
	if client == nil {
		client = http.DefaultClient
	}
	return func(method, target, contentRange string, body []byte, status int) (map[string]any, error) {
		if len(target) > 0 && target[0] == '/' {
			target = baseURL + target
		}
		req, e := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if e != nil {
			return nil, e
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if contentRange != "" {
			req.Header.Set("Content-Range", contentRange)
		} else if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, e := client.Do(req)
		if e != nil {
			return nil, fmt.Errorf("content request failed")
		}
		defer res.Body.Close()
		data, e := io.ReadAll(io.LimitReader(res.Body, 2<<20))
		if e != nil {
			return nil, e
		}
		if res.StatusCode != status {
			return nil, fmt.Errorf("content %s expected HTTP %d, received %d", method, status, res.StatusCode)
		}
		result := map[string]any{}
		if len(data) > 0 && json.Unmarshal(data, &result) != nil {
			return nil, fmt.Errorf("invalid content response")
		}
		return result, nil
	}
}

func CheckContent(ctx context.Context, f CoreFixture) error {
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	call := newContentCall(ctx, client, f.URL, f.Token)
	listing, err := call("GET", "/v1/stores", "", nil, 200)
	if err != nil {
		return err
	}
	stores, ok := listing["stores"].([]any)
	if !ok || len(stores) != 1 {
		return fmt.Errorf("content fixture requires one store")
	}
	id := stores[0].(map[string]any)["id"].(string)
	advertised := map[string]bool{}
	if declared, listed := stores[0].(map[string]any)["capabilities"].([]any); listed {
		for _, name := range declared {
			if text, isText := name.(string); isText {
				advertised[text] = true
			}
		}
	}
	base := "/v1/stores/" + url.PathEscape(id)
	request := []byte(`{"operation_id":"conformance-import","content_type":"application/octet-stream","byte_count":3,"maximum_bytes":20}`)
	opened, err := call("POST", base+"/imports", "", request, 200)
	if err != nil {
		return err
	}
	upload, ok := opened["upload_id"].(string)
	if !ok {
		return fmt.Errorf("missing upload")
	}
	target, err := issuedURI(opened["url"], "the transfer URI")
	if err != nil {
		return err
	}
	if _, err = call("POST", base+"/imports/"+upload+":commit", "", nil, 409); err != nil {
		return err
	}
	for i := 0; i < 2; i++ {
		if _, err = call("PATCH", target, "bytes 0-2/3", []byte("abc"), 204); err != nil {
			return err
		}
	}
	if _, err = call("PATCH", target, "bytes 0-2/3", []byte("xyz"), 409); err != nil {
		return err
	}
	committed, err := call("POST", base+"/imports/"+upload+":commit", "", nil, 200)
	if err != nil {
		return err
	}
	item, ok := committed["item_id"].(string)
	if !ok {
		return fmt.Errorf("missing item identity")
	}
	resumed, err := call("POST", base+"/imports", "", request, 200)
	if err != nil {
		return err
	}
	if resumed["state"] != "committed" || resumed["item_id"] != item {
		return fmt.Errorf("commit recovery changed identity")
	}
	inspect, err := call("GET", base+"/items/"+item+"?representation=original", "", nil, 200)
	if err != nil {
		return err
	}
	if inspect["byte_count"] != float64(3) {
		return fmt.Errorf("incorrect content size")
	}
	// An optional representation the store does not advertise is a missing
	// capability, refused 403 not_supported. 415 is reserved for a capability
	// that is advertised but cannot be produced for this item. Neither refusal
	// may answer with the original instead.
	//
	// This fixture's item is three bytes of application/octet-stream, so a
	// store that advertises thumbnails still cannot produce one for it.
	thumbnailStatus, currentStatus := 403, 403
	if advertised["thumbnails"] {
		thumbnailStatus = 415
	}
	if advertised["current_representation"] {
		currentStatus = 415
	}
	if _, err = call("POST", base+"/items/"+item+":resolve", "", []byte(`{"representation":"thumbnail","maximum_pixel_size":64}`), thumbnailStatus); err != nil {
		return err
	}
	if _, err = call("POST", base+"/items/"+item+":resolve", "", []byte(`{"representation":"current"}`), currentStatus); err != nil {
		return err
	}
	// An advertised thumbnails capability has to actually produce one, or the
	// advertisement is the only evidence for it. This imports a real image so
	// that the positive path is proven against the same store that just
	// refused a thumbnail of an undecodable item.
	if advertised["thumbnails"] {
		if err = checkThumbnail(ctx, call, client, base); err != nil {
			return err
		}
	}
	// A malformed combination is neither: it is a bad request.
	if _, err = call("POST", base+"/items/"+item+":resolve", "", []byte(`{"representation":"thumbnail"}`), 400); err != nil {
		return err
	}
	if _, err = call("POST", base+"/items/"+item+":resolve", "", []byte(`{"representation":"invented"}`), 400); err != nil {
		return err
	}
	resolved, err := call("POST", base+"/items/"+item+":resolve", "", []byte(`{"representation":"original"}`), 200)
	if err != nil {
		return err
	}
	readURI, err := issuedURI(resolved["url"], "the resolved read URI")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", readURI, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4))
	if err != nil || res.StatusCode != 200 || !bytes.Equal(data, []byte("abc")) {
		return fmt.Errorf("transferred bytes changed")
	}
	return checkRetryMayDeclareTheTotal(call, base)
}

// checkRetryMayDeclareTheTotal settles the case between the two transfer
// rules: the same bytes at the same offset, carrying a concrete total where
// the earlier send carried `*`.
//
// It is not an identical retry, so the rule that a retry MUST succeed does not
// reach it; it is not different bytes, so the rule that a conflict MUST fail
// does not either. Which way a store answered therefore depended on what it
// happened to key idempotency on, and the specification did not say.
//
// It is also the first thing a client reaches for on an unknown-length
// import. The completing chunk must name a total, and a client streaming from
// a source that will not state its length does not know which chunk is last
// until the stream ends — so it sends the last one again with the total filled
// in. A consumer read the retry rule as settling this, and wrote a defect on
// the strength of it. That is why the case is here and not only the sentence.
func checkRetryMayDeclareTheTotal(call contentCall, base string) error {
	const chunk = 256 << 10
	body := make([]byte, chunk)
	for i := range body {
		body[i] = byte(i % 251)
	}
	total := strconv.Itoa(2 * chunk)
	// Opened with no byte_count: the length is genuinely unknown, which is the
	// only way the `*` in the ranges below is honest.
	request := []byte(`{"operation_id":"conformance-unknown-length","content_type":"application/octet-stream","maximum_bytes":` + strconv.Itoa(4*chunk) + `}`)
	opened, err := call("POST", base+"/imports", "", request, 200)
	if err != nil {
		return err
	}
	target, err := issuedURI(opened["url"], "the transfer URI")
	if err != nil {
		return err
	}
	upload, uploaded := opened["upload_id"].(string)
	if !uploaded {
		return fmt.Errorf("an unknown-length import did not open")
	}
	if _, err = call("PATCH", target, "bytes 0-"+strconv.Itoa(chunk-1)+"/*", body, 204); err != nil {
		return fmt.Errorf("a first chunk of unknown total was refused: %w", err)
	}
	last := "bytes " + strconv.Itoa(chunk) + "-" + strconv.Itoa(2*chunk-1)
	if _, err = call("PATCH", target, last+"/*", body, 204); err != nil {
		return fmt.Errorf("a further chunk of unknown total was refused: %w", err)
	}
	// Rejoined before the retry, because that is how a client that lost its
	// transfer URI gets another: replaying the import returns the same upload
	// with a fresh URI and the store's own received_bytes. A retry is
	// identified by its offset and its bytes, not by the URI it arrives on, so
	// the retry below is the same retry even though it arrives somewhere else.
	//
	// A consumer asked this and could only trace it, never run it: the case
	// existed in no suite and the answer in no document. Both now.
	rejoined, err := call("POST", base+"/imports", "", request, 200)
	if err != nil {
		return fmt.Errorf("an open import could not be rejoined: %w", err)
	}
	resumed, err := issuedURI(rejoined["url"], "the rejoined transfer URI")
	if err != nil {
		return err
	}
	if resumed == target {
		return fmt.Errorf("rejoining an import did not issue a fresh transfer URI")
	}
	if again, sameUpload := rejoined["upload_id"].(string); !sameUpload || again != upload {
		return fmt.Errorf("rejoining an import did not return the same upload")
	}
	if received, reported := rejoined["received_bytes"].(float64); !reported || int64(received) != 2*chunk {
		return fmt.Errorf("rejoining an import did not report the bytes it holds")
	}
	// The case itself: same offset, same bytes, now a total — and arriving on
	// the URI the rejoin issued rather than the one the first send used.
	if _, err = call("PATCH", resumed, last+"/"+total, body, 204); err != nil {
		return fmt.Errorf("a retry carrying the total where the first send carried * was refused: %w", err)
	}
	target = resumed
	// Establishing it does not make it negotiable. A different total is the
	// conflict the rule above already describes.
	if _, err = call("PATCH", target, last+"/"+strconv.Itoa(2*chunk+1), body, 409); err != nil {
		return fmt.Errorf("a retry contradicting an established total was not refused: %w", err)
	}
	if _, err = call("POST", base+"/imports/"+upload+":commit", "", nil, 200); err != nil {
		return fmt.Errorf("an import whose total a retry established would not commit: %w", err)
	}
	return nil
}

// pngFixture is a 16x8 gradient, encoded here rather than vendored so the
// suite stays a single importable package with no data files beside it.
func pngFixture() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 16, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 16), G: uint8(y * 32), B: 70, A: 255})
		}
	}
	out := bytes.NewBuffer(nil)
	if err := png.Encode(out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// checkThumbnail imports an image and resolves a bounded rendition of it. The
// rendition must be smaller than the original and must not be the original
// bytes: returning the source for a thumbnail is the substitution the content
// protocol forbids, and it is invisible unless something compares them.
func checkThumbnail(ctx context.Context, call func(method, target, contentRange string, body []byte, status int) (map[string]any, error), client *http.Client, base string) error {
	source, err := pngFixture()
	if err != nil {
		return err
	}
	size := strconv.Itoa(len(source))
	request := []byte(`{"operation_id":"conformance-image","content_type":"image/png","byte_count":` + size + `,"maximum_bytes":` + size + `}`)
	opened, err := call("POST", base+"/imports", "", request, 200)
	if err != nil {
		return err
	}
	target, err := issuedURI(opened["url"], "the transfer URI")
	if err != nil {
		return err
	}
	upload, uploaded := opened["upload_id"].(string)
	if !uploaded {
		return fmt.Errorf("image import did not open")
	}
	if _, err = call("PATCH", target, "bytes 0-"+strconv.Itoa(len(source)-1)+"/"+size, source, 204); err != nil {
		return err
	}
	committed, err := call("POST", base+"/imports/"+upload+":commit", "", nil, 200)
	if err != nil {
		return err
	}
	item, ok := committed["item_id"].(string)
	if !ok {
		return fmt.Errorf("image commit returned no identity")
	}
	resolved, err := call("POST", base+"/items/"+item+":resolve", "", []byte(`{"representation":"thumbnail","maximum_pixel_size":4}`), 200)
	if err != nil {
		return err
	}
	location, ok := resolved["url"].(string)
	if !ok {
		return fmt.Errorf("thumbnail resolve returned no URL")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", location, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("thumbnail read failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("thumbnail read: expected HTTP 200, received %d", response.StatusCode)
	}
	rendition, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("thumbnail body unreadable")
	}
	if bytes.Equal(rendition, source) {
		return fmt.Errorf("the original was substituted for a thumbnail")
	}
	decoded, _, err := image.Decode(bytes.NewReader(rendition))
	if err != nil {
		return fmt.Errorf("the thumbnail is not a decodable image")
	}
	if decoded.Bounds().Dx() > 4 || decoded.Bounds().Dy() > 4 {
		return fmt.Errorf("the thumbnail exceeds the requested bound")
	}
	if decoded.Bounds().Dx() < 1 || decoded.Bounds().Dy() < 1 {
		return fmt.Errorf("the thumbnail is empty")
	}
	return nil
}

// issuedURI is every transfer and resolved URI a store hands back, checked at
// the one place they all pass through.
//
// The scheme matters more here than anywhere else in this protocol. The
// content protocol says of a resolved URI, in its own words, that it is a
// credential: for its lifetime whoever holds it can read those bytes. Issued
// over plaintext it is a credential every intermediary on the path holds until
// it expires — and the client that was handed it chose neither the origin nor
// the scheme, so nothing on the client side can see the difference.
//
// This is the half of that rule a suite can enforce. Refusing to fetch such a
// URI is a client obligation and this suite tests servers; never issuing one
// is a server's behaviour, and it is this.
func issuedURI(raw any, what string) (string, error) {
	target, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("missing %s", what)
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("%s must be absolute", what)
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return target, nil
	}
	if strings.EqualFold(parsed.Scheme, "http") && isLoopback(parsed.Hostname()) {
		return target, nil
	}
	return "", fmt.Errorf("%s is issued over %q to a non-loopback host, and it is a credential", what, parsed.Scheme)
}
