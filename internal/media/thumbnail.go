package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"log/slog"

	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/store"
)

// Thumbnail sizes are capped rather than honoured without limit. The cap is not about storage —
// a thumbnail is small — it is about work: generating one costs time on this server, and a request
// for a 40000×40000 thumbnail is a way to spend someone else's CPU. Anything beyond these dimensions
// is answered with the largest thumbnail that fits, which is what a client displaying a grid asked for.
const (
	maxThumbnailEdge = 1024
	// maxSourcePixels bounds the picture we are willing to decode. It is a decompression-guard:
	// a 20 MB PNG can decompose to gigabytes, and the request that follows would be the one paying.
	maxSourcePixels = 40_000_000
	// samplesPerAxis caps how many source samples go into one thumbnail pixel. It keeps a
	// downscale of a huge image from becoming quadratic in the source size.
	samplesPerAxis = 6
)

// Thumbnail returns a scaled copy of a file as PNG bytes, generating it if this server has not made
// that thumbnail before.
//
// The two methods differ in what they give up. `scale` keeps the whole image inside the requested box
// and preserves the aspect ratio, so the result may be smaller than asked in one dimension. `crop`
// fills the box exactly and gives up the edges, which is what a square grid of avatars wants.
//
// The output is always PNG: a thumbnail is a derived artifact, and there is no reason to preserve the
// source format when one of them is lossy, has patent history, or animates.
func (s *Service) Thumbnail(ctx context.Context, origin, mediaID string, width, height int,
	method string) (row *store.MediaFile, body []byte, err error) {
	if !s.thumbnailsEnabled {
		return nil, nil, matrix.NewUnrecognized("This homeserver does not generate thumbnails")
	}
	if width <= 0 || height <= 0 {
		return nil, nil, matrix.NewInvalidParam("width and height must be positive integers")
	}
	if width > maxThumbnailEdge || height > maxThumbnailEdge {
		return nil, nil, matrix.NewInvalidParam("a thumbnail may be at most %d by %d pixels",
			maxThumbnailEdge, maxThumbnailEdge)
	}
	if method == "" {
		method = "scale"
	}
	if method != "scale" && method != "crop" {
		return nil, nil, matrix.NewInvalidParam("method must be one of scale or crop, not %q", method)
	}

	source, file, err := s.Download(ctx, origin, mediaID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()

	if !isImage(source.ContentType) {
		return nil, nil, matrix.NewAPIError(400, matrix.ErrUnknown,
			"Cannot thumbnail a %s file", source.ContentType)
	}

	// The cached thumbnail sits beside the file it was made from and is named after its dimensions,
	// so a request for a different size makes a different file rather than overwriting this one, and
	// deleting the original takes its thumbnails with it.
	cachePath := s.thumbnailPath(source.ID, width, height, method)
	if cached, err := os.ReadFile(cachePath); err == nil {
		return thumbnailRow(source, len(cached)), cached, nil
	}

	decoded, err := s.decode(source, file)
	if err != nil {
		return nil, nil, err
	}
	scaled := fit(decoded, width, height, method)
	var out bytes.Buffer
	if err := png.Encode(&out, scaled); err != nil {
		return nil, nil, matrix.NewAPIError(500, matrix.ErrUnknown, "Could not encode the thumbnail")
	}
	body = out.Bytes()

	// A failed cache write is not a failed request: the thumbnail is already computed and the client
	// is waiting. It is logged, because the next request for the same thumbnail will pay for it again.
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		slog.Warn("media: could not cache a thumbnail", "media_id", loggableID(source.ID), "error", err)
	}
	return thumbnailRow(source, len(body)), body, nil
}

// decode reads the stored file as an image. GIF is decoded to its first frame, which is the frame a
// thumbnail should show anyway; animating a thumbnail would be a surprise to whoever pays for it.
func (s *Service) decode(source *store.MediaFile, file *os.File) (image.Image, error) {
	if _, err := file.Seek(0, 0); err != nil {
		return nil, matrix.NewAPIError(500, matrix.ErrUnknown, "Could not read the stored file")
	}
	// The decoders register themselves when their packages are imported; GIF comes back as its first
	// frame, which is the frame a thumbnail should show anyway.
	decoded, _, err := image.Decode(file)
	if err != nil {
		slog.Error("media: could not decode a file for thumbnailling",
			"media_id", loggableID(source.ID), "content_type", source.ContentType, "error", err)
		return nil, matrix.NewAPIError(400, matrix.ErrUnknown,
			"Cannot thumbnail this file: it could not be read as an image")
	}
	bounds := decoded.Bounds()
	if int64(bounds.Dx())*int64(bounds.Dy()) > maxSourcePixels {
		return nil, matrix.NewAPIError(400, matrix.ErrUnknown,
			"This image is too large to thumbnail (%d by %d pixels)", bounds.Dx(), bounds.Dy())
	}
	return decoded, nil
}

