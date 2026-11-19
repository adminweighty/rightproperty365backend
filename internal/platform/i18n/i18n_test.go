package i18n

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"testing"
)

func TestNegotiate(t *testing.T) {
	tests := []struct {
		name, param, header string
		want                string
		fallback            bool
	}{
		{"nothing asked", "", "", "en", false},
		{"explicit en", "en", "", "en", false},
		{"explicit region", "en-GB", "", "en", false},
		{"explicit wins over header", "EN", "fr-FR", "en", false},
		{"unsupported explicit falls back", "fr", "en", "en", true},
		{"header region", "", "en-GB,en;q=0.9", "en", false},
		{"header q order", "", "fr;q=0.9, en;q=0.8", "en", false},
		{"header only unsupported", "", "fr-FR, de;q=0.5", "en", true},
		{"header wildcard", "", "fr;q=0, *", "en", false},
		{"header q=0 ignored", "", "en;q=0", "en", false},
		{"malformed header", "", ";;;,q=abc", "en", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, fallback := Negotiate(tt.param, tt.header)
			if got != tt.want || fallback != tt.fallback {
				t.Fatalf("Negotiate(%q, %q) = %q, %v; want %q, %v", tt.param, tt.header, got, fallback, tt.want, tt.fallback)
			}
		})
	}
}

func TestTranslateFallbacks(t *testing.T) {
	if got := Translate("en", "error.not_found", nil); got != "The requested resource was not found." {
		t.Fatalf("en: %q", got)
	}
	// An unknown language answers in English; an unknown key with the key.
	if got := Translate("fr", "error.not_found", nil); got != "The requested resource was not found." {
		t.Fatalf("fr falls back to English: %q", got)
	}
	if got := Translate("en", "no.such.key", nil); got != "no.such.key" {
		t.Fatalf("missing key: %q", got)
	}
	if got := Translate("en", "field.too_long", Args{"max": 40}); got != "must be at most 40 characters" {
		t.Fatalf("args: %q", got)
	}
	ctx := WithLanguage(context.Background(), "xx")
	if Lang(ctx) != "en" || Lang(context.Background()) != "en" {
		t.Fatal("an unsupported language is stored as the default")
	}
	if T(ctx, "legal.kind.TERMS", nil) != "Terms and Conditions" {
		t.Fatal("T")
	}
}

func TestMiddleware(t *testing.T) {
	var seen string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = Lang(r.Context()) }))
	req := httptest.NewRequest(http.MethodGet, "/x?lang=fr", nil)
	req.Header.Set("Accept-Language", "en-GB")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "en" {
		t.Fatalf("lang = %q", seen)
	}
}

// Every language other than English may only translate English's keys,
// with the same {placeholders}; English is the default and listed first.
func TestCataloguesUseKnownKeys(t *testing.T) {
	if Languages()[0].Code != DefaultLanguage || !Supported(DefaultLanguage) {
		t.Fatal("the default language must be listed first")
	}
	for _, l := range Languages() {
		if _, ok := catalogues[l.Code]; !ok {
			t.Fatalf("language %s has no catalogue", l.Code)
		}
	}
	placeholder := regexp.MustCompile(`\{[a-z_]+\}`)
	for lang, cat := range catalogues {
		if !Supported(lang) {
			t.Fatalf("catalogue %s is not a listed language", lang)
		}
		for key, msg := range cat {
			ref, ok := catalogueEN[key]
			if !ok {
				t.Fatalf("%s: key %q is not in English", lang, key)
			}
			a, b := placeholder.FindAllString(ref, -1), placeholder.FindAllString(msg, -1)
			slices.Sort(a)
			slices.Sort(b)
			if !slices.Equal(a, b) {
				t.Fatalf("%s: %q placeholders %v, English has %v", lang, key, b, a)
			}
			if msg == "" {
				t.Fatalf("%s: %q is empty", lang, key)
			}
		}
	}
}
