package agreements

import "testing"

func TestSignatureFontsCoverLatinNames(t *testing.T) {
	for _, sf := range signatureFonts {
		for _, name := range []string{"Zoë Müller-Śmiałek", "Tendai Chiwenga", "José Núñez", "Ngũgĩ wa Thiong'o"} {
			if !sf.covers(name) {
				t.Errorf("%s does not cover %q", sf.Key, name)
			}
			if got := fontFor(sf.Key, name); got == "" || got[len(got)-2:] == "fb" {
				t.Errorf("fontFor(%s, %q) = %s, want the script font", sf.Key, name, got)
			}
		}
	}
}

func TestSignatureFontFallsBackForMissingLetters(t *testing.T) {
	if got := fontFor("FLOWING", "王小明"); got != "sigflowingfb" {
		t.Errorf("Chinese name drawn with %s, want the Go Italic fallback", got)
	}
	if got := fontFor("UNKNOWN", "Ann"); got != "sigflowing" {
		t.Errorf("unknown style drawn with %s, want the first style", got)
	}
}
