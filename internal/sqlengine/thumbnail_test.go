package sqlengine

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	p "github.com/ekarpov/tessera/pkg/protocol"
)

// sample encodes a gradient so that averaging is visible: a thumbnail of a
// solid colour would pass even if the filter dropped every pixel but one.
func sample(t *testing.T, width, height int, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / width), G: uint8(y * 255 / height), B: 40, A: 255})
		}
	}
	out := bytes.NewBuffer(nil)
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(out, img, nil)
	} else {
		err = png.Encode(out, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestThumbnailBoundsAndFormat(t *testing.T) {
	for _, tc := range []struct {
		format, contentType string
		width, height, size int
		wantW, wantH        int
	}{
		{"jpeg", "image/jpeg", 800, 400, 100, 100, 50},
		{"png", "image/png", 400, 800, 100, 50, 100},
		{"png", "image/png", 40, 20, 100, 40, 20}, // Already smaller: not upscaled.
		{"jpeg", "image/jpeg", 300, 300, 1, 1, 1},
	} {
		rendition, contentType, err := thumbnail(bytes.NewReader(sample(t, tc.width, tc.height, tc.format)), tc.size)
		if err != nil {
			t.Fatal(err)
		}
		if contentType != tc.contentType {
			t.Fatal("thumbnail changed format", contentType, tc.contentType)
		}
		decoded, _, err := image.Decode(bytes.NewReader(rendition))
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Bounds().Dx() != tc.wantW || decoded.Bounds().Dy() != tc.wantH {
			t.Fatal("wrong thumbnail bounds", decoded.Bounds(), tc.wantW, tc.wantH)
		}
		// Where a downscale actually happens the bytes must differ. An image
		// already within bounds may re-encode identically, which is the right
		// rendition rather than a substituted original.
		if tc.wantW != tc.width && bytes.Equal(rendition, sample(t, tc.width, tc.height, tc.format)) {
			t.Fatal("a downscaled thumbnail returned the original bytes")
		}
	}
	// A gradient averages to something in the middle, which a filter that
	// simply sampled one corner would not produce.
	rendition, _, err := thumbnail(bytes.NewReader(sample(t, 256, 256, "png")), 1)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(rendition))
	if err != nil {
		t.Fatal(err)
	}
	r, g, _, _ := decoded.At(0, 0).RGBA()
	if r>>8 < 100 || r>>8 > 155 || g>>8 < 100 || g>>8 > 155 {
		t.Fatal("a one-pixel thumbnail did not average the image", r>>8, g>>8)
	}
	// What cannot be decoded is not producible, and the bound is enforced.
	if _, _, err = thumbnail(strings.NewReader("not an image at all"), 64); p.ErrorCode(err) != "unsupported_representation" {
		t.Fatal("undecodable bytes were not refused", err)
	}
	if _, _, err = thumbnail(bytes.NewReader(sample(t, 32, 32, "png")), maxThumbnailSize+1); p.ErrorCode(err) != "invalid_request" {
		t.Fatal("an out-of-range size was accepted", err)
	}
}

// End to end: resolve authorizes a thumbnail, and reading the resolved URI
// produces a rendition without the original ever being transferred.
func TestThumbnailResolvesWithoutTransferringTheOriginal(t *testing.T) {
	s, contentID, _ := contentFixture(t)
	original := sample(t, 600, 300, "png")
	request := json.RawMessage(`{"operation_id":"thumb","content_type":"image/png","byte_count":` +
		strconv.Itoa(len(original)) + `,"maximum_bytes":` + strconv.Itoa(len(original)) + `}`)
	value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", request)
	opened := contentMap(t, value, err)
	token := strings.TrimPrefix(opened["url"].(string), "/v1/transfers/")
	if _, err = s.ContentTransfer(t.Context(), token, "PATCH", "bytes 0-"+strconv.Itoa(len(original)-1)+"/"+strconv.Itoa(len(original)), bytes.NewReader(original)); err != nil {
		t.Fatal(err)
	}
	value, err = s.ContentControl(t.Context(), owner, "commit", contentID, opened["upload_id"].(string), nil)
	item := contentMap(t, value, err)["item_id"].(string)

	value, err = s.ContentControl(t.Context(), owner, "resolve", contentID, item, json.RawMessage(`{"representation":"thumbnail","maximum_pixel_size":60}`))
	resolved := contentMap(t, value, err)
	read, err := s.ContentTransfer(t.Context(), strings.TrimPrefix(resolved["url"].(string), "/v1/transfers/"), "GET", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Body.Close()
	rendition, err := io.ReadAll(read.Body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(rendition)) != read.Size {
		t.Fatal("declared size disagreed with the bytes served")
	}
	if bytes.Equal(rendition, original) {
		t.Fatal("the original was served as a thumbnail")
	}
	decoded, _, err := image.Decode(bytes.NewReader(rendition))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != 60 || decoded.Bounds().Dy() != 30 {
		t.Fatal("wrong rendition bounds", decoded.Bounds())
	}
	// The original remains resolvable and unchanged, so producing a rendition
	// did not replace the resource it came from.
	value, err = s.ContentControl(t.Context(), owner, "resolve", contentID, item, json.RawMessage(`{"representation":"original"}`))
	full, err := s.ContentTransfer(t.Context(), strings.TrimPrefix(contentMap(t, value, err)["url"].(string), "/v1/transfers/"), "GET", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Body.Close()
	served, err := io.ReadAll(full.Body)
	if err != nil || !bytes.Equal(served, original) {
		t.Fatal("the original changed", err)
	}
}

// An item this server cannot decode is the 415 case: the capability is
// advertised, but not producible for this item.
func TestThumbnailOfANonImageIsUnsupportedNotUnadvertised(t *testing.T) {
	s, contentID, _ := contentFixture(t)
	request := json.RawMessage(`{"operation_id":"text","content_type":"text/plain","byte_count":3,"maximum_bytes":3}`)
	value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", request)
	opened := contentMap(t, value, err)
	if _, err = s.ContentTransfer(t.Context(), strings.TrimPrefix(opened["url"].(string), "/v1/transfers/"), "PATCH", "bytes 0-2/3", bytes.NewReader([]byte("abc"))); err != nil {
		t.Fatal(err)
	}
	value, err = s.ContentControl(t.Context(), owner, "commit", contentID, opened["upload_id"].(string), nil)
	item := contentMap(t, value, err)["item_id"].(string)
	_, err = s.ContentControl(t.Context(), owner, "resolve", contentID, item, json.RawMessage(`{"representation":"thumbnail","maximum_pixel_size":64}`))
	if p.ErrorCode(err) != "unsupported_representation" {
		t.Fatal("a non-image thumbnail was not 415", err)
	}
	// current is a capability this store does not have at all, so it is 403.
	_, err = s.ContentControl(t.Context(), owner, "resolve", contentID, item, json.RawMessage(`{"representation":"current"}`))
	if p.ErrorCode(err) != "not_supported" {
		t.Fatal("an unadvertised representation was not 403", err)
	}
}

// Drawing a thumbnail must not hold every other transfer on the server. The
// test takes every rendition slot, so a thumbnail request waits for one — as
// it would behind a large image — and a download of the original must finish
// meanwhile. The waiting request gives up at its own deadline.
func TestAThumbnailDoesNotHoldOtherTransfers(t *testing.T) {
	s, contentID, _ := contentFixture(t)
	original := sample(t, 64, 32, "png")
	request := json.RawMessage(`{"operation_id":"thumb-wait","content_type":"image/png","byte_count":` +
		strconv.Itoa(len(original)) + `,"maximum_bytes":` + strconv.Itoa(len(original)) + `}`)
	value, err := s.ContentControl(t.Context(), owner, "begin", contentID, "", request)
	opened := contentMap(t, value, err)
	token := strings.TrimPrefix(opened["url"].(string), "/v1/transfers/")
	if _, err = s.ContentTransfer(t.Context(), token, "PATCH", "bytes 0-"+strconv.Itoa(len(original)-1)+"/"+strconv.Itoa(len(original)), bytes.NewReader(original)); err != nil {
		t.Fatal(err)
	}
	value, err = s.ContentControl(t.Context(), owner, "commit", contentID, opened["upload_id"].(string), nil)
	item := contentMap(t, value, err)["item_id"].(string)
	resolve := func(representation string) string {
		v, e := s.ContentControl(t.Context(), owner, "resolve", contentID, item, json.RawMessage(representation))
		return strings.TrimPrefix(contentMap(t, v, e)["url"].(string), "/v1/transfers/")
	}
	thumb, full := resolve(`{"representation":"thumbnail","maximum_pixel_size":8}`), resolve(`{"representation":"original"}`)

	for i := 0; i < cap(s.renditions); i++ {
		s.renditions <- struct{}{}
	}
	deadline, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	waited := make(chan error, 1)
	go func() {
		_, e := s.ContentTransfer(deadline, thumb, "GET", "", nil)
		waited <- e
	}()
	time.Sleep(50 * time.Millisecond)
	served := make(chan error, 1)
	go func() {
		read, e := s.ContentTransfer(t.Context(), full, "GET", "", nil)
		if e == nil {
			read.Body.Close()
		}
		served <- e
	}()
	select {
	case e := <-served:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("a download waited behind a thumbnail")
	}
	if e := <-waited; e == nil {
		t.Fatal("a thumbnail with no slot before its deadline was drawn anyway")
	}
	for i := 0; i < cap(s.renditions); i++ {
		<-s.renditions
	}
}
