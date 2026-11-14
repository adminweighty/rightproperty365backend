package directory

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/platform/storage"
)

// transparentPNG is a w x h PNG: a fully transparent background with an
// opaque red rectangle in its middle half.
func transparentPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := h / 4; y < h*3/4; y++ {
		for x := w / 4; x < w*3/4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 0xd0, G: 0x10, B: 0x10, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// halvesJPEG is a w x h JPEG whose left half is red and right half is blue.
func halvesJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{R: 0xe0, A: 0xff}
			if x >= w/2 {
				c = color.RGBA{B: 0xe0, A: 0xff}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withOrientation inserts an EXIF segment that says only the orientation,
// right after the start-of-image marker of a JPEG.
func withOrientation(content []byte, orientation uint16) []byte {
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, // big endian, IFD at 8, one entry
		0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 0, 0, 0, // Orientation, SHORT, count 1, value
		0, 0, 0, 0} // no next IFD
	binary.BigEndian.PutUint16(tiff[18:20], orientation)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(payload)+2))
	out := append([]byte{}, content[:2]...)
	out = append(out, segment...)
	out = append(out, payload...)
	return append(out, content[2:]...)
}

func checksum(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func newFiles(t *testing.T) *storage.Local {
	t.Helper()
	files, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// stored stores content as a logo and returns the Logo with the bytes that
// were stored. What is recorded must describe those bytes.
func stored(t *testing.T, content []byte) (Logo, []byte) {
	t.Helper()
	files := newFiles(t)
	logo, err := StoreLogo(t.Context(), files, content, time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("StoreLogo: %v", err)
	}
	rc, err := files.Open(t.Context(), logo.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("the stored logo is not an image: %v", err)
	}
	if logo.Size != int64(len(got)) || logo.Checksum != checksum(got) || logo.Width != cfg.Width || logo.Height != cfg.Height ||
		logo.ContentType != "image/"+format {
		t.Fatalf("recorded %+v, but the stored file is %d bytes, %dx%d %s, checksum %s", logo, len(got), cfg.Width, cfg.Height, format, checksum(got))
	}
	// The stored image is itself an acceptable logo, and is not shrunk again.
	if again, same, err := PrepareLogo(got); err != nil || !bytes.Equal(same, got) || again.Checksum != logo.Checksum {
		t.Fatalf("the stored logo is not stable: %+v %v", again, err)
	}
	return logo, got
}

func TestLargePNGIsScaledAndKeepsTransparency(t *testing.T) {
	upload := transparentPNG(t, 2000, 1000)
	logo, got := stored(t, upload)
	if logo.Width != 512 || logo.Height != 256 || logo.ContentType != "image/png" {
		t.Fatalf("logo = %+v, want a 512x256 PNG", logo)
	}
	if bytes.Equal(got, upload) || logo.Checksum == checksum(upload) {
		t.Fatal("the upload was stored as it was")
	}
	img, err := png.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, a := img.At(4, 4).RGBA(); a != 0 {
		t.Fatalf("the corner has alpha %#x, want fully transparent", a)
	}
	if c := color.NRGBAModel.Convert(img.At(256, 128)).(color.NRGBA); c.A != 0xff || c.R < 0xc0 || c.G > 0x20 || c.B > 0x20 {
		t.Fatalf("the middle is %+v, want opaque red", c)
	}
}

func TestLargeJPEGIsScaled(t *testing.T) {
	upload := halvesJPEG(t, 900, 1800)
	logo, got := stored(t, upload)
	if logo.Width != 256 || logo.Height != 512 || logo.ContentType != "image/jpeg" {
		t.Fatalf("logo = %+v, want a 256x512 JPEG", logo)
	}
	if len(got) >= len(upload) {
		t.Fatalf("the stored JPEG has %d bytes, the upload %d", len(got), len(upload))
	}
	img, err := jpeg.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if r, _, b, _ := img.At(40, 256).RGBA(); r>>8 < 0xa0 || b>>8 > 0x40 {
		t.Fatalf("the left half is not red: r=%#x b=%#x", r>>8, b>>8)
	}
	if r, _, b, _ := img.At(215, 256).RGBA(); b>>8 < 0xa0 || r>>8 > 0x40 {
		t.Fatalf("the right half is not blue: r=%#x b=%#x", r>>8, b>>8)
	}
}

// A photo taken with the phone held sideways says so in its EXIF data, which
// encoding again drops: the stored image is turned upright instead.
func TestScaledJPEGIsTurnedUpright(t *testing.T) {
	// Orientation 6: the stored rows are the picture's columns; shown, the
	// (red) left of the file is at the top.
	upload := withOrientation(halvesJPEG(t, 1600, 800), 6)
	if jpegOrientation(upload) != 6 || jpegOrientation(halvesJPEG(t, 200, 200)) != 1 {
		t.Fatal("the orientation is not read")
	}
	logo, got := stored(t, upload)
	if logo.Width != 256 || logo.Height != 512 {
		t.Fatalf("logo = %+v, want 256x512", logo)
	}
	img, err := jpeg.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if r, _, b, _ := img.At(128, 40).RGBA(); r>>8 < 0xa0 || b>>8 > 0x40 {
		t.Fatalf("the top is not red: r=%#x b=%#x", r>>8, b>>8)
	}
	if r, _, b, _ := img.At(128, 470).RGBA(); b>>8 < 0xa0 || r>>8 > 0x40 {
		t.Fatalf("the bottom is not blue: r=%#x b=%#x", r>>8, b>>8)
	}
	if jpegOrientation(got) != 1 {
		t.Fatal("the stored image still carries an orientation")
	}
}

func TestSmallLogoIsStoredUnchanged(t *testing.T) {
	for name, upload := range map[string][]byte{
		"256 png":             transparentPNG(t, 256, 256),
		"512 png (the limit)": transparentPNG(t, 512, 512),
		"512x128 jpeg":        halvesJPEG(t, 512, 128),
		"jpeg with exif":      withOrientation(halvesJPEG(t, 300, 200), 6),
	} {
		logo, got := stored(t, upload)
		if !bytes.Equal(got, upload) || logo.Checksum != checksum(upload) || logo.Size != int64(len(upload)) {
			t.Fatalf("%s: stored %d bytes, uploaded %d: not the same file", name, len(got), len(upload))
		}
	}
	if logo, _ := stored(t, transparentPNG(t, 256, 256)); logo.Width != 256 || logo.Height != 256 {
		t.Fatalf("logo = %+v", logo)
	}
	// As thin as a logo may be: scaling it would take it under 128 pixels.
	thin := transparentPNG(t, 4096, 128)
	if logo, got := stored(t, thin); !bytes.Equal(got, thin) || logo.Width != 4096 || logo.Height != 128 {
		t.Fatalf("a 4096x128 logo was stored as %+v", logo)
	}
}

func TestScaledLogoKeepsItsProportions(t *testing.T) {
	for _, c := range []struct{ w, h, wantW, wantH int }{
		{513, 513, 512, 512},
		{1000, 750, 512, 384},
		{2048, 512, 512, 128},
		// No side goes under the 128 pixels a logo must have.
		{1200, 200, 768, 128},
		{200, 1200, 128, 768},
		{4096, 129, 4064, 128},
		{600, 601, 511, 512},
	} {
		img, content, err := PrepareLogo(transparentPNG(t, c.w, c.h))
		if err != nil {
			t.Fatalf("%dx%d: %v", c.w, c.h, err)
		}
		if img.Width != c.wantW || img.Height != c.wantH {
			t.Fatalf("%dx%d became %dx%d, want %dx%d", c.w, c.h, img.Width, img.Height, c.wantW, c.wantH)
		}
		// The bytes are that size, and are themselves an acceptable logo.
		if again, err := CheckLogo(content); err != nil || again.Width != c.wantW || again.Height != c.wantH || again.Checksum != img.Checksum {
			t.Fatalf("%dx%d: the bytes are %+v (%v)", c.w, c.h, again, err)
		}
	}
}

// What may be uploaded is decided before anything is decoded in full, and is
// not changed by the scaling.
func TestPrepareLogoRefusesWhatCheckLogoRefuses(t *testing.T) {
	huge := transparentPNG(t, MaxLogoPixels+1, 200)
	if _, _, err := PrepareLogo(huge); !errors.Is(err, ErrLogoDimensions) {
		t.Fatalf("4097 pixels wide: %v", err)
	}
	if _, _, err := PrepareLogo(transparentPNG(t, 100, 100)); !errors.Is(err, ErrLogoDimensions) {
		t.Fatalf("100x100: %v", err)
	}
	if _, _, err := PrepareLogo(make([]byte, MaxLogoBytes+1)); !errors.Is(err, ErrLogoTooLarge) {
		t.Fatalf("more than 2 MB: %v", err)
	}
	if _, _, err := PrepareLogo([]byte("GIF89a not an image")); !errors.Is(err, ErrLogoUnsupported) {
		t.Fatalf("a GIF: %v", err)
	}
	// A valid header on data that cannot be decoded: refused, nothing stored.
	broken := transparentPNG(t, 1024, 1024)
	broken = broken[:len(broken)/2]
	if _, err := CheckLogo(broken); err != nil {
		t.Fatalf("the header of the broken image is not accepted: %v", err)
	}
	if _, _, err := PrepareLogo(broken); !errors.Is(err, ErrLogoUnsupported) {
		t.Fatalf("a truncated large PNG: %v", err)
	}
}
