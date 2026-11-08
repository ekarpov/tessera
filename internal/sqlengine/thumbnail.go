package sqlengine

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"strconv"
	"strings"

	"github.com/ekarpov/tessera/pkg/store"
)

// Thumbnails are produced on demand and never stored. A cached rendition would
// be a derivative that item deletion, store erasure, retirement and backup
// restoration all have to reach, and the content protocol is explicit that
// derivatives are among the things an erase must not miss. Recomputing a small
// image costs less than owning that obligation.

// A small requested thumbnail must not be usable to make the server decompress
// an enormous image, so both the bytes read and the pixels decoded are bounded.
const maxThumbnailBytes = 64 << 20
const maxThumbnailPixels = 50 << 20
const maxThumbnailSize = 4096

// thumbnailable reports whether a stored content type is one this server can
// decode. It is deliberately a short list: a format it cannot decode is an item
// it cannot produce a thumbnail for, which is 415, not a missing capability.
func thumbnailable(contentType string) bool {
	media, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(media)) {
	case "image/jpeg", "image/png", "image/gif":
		return true
	}
	return false
}

// thumbnail decodes source and returns a rendition whose longer side is at most
// size pixels, together with its content type. It always re-encodes: handing
// back the original bytes when an image is already small would make two
// different resources indistinguishable on the wire.
func thumbnail(source io.Reader, size int) ([]byte, string, error) {
	if size < 1 || size > maxThumbnailSize {
		return nil, "", store.Refuse("invalid_request")
	}
	bounded := io.LimitReader(source, maxThumbnailBytes)
	header := bytes.NewBuffer(nil)
	config, format, err := image.DecodeConfig(io.TeeReader(bounded, header))
	if err != nil {
		return nil, "", store.Refuse("unsupported_representation")
	}
	if config.Width < 1 || config.Height < 1 || config.Width*config.Height > maxThumbnailPixels {
		return nil, "", store.Refuse("unsupported_representation")
	}
	decoded, _, err := image.Decode(io.MultiReader(header, bounded))
	if err != nil {
		return nil, "", store.Refuse("unsupported_representation")
	}
	scaled := downscale(decoded, size)
	out := bytes.NewBuffer(nil)
	// Keep a photograph a photograph; everything else becomes PNG, because an
	// animated or paletted source has no faithful single-frame equivalent in
	// its own format.
	if format == "jpeg" {
		err = jpeg.Encode(out, scaled, &jpeg.Options{Quality: 82})
		return out.Bytes(), "image/jpeg", err
	}
	err = png.Encode(out, scaled)
	return out.Bytes(), "image/png", err
}

// downscale area-averages the source into the destination. A box filter is
// enough for a thumbnail and avoids a dependency on an image-processing
// library, which matters in a repository people are expected to fork.
func downscale(source image.Image, size int) image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	target := width
	if height > target {
		target = height
	}
	scaledWidth, scaledHeight := width, height
	if target > size {
		scaledWidth = width * size / target
		scaledHeight = height * size / target
	}
	if scaledWidth < 1 {
		scaledWidth = 1
	}
	if scaledHeight < 1 {
		scaledHeight = 1
	}
	destination := image.NewRGBA(image.Rect(0, 0, scaledWidth, scaledHeight))
	if scaledWidth == width && scaledHeight == height {
		draw.Draw(destination, destination.Bounds(), source, bounds.Min, draw.Src)
		return destination
	}
	for y := 0; y < scaledHeight; y++ {
		top := bounds.Min.Y + y*height/scaledHeight
		bottom := bounds.Min.Y + (y+1)*height/scaledHeight
		if bottom <= top {
			bottom = top + 1
		}
		for x := 0; x < scaledWidth; x++ {
			left := bounds.Min.X + x*width/scaledWidth
			right := bounds.Min.X + (x+1)*width/scaledWidth
			if right <= left {
				right = left + 1
			}
			var r, g, b, a, n uint64
			for sy := top; sy < bottom; sy++ {
				for sx := left; sx < right; sx++ {
					pr, pg, pb, pa := source.At(sx, sy).RGBA()
					r, g, b, a, n = r+uint64(pr), g+uint64(pg), b+uint64(pb), a+uint64(pa), n+1
				}
			}
			if n == 0 {
				continue
			}
			destination.Set(x, y, color.RGBA64{R: uint16(r / n), G: uint16(g / n), B: uint16(b / n), A: uint16(a / n)})
		}
	}
	return destination
}

// thumbnailRepresentation encodes a requested rendition on a transfer token, so
// the read side knows what to produce without a second authorization decision.
func thumbnailRepresentation(size int) string { return "thumbnail:" + strconv.Itoa(size) }

func parseThumbnailRepresentation(value string) (int, bool) {
	rest, found := strings.CutPrefix(value, "thumbnail:")
	if !found {
		return 0, false
	}
	size, err := strconv.Atoi(rest)
	if err != nil || size < 1 || size > maxThumbnailSize {
		return 0, false
	}
	return size, true
}
