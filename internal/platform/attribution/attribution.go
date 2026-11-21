// Package attribution is the campaign a visit came from (API 0.58.0,
// FR-013): the utm_source, utm_medium and utm_campaign of the page the
// visitor landed on, kept by the browser for its session and sent with
// property requests and analytics events.
//
// Rules kept here, whatever the caller:
//
//   - Do Not Track ("DNT: 1") or Global Privacy Control ("Sec-GPC: 1") on
//     the request means no campaign is stored (Suppressed);
//   - values are cleaned, never rejected: lower case, spaces collapsed,
//     only letters, digits and . _ - + : / kept, at most 100 characters;
//     a campaign with nothing left is no campaign;
//   - nothing else of the landing URL is received (no term, content, click
//     identifiers or referrer).
package attribution

import (
	"net/http"
	"strings"
	"unicode"
)

// MaxLength is the longest value kept, in characters.
const MaxLength = 100

// Campaign is where a visit came from. The zero value is "no campaign".
type Campaign struct {
	Source   string `json:"source"`
	Medium   string `json:"medium"`
	Campaign string `json:"campaign"`
}

// Input is the campaign as clients send it: `{"source", "medium",
// "campaign"}`, every key optional.
type Input struct {
	Source   *string `json:"source"`
	Medium   *string `json:"medium"`
	Campaign *string `json:"campaign"`
}

// Empty reports whether there is no campaign.
func (c Campaign) Empty() bool { return c.Source == "" && c.Medium == "" && c.Campaign == "" }

// Ptr returns nil for no campaign, for JSON views (`campaign: null`).
func (c Campaign) Ptr() *Campaign {
	if c.Empty() {
		return nil
	}
	return &c
}

// Clean returns one value as it is stored.
func Clean(raw string) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if n >= MaxLength {
			break
		}
		switch {
		case unicode.IsSpace(r) || r == ' ':
			space = true
			continue
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)), strings.ContainsRune("._-+:/", r):
		case r >= 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
		default:
			continue
		}
		if space && b.Len() > 0 {
			if n+1 >= MaxLength {
				break
			}
			b.WriteByte(' ')
			n++
		}
		space = false
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func clean(p *string) string {
	if p == nil {
		return ""
	}
	return Clean(*p)
}

// From cleans an input. Nil is no campaign.
func From(in *Input) Campaign {
	if in == nil {
		return Campaign{}
	}
	return Campaign{Source: clean(in.Source), Medium: clean(in.Medium), Campaign: clean(in.Campaign)}
}

// Suppressed reports whether the request asks not to be tracked: Do Not
// Track or Global Privacy Control.
func Suppressed(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.TrimSpace(r.Header.Get("DNT")) == "1" || strings.TrimSpace(r.Header.Get("Sec-GPC")) == "1"
}

// FromRequest is From, or no campaign when the request is Suppressed.
func FromRequest(r *http.Request, in *Input) Campaign {
	if Suppressed(r) {
		return Campaign{}
	}
	return From(in)
}

// Nullable returns the stored form of a value: nil for "".
func Nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Deref is the reverse of Nullable.
func Deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
