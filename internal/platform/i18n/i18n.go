// Package i18n is a small message catalogue (API 0.43.0, BR-001).
//
// Messages are looked up by a stable key in the language of the request:
//
//	i18n.T(ctx, "error.not_found", nil)
//	i18n.T(ctx, "legal.kind.TERMS", nil)
//	i18n.T(ctx, "example.greeting", i18n.Args{"name": "Rudo"}) // "Hello {name}"
//
// The language is chosen once per request by Middleware: a supported `lang`
// query parameter, else the best supported match of Accept-Language, else
// English. English is the only filled language and the fallback for every
// key: a key missing in another language is answered in English, and a key
// missing everywhere is answered with the key itself (never an empty string).
//
// Only a few places use the catalogue so far (the error envelope's shared
// messages and the legal routes). Everything else still writes literal
// English; convert it place by place, never all at once.
//
// # Adding a language
//
//  1. Add it to Languages() (code, English name, native name), keeping
//     DefaultLanguage first.
//  2. Add its messages in a file catalogue_<code>.go: a map literal like
//     catalogueEN, registered in catalogues. Keys you leave out fall back to
//     English, so a partial translation is safe.
//  3. Run the tests: TestCataloguesUseKnownKeys refuses a key English does not
//     have and placeholders ({name}) that differ from English.
//  4. Legal documents are translated as new versions with that `lang` (the
//     admin API accepts a language once it is listed here).
package i18n

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// DefaultLanguage is English: always supported, the fallback for everything.
const DefaultLanguage = "en"

// Language is one supported language.
type Language struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	NativeName string `json:"native_name"`
}

// Languages returns the supported languages, the default first.
func Languages() []Language {
	return []Language{{Code: "en", Name: "English", NativeName: "English"}}
}

// Supported reports whether code is a supported language.
func Supported(code string) bool {
	return slices.ContainsFunc(Languages(), func(l Language) bool { return l.Code == code })
}

// catalogues holds every language's messages.
var catalogues = map[string]map[string]string{
	"en": catalogueEN,
}

// Args fills the {placeholders} of a message.
type Args map[string]any

type langKey struct{}

// WithLanguage returns ctx carrying the language code (must be supported;
// anything else is stored as the default).
func WithLanguage(ctx context.Context, code string) context.Context {
	if !Supported(code) {
		code = DefaultLanguage
	}
	return context.WithValue(ctx, langKey{}, code)
}

// Lang returns the request's language (the default when none was chosen).
func Lang(ctx context.Context) string {
	if ctx != nil {
		if code, ok := ctx.Value(langKey{}).(string); ok && code != "" {
			return code
		}
	}
	return DefaultLanguage
}

// T returns the message for key in the context's language, with args filled.
func T(ctx context.Context, key string, args Args) string {
	return Translate(Lang(ctx), key, args)
}

// Translate returns the message for key in lang (falling back to English,
// then to the key itself), with args filled.
func Translate(lang, key string, args Args) string {
	msg, ok := catalogues[lang][key]
	if !ok {
		msg, ok = catalogues[DefaultLanguage][key]
	}
	if !ok {
		msg = key
	}
	return fill(msg, args)
}

// Has reports whether English (the reference catalogue) defines key.
func Has(key string) bool {
	_, ok := catalogues[DefaultLanguage][key]
	return ok
}

func fill(msg string, args Args) string {
	if len(args) == 0 || !strings.Contains(msg, "{") {
		return msg
	}
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, 2*len(names))
	for _, name := range names {
		pairs = append(pairs, "{"+name+"}", toString(args[name]))
	}
	return strings.NewReplacer(pairs...).Replace(msg)
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case interface{ String() string }:
		return x.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

// Negotiate chooses the language: a supported explicit `lang` wins, else the
// best supported Accept-Language entry, else the default. It also reports
// whether the result was a fallback (something was asked for and not found).
func Negotiate(langParam, acceptLanguage string) (code string, fallback bool) {
	langParam = strings.ToLower(strings.TrimSpace(langParam))
	if langParam != "" {
		if Supported(primary(langParam)) {
			return primary(langParam), false
		}
		return DefaultLanguage, true
	}
	ranges := parseAcceptLanguage(acceptLanguage)
	for _, r := range ranges {
		if r.tag == "*" {
			return DefaultLanguage, false
		}
		if Supported(primary(r.tag)) {
			return primary(r.tag), false
		}
	}
	return DefaultLanguage, len(ranges) > 0
}

// primary is the language subtag of a BCP 47 tag ("en-GB" -> "en").
func primary(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

type langRange struct {
	tag string
	q   float64
}

// parseAcceptLanguage returns the ranges with q > 0, best first (stable for
// equal q). Malformed entries are skipped. At most 20 entries are read.
func parseAcceptLanguage(header string) []langRange {
	var out []langRange
	for i, part := range strings.Split(header, ",") {
		if i >= 20 {
			break
		}
		fields := strings.Split(strings.TrimSpace(part), ";")
		tag := strings.TrimSpace(fields[0])
		if tag == "" || len(tag) > 35 {
			continue
		}
		q := 1.0
		for _, f := range fields[1:] {
			f = strings.TrimSpace(f)
			if v, ok := strings.CutPrefix(f, "q="); ok {
				parsed, err := strconv.ParseFloat(v, 64)
				if err != nil || parsed < 0 || parsed > 1 {
					parsed = 0
				}
				q = parsed
			}
		}
		if q > 0 {
			out = append(out, langRange{tag: tag, q: q})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].q > out[j].q })
	return out
}

// Middleware puts the negotiated language in every request's context.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, _ := Negotiate(r.URL.Query().Get("lang"), r.Header.Get("Accept-Language"))
		next.ServeHTTP(w, r.WithContext(WithLanguage(r.Context(), code)))
	})
}
