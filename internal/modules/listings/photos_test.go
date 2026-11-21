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

// API 0.60.0, FR-010 "metadata strip": a JPEG carrying EXIF with a GPS
// position comes out of ProcessPhoto without any EXIF (or other APPn)
// segment: every stored variant is re-encoded from pixels only.
func TestProcessPhotoStripsEXIFAndGPS(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 800, 600)), nil); err != nil {
		t.Fatal(err)
	}
	plain := buf.Bytes()
	// An APP1 Exif segment with a GPS IFD marker and a recognisable position.
	payload := append([]byte("Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08"), []byte("GPSLatitude=17.8292S;GPSLongitude=31.0522E;Model=TestCam")...)
	seg := []byte{0xff, 0xe1, byte((len(payload) + 2) >> 8), byte((len(payload) + 2) & 0xff)}
	withExif := append(append(append([]byte{}, plain[:2]...), append(seg, payload...)...), plain[2:]...)
	if !bytes.Contains(withExif, []byte("GPSLatitude")) {
		t.Fatal("fixture")
	}
	out, err := ProcessPhoto(withExif)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range out.Variants {
		for _, needle := range [][]byte{[]byte("Exif\x00\x00"), []byte("GPSLatitude"), []byte("17.8292"), []byte("TestCam")} {
			if bytes.Contains(v.Data, needle) {
				t.Fatalf("%s variant still holds %q", v.Size, needle)
			}
		}
		for at := 2; at+4 <= len(v.Data) && v.Data[at] == 0xff && v.Data[at+1] != 0xda; {
			if m := v.Data[at+1]; m >= 0xe1 && m <= 0xef {
				t.Fatalf("%s variant has an APP%d segment", v.Size, m-0xe0)
			}
			at += 2 + int(v.Data[at+2])<<8 + int(v.Data[at+3])
		}
	}
}
