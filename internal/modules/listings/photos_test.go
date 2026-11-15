package listings

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProcessPhoto(t *testing.T) {
	// A transparent PNG becomes three JPEGs on white, never enlarged.
	img := image.NewNRGBA(image.Rect(0, 0, 1000, 800))
	out, err := ProcessPhoto(encodePNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if out.Width != 1000 || out.Height != 800 || len(out.Variants) != 3 || len(out.Checksum) != 64 {
		t.Fatalf("processed: %dx%d, %d variants", out.Width, out.Height, len(out.Variants))
	}
	sizes := map[string][2]int{PhotoSizeLarge: {1000, 800}, PhotoSizeMedium: {1000, 800}, PhotoSizeSmall: {640, 512}}
	for _, v := range out.Variants {
		decoded, format, err := image.Decode(bytes.NewReader(v.Data))
		if err != nil || format != "jpeg" {
			t.Fatalf("%s: %s %v", v.Size, format, err)
		}
		if want := sizes[v.Size]; v.Width != want[0] || v.Height != want[1] || decoded.Bounds().Dx() != want[0] {
			t.Fatalf("%s: %dx%d", v.Size, v.Width, v.Height)
		}
		if r, g, b, _ := decoded.At(10, 10).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
			t.Fatalf("%s: transparency was not flattened onto white", v.Size)
		}
	}

	// Limits.
	small := image.NewRGBA(image.Rect(0, 0, 799, 600))
	if _, err := ProcessPhoto(encodePNG(t, small)); !errors.Is(err, ErrPhotoDimensions) {
		t.Fatalf("799 px: %v", err)
	}
	if _, err := ProcessPhoto([]byte("GIF89a not really")); !errors.Is(err, ErrPhotoUnsupported) {
		t.Fatalf("gif: %v", err)
	}
	if _, err := ProcessPhoto(make([]byte, MaxPhotoBytes+1)); !errors.Is(err, ErrPhotoTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	// A tall photo keeps its proportions.
	tall := image.NewRGBA(image.Rect(0, 0, 1500, 3000))
	for y := 0; y < 3000; y += 50 {
		tall.Set(10, y, color.Black)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, tall, nil); err != nil {
		t.Fatal(err)
	}
	out, err = ProcessPhoto(buf.Bytes())
	if err != nil || out.Width != 1024 || out.Height != 2048 {
		t.Fatalf("tall: %dx%d %v", out.Width, out.Height, err)
	}
}

func TestSlugFor(t *testing.T) {
	for _, tc := range []struct{ title, ref, want string }{
		{"Three-bedroom House, Borrowdale!", "RP-ZW-482913", "three-bedroom-house-borrowdale-zw-482913"},
		{"", "RP-GB-100200", "listing-gb-100200"},
		{"Café über alles", "RP-MU-1", "caf-ber-alles-mu-1"},
	} {
		if got := slugFor(tc.title, tc.ref); got != tc.want || !slugRe.MatchString(got) {
			t.Fatalf("slugFor(%q) = %q, want %q", tc.title, got, tc.want)
		}
	}
}
