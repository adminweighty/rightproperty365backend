package attribution

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"  Facebook ":             "facebook",
		"Summer   Sale\t2026":     "summer sale 2026",
		"cpc<script>":             "cpcscript",
		"news-letter_01.v2+x:y/z": "news-letter_01.v2+x:y/z",
		"\"quoted'\x00":           "quoted",
		"Ñandú":                   "ñandú",
		"":                        "",
		"   ":                     "",
		strings.Repeat("a", 150):  strings.Repeat("a", MaxLength),
		strings.Repeat("ab ", 60): strings.Repeat("ab ", 60)[:MaxLength],
	} {
		got := Clean(in)
		if got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
		if len([]rune(got)) > MaxLength {
			t.Errorf("Clean(%q) is %d characters", in, len([]rune(got)))
		}
	}
}

func TestFromRequest(t *testing.T) {
	s := func(v string) *string { return &v }
	in := &Input{Source: s("Google"), Medium: s("CPC"), Campaign: s("Launch")}
	r := httptest.NewRequest("POST", "/", nil)
	if c := FromRequest(r, in); c != (Campaign{"google", "cpc", "launch"}) {
		t.Fatalf("%+v", c)
	}
	for _, h := range []string{"DNT", "Sec-GPC"} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set(h, "1")
		if c := FromRequest(r, in); !c.Empty() || c.Ptr() != nil {
			t.Fatalf("%s: %+v", h, c)
		}
	}
	r.Header.Set("DNT", "0")
	if FromRequest(r, in).Empty() {
		t.Fatal("DNT 0 suppressed")
	}
	if c := From(&Input{Source: s("<>")}); !c.Empty() {
		t.Fatalf("nothing left: %+v", c)
	}
	if From(nil).Ptr() != nil || Nullable("") != nil || Deref(Nullable("x")) != "x" {
		t.Fatal("helpers")
	}
}
