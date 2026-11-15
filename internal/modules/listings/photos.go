package listings

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // PNG uploads
	"net/http"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // WebP uploads (decoder only: everything is stored as JPEG)

	"rightproperty365.com/backend/internal/platform/imaging"
)

// Listing photo rules. A photo is uploaded once and stored as three JPEG
// files: a large one whose longer side is at most PhotoLargeSide pixels and
// two narrower ones (PhotoMediumWidth and PhotoSmallWidth wide, never wider
// than the large one). Every stored file is encoded again from the decoded
// pixels, so no metadata of the upload (EXIF, GPS, colour profile, comments)
// survives; the EXIF orientation of a JPEG is applied first.
const (
	// MaxPhotosPerListing is the owner's rule: "max of 20 photos per property".
	MaxPhotosPerListing = 20
	// MinPhotosToSubmit is the number of photos a listing needs before it can
	// be submitted for review.
	MinPhotosToSubmit = 1
	// MaxPhotoBytes is the largest upload.
	MaxPhotoBytes = 15 << 20
	// MinPhotoLongSide is the smallest longer side an upload may have.
	MinPhotoLongSide = 800
	// MaxPhotoSide and MaxPhotoPixels bound what is decoded (checked on the
	// image header, before anything is decoded in full).
	MaxPhotoSide   = 10000
	MaxPhotoPixels = 40_000_000
	// Stored sizes.
	PhotoLargeSide   = 2048
	PhotoMediumWidth = 1280
	PhotoSmallWidth  = 640
	PhotoJPEGQuality = 82
	// PhotoKeyPrefix is the object-key prefix of the stored files.
	PhotoKeyPrefix = "listingphotos"
	// MaxPhotoAltLength bounds a photo's alternative text.
	MaxPhotoAltLength = 250
	maxPhotoBodyExtra = 64 << 10
)

// MaxPhotoBodyBytes is the request body limit of a photo upload: the image
// plus room for the multipart framing.
const MaxPhotoBodyBytes = MaxPhotoBytes + maxPhotoBodyExtra

// Stored sizes of a photo, as they appear in its URLs.
const (
	PhotoSizeLarge  = "large"
	PhotoSizeMedium = "medium"
	PhotoSizeSmall  = "small"
)

// PhotoSizes lists the stored sizes, largest first.
func PhotoSizes() []string { return []string{PhotoSizeLarge, PhotoSizeMedium, PhotoSizeSmall} }

// Accepted upload types, decided by the bytes alone.
const (
	photoTypeJPEG = "image/jpeg"
	photoTypePNG  = "image/png"
	photoTypeWebP = "image/webp"
)

// Photo upload errors.
var (
	ErrPhotoTooLarge    = errors.New("photo larger than 15 MB")
	ErrPhotoUnsupported = errors.New("a photo must be a JPEG, PNG or WebP image")
	// ErrPhotoDimensions is a photo whose longer side is under
	// MinPhotoLongSide pixels, or one too large to decode.
	ErrPhotoDimensions = errors.New("photo dimensions out of range")
)

// EncodedVariant is one stored size of a photo, ready to be written.
type EncodedVariant struct {
	Size   string
	Width  int
	Height int
	Data   []byte
}

// ProcessedPhoto is an upload turned into what is stored.
type ProcessedPhoto struct {
	// Checksum is the SHA-256 (hex) of the large file: the photo's version.
	Checksum string
	// Width and Height are the large file's.
	Width, Height int
	Variants      []EncodedVariant
}

// ProcessPhoto validates an upload and produces its stored files. The type
// is decided by sniffing the content and decoding the header (never a file
// name or a Content-Type header); the limits are checked on the header before
// anything is decoded in full. A JPEG is turned upright by its EXIF
// orientation, transparency is flattened onto white, and every size is
// encoded as a new JPEG, which drops all metadata.
func ProcessPhoto(content []byte) (ProcessedPhoto, error) {
	if len(content) > MaxPhotoBytes {
		return ProcessedPhoto{}, ErrPhotoTooLarge
	}
	contentType := http.DetectContentType(content)
	want := map[string]string{photoTypeJPEG: "jpeg", photoTypePNG: "png", photoTypeWebP: "webp"}[contentType]
	if want == "" {
		return ProcessedPhoto{}, ErrPhotoUnsupported
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || format != want {
		return ProcessedPhoto{}, ErrPhotoUnsupported
	}
	if format == "png" && imaging.AnimatedPNG(content) {
		return ProcessedPhoto{}, ErrPhotoUnsupported
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxPhotoSide || cfg.Height > MaxPhotoSide || cfg.Width*cfg.Height > MaxPhotoPixels {
		return ProcessedPhoto{}, ErrPhotoDimensions
	}
	if max(cfg.Width, cfg.Height) < MinPhotoLongSide {
		return ProcessedPhoto{}, ErrPhotoDimensions
	}
	src, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return ProcessedPhoto{}, ErrPhotoUnsupported
	}
	orientation := 1
	if format == "jpeg" {
		orientation = imaging.JPEGOrientation(content)
	}

	// The large size, from the source: the shown (upright) longer side to at
	// most PhotoLargeSide, never enlarged.
	shownW, shownH := cfg.Width, cfg.Height
	if imaging.Rotated(orientation) {
		shownW, shownH = shownH, shownW
	}
	largeW, largeH := fit(shownW, shownH, PhotoLargeSide, PhotoLargeSide)
	storedW, storedH := largeW, largeH // before turning upright
	if imaging.Rotated(orientation) {
		storedW, storedH = largeH, largeW
	}
	large := imaging.Upright(scale(src, storedW, storedH), orientation)

	out := ProcessedPhoto{Width: large.Bounds().Dx(), Height: large.Bounds().Dy()}
	encode := func(size string, img *image.RGBA) error {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: PhotoJPEGQuality}); err != nil {
			return fmt.Errorf("encode photo: %w", err)
		}
		out.Variants = append(out.Variants, EncodedVariant{Size: size, Width: img.Bounds().Dx(), Height: img.Bounds().Dy(), Data: buf.Bytes()})
		return nil
	}
	if err := encode(PhotoSizeLarge, large); err != nil {
		return ProcessedPhoto{}, err
	}
	sum := sha256.Sum256(out.Variants[0].Data)
	out.Checksum = hex.EncodeToString(sum[:])
	// The narrower sizes, from the large one (already upright).
	for _, v := range []struct {
		size  string
		width int
	}{{PhotoSizeMedium, PhotoMediumWidth}, {PhotoSizeSmall, PhotoSmallWidth}} {
		img := large
		if large.Bounds().Dx() > v.width {
			w, h := fit(large.Bounds().Dx(), large.Bounds().Dy(), v.width, 1<<30)
			img = scale(large, w, h)
		}
		if err := encode(v.size, img); err != nil {
			return ProcessedPhoto{}, err
		}
	}
	return out, nil
}

// fit returns w x h scaled down (same proportions, to the nearest pixel) to
// fit maxW x maxH; never enlarged, never under one pixel.
func fit(w, h, maxW, maxH int) (int, int) {
	if w <= maxW && h <= maxH {
		return w, h
	}
	// The scale is num/den, the smaller of the two ratios.
	num, den := maxW, w
	if maxH*w < maxW*h {
		num, den = maxH, h
	}
	return max(1, (w*num+den/2)/den), max(1, (h*num+den/2)/den)
}

// scale draws src into a new w x h RGBA image over white (Catmull-Rom), so a
// transparent PNG becomes a photo on a white background.
func scale(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	if src.Bounds().Dx() == w && src.Bounds().Dy() == h {
		draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Over)
		return dst
	}
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}