// fit scales an image into a width×height box.
func fit(source image.Image, width, height int, method string) image.Image {
	bounds := source.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	if sourceWidth == 0 || sourceHeight == 0 {
		return image.NewRGBA(image.Rect(0, 0, width, height))
	}
	if method == "crop" {
		// Crop: scale so the image covers the box entirely, then take the centred window. Covering
		// first and cropping second is what keeps a portrait photo from coming out letterboxed.
		scale := math.Max(float64(width)/float64(sourceWidth), float64(height)/float64(sourceHeight))
		wide := int(math.Ceil(float64(sourceWidth) * scale))
		tall := int(math.Ceil(float64(sourceHeight) * scale))
		scaled := sample(source, wide, tall)
		left := (wide - width) / 2
		top := (tall - height) / 2
		return subImage(scaled, left, top, width, height)
	}
	// Scale: fit inside the box, keeping the aspect ratio. Rounding down never exceeds the request.
	scale := math.Min(float64(width)/float64(sourceWidth), float64(height)/float64(sourceHeight))
	w := maxInt(1, int(math.Floor(float64(sourceWidth)*scale+0.5)))
	h := maxInt(1, int(math.Floor(float64(sourceHeight)*scale+0.5)))
	if scale >= 1 {
		// The requested box is bigger than the image: give the image back at its own size rather
		// than inventing detail and charging for it.
		w, h = sourceWidth, sourceHeight
		if w > width {
			w = width
		}
		if h > height {
			h = height
		}
	}
	return sample(source, w, h)
}

// sample resamples an image to dstW×dstH by averaging the region of the source each output pixel
// stands for, taking at most samplesPerAxis samples per axis.
func sample(source image.Image, dstW, dstH int) image.Image {
	bounds := source.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	out := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	xRatio := float64(srcW) / float64(dstW)
	yRatio := float64(srcH) / float64(dstH)
	stepX := maxF(1, float64(int(samplesPerAxis))/maxF(xRatio, 1e-9))
	stepY := maxF(1, float64(int(samplesPerAxis))/maxF(yRatio, 1e-9))
	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			x0 := float64(x) * xRatio
			x1 := math.Min(x0+xRatio, float64(srcW))
			y0 := float64(y) * yRatio
			y1 := math.Min(y0+yRatio, float64(srcH))
			var r, g, b, a float64
			count := 0.0
			for sy := y0; sy < y1; sy += stepY {
				for sx := x0; sx < x1; sx += stepX {
					c := source.At(bounds.Min.X+int(sx), bounds.Min.Y+int(sy))
					rr, gg, bb, aa := c.RGBA()
					// RGBA() returns 16-bit premultiplied values on a 0–65535 scale; normalising
					// once at the end keeps the accumulation in floats and the result in range.
					r += float64(rr)
					g += float64(gg)
					b += float64(bb)
					a += float64(aa)
					count++
				}
			}
			if count == 0 {
				count = 1
			}
			out.SetRGBA(x, y, color.RGBA{
				R: uint8(r / count / 257),
				G: uint8(g / count / 257),
				B: uint8(b / count / 257),
				A: uint8(a / count / 257),
			})
		}
	}
	return out
}

// subImage copies a window out of an image, clamped to its bounds.
func subImage(source image.Image, left, top, width, height int) image.Image {
	bounds := source.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			sourceX, sourceY := bounds.Min.X+left+x, bounds.Min.Y+top+y
			if sourceX >= bounds.Max.X || sourceY >= bounds.Max.Y {
				continue // stays transparent at the edge of an image that ran out
			}
			out.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	return out
}

// thumbnailRow describes a generated thumbnail with the metadata of the file it came from.
func thumbnailRow(source *store.MediaFile, size int) *store.MediaFile {
	thumbnail := *source
	thumbnail.ContentType = "image/png"
	thumbnail.Size = int64(size)
	return &thumbnail
}

// thumbnailPath names the cache file for one thumbnail. The media ID is already the name of the
// original on disk, so the derivatives hang off it and share its fate.
func (s *Service) thumbnailPath(mediaID string, width, height int, method string) string {
	name := filepath.Base(s.pathFor(mediaID))
	return filepath.Join(s.thumbnailDir, name+"-"+strconv.Itoa(width)+"x"+strconv.Itoa(height)+"-"+method+".png")
}

func isImage(contentType string) bool {
	if len(contentType) < 6 {
		return false
	}
	return contentType[:5] == "image"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
