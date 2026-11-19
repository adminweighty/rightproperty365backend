package maintenance

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"slices"
	"strings"
	"testing"
)

// The state machine, pair by pair: POST .../status moves exactly these;
// ASSIGNED only through an assignment, from NEW, TRIAGED or ASSIGNED.
func TestStateMachine(t *testing.T) {
	allowed := map[string][]string{
		StatusNew:        {StatusTriaged, StatusCancelled},
		StatusTriaged:    {StatusInProgress, StatusCancelled},
		StatusAssigned:   {StatusInProgress, StatusCancelled},
		StatusInProgress: {StatusResolved, StatusCancelled},
		StatusResolved:   {StatusClosed, StatusInProgress},
		StatusClosed:     {},
		StatusCancelled:  {},
	}
	for _, from := range Statuses() {
		for _, to := range Statuses() {
			if got, want := CanMove(from, to), slices.Contains(allowed[from], to); got != want {
				t.Errorf("CanMove(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
		if CanMove(from, StatusAssigned) {
			t.Errorf("%s -> ASSIGNED through the status route", from)
		}
		wantAssign := from == StatusNew || from == StatusTriaged || from == StatusAssigned
		if Assignable(from) != wantAssign {
			t.Errorf("Assignable(%s) = %v", from, Assignable(from))
		}
		if Open(from) == (from == StatusClosed || from == StatusCancelled) {
			t.Errorf("Open(%s) = %v", from, Open(from))
		}
		if StatusLabel(from) == from {
			t.Errorf("no label for %s", from)
		}
	}
	// Transitions returns a copy.
	list := Transitions(StatusNew)
	list[0] = "X"
	if Transitions(StatusNew)[0] != StatusTriaged {
		t.Fatal("Transitions leaks its table")
	}
}

func encodedJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// withEXIF puts an APP1 Exif segment (with a GPS marker) after the SOI.
func withEXIF(content []byte) []byte {
	payload := append([]byte("Exif\x00\x00"), []byte("GPSLatitude -17.8292 GPSLongitude 31.0522")...)
	seg := []byte{0xff, 0xe1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	out := append([]byte{}, content[:2]...)
	out = append(out, seg...)
	out = append(out, payload...)
	return append(out, content[2:]...)
}

// Photos: images only, decided by the bytes; re-encoded without metadata
// and brought down to StoredPhotoSide.
func TestProcessPhoto(t *testing.T) {
	for name, content := range map[string][]byte{
		"pdf":   []byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"),
		"gif":   []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"),
		"svg":   []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
		"text":  []byte("hello"),
		"empty": {},
		"fake":  append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte("truncated")...),
	} {
		if _, err := ProcessPhoto(content); !errors.Is(err, ErrPhotoUnsupported) {
			t.Errorf("%s: err = %v, want ErrPhotoUnsupported", name, err)
		}
	}
	if _, err := ProcessPhoto(make([]byte, MaxPhotoBytes+1)); !errors.Is(err, ErrPhotoTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	src := withEXIF(encodedJPEG(t, 3000, 1500))
	if !strings.Contains(string(src), "GPSLatitude") {
		t.Fatal("test setup: no EXIF")
	}
	got, err := ProcessPhoto(src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got.Data), "Exif") || strings.Contains(string(got.Data), "GPS") {
		t.Fatal("the stored photo keeps its metadata")
	}
	if got.Width != StoredPhotoSide || got.Height != StoredPhotoSide/2 || len(got.SHA256) != 64 {
		t.Fatalf("stored %dx%d %s", got.Width, got.Height, got.SHA256)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 20, 10))); err != nil {
		t.Fatal(err)
	}
	small, err := ProcessPhoto(b.Bytes())
	if err != nil || small.Width != 20 || small.Height != 10 {
		t.Fatalf("png: %+v %v", small, err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(small.Data)); err != nil {
		t.Fatalf("not stored as JPEG: %v", err)
	}
}

// The enquiry message quotes the reference and the manager's words only.
func TestEnquiryMessage(t *testing.T) {
	r := Request{Reference: "RPM-2026-000007", Title: "Leak at Tariro's", Description: "Call +263771234567"}
	msg := EnquiryMessage(r, "Avondale Court", "Flat 1", "ZW", "Plumbing", "High", "Ring the office.")
	want := "Maintenance request RPM-2026-000007\nCategory: Plumbing. Priority: High.\nProperty: Avondale Court, Flat 1 (ZW).\n\nRing the office."
	if msg != want {
		t.Fatalf("message:\n%s", msg)
	}
	if EnquiryKey(r.ID, r.ID, 1) == EnquiryKey(r.ID, r.ID, 2) {
		t.Fatal("the key does not change with the assignment")
	}
}
