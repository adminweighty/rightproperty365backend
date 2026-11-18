package crm

import (
	"errors"
	"testing"
)

func TestNormalisePhone(t *testing.T) {
	cases := []struct {
		raw, region, want string
		err               error
	}{
		// Zimbabwe: national with the trunk 0, without it, with the country code.
		{"077 123 4567", "ZW", "+263771234567", nil},
		{"0771234567", "ZW", "+263771234567", nil},
		{"771234567", "ZW", "+263771234567", nil},
		{"263771234567", "ZW", "+263771234567", nil},
		{"+263 77 123 4567", "ZW", "+263771234567", nil},
		{"00263771234567", "", "+263771234567", nil},
		{"+263 (0)77 123 4567", "", "+263771234567", nil},
		{"+263 0771234567", "", "+263771234567", nil},
		{"(024) 2700-123", "ZW", "+263242700123", nil},
		// South Africa, the United Kingdom, the United States.
		{"082 123 4567", "ZA", "+27821234567", nil},
		{"+27 82 123 4567", "GB", "+27821234567", nil},
		{"020 7946 0018", "GB", "+442079460018", nil},
		{"07700 900123", "GB", "+447700900123", nil},
		{"+44 (0)20 7946 0018", "ZW", "+442079460018", nil},
		{"(212) 555-0142", "US", "+12125550142", nil},
		{"1 212 555 0142", "US", "+12125550142", nil},
		{"71 234 567", "BW", "+26771234567", nil},
		{"050 123 4567", "AE", "+971501234567", nil},
		// Any country in international form.
		{"+7 912 345 67 89", "ZW", "+79123456789", nil},
		{"+33 1 23 45 67 89", "", "+33123456789", nil},
		// Refused.
		{"", "ZW", "", ErrInvalidPhone},
		{"0771234567", "", "", ErrPhoneNeedsCountry},
		{"0771234567", "FR", "", ErrPhoneNeedsCountry},
		{"077 123 456", "ZW", "+26377123456", nil},
		{"07712", "ZW", "", ErrInvalidPhone},
		{"077 123 4567 ext 2", "ZW", "", ErrInvalidPhone},
		{"+0 123 456 789", "", "", ErrInvalidPhone},
		{"+1234567", "", "", ErrInvalidPhone},
		{"+1234567890123456", "", "", ErrInvalidPhone},
		{"+263 77 123 45678 9", "", "", ErrInvalidPhone},
		{"082 123 456", "ZA", "", ErrInvalidPhone},
		{"abc", "ZW", "", ErrInvalidPhone},
	}
	for _, tc := range cases {
		got, err := NormalisePhone(tc.raw, tc.region)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("NormalisePhone(%q, %q) = %q, %v; want %q, %v", tc.raw, tc.region, got, err, tc.want, tc.err)
		}
	}
	for _, code := range PhoneRegions() {
		if r := phoneRules[code]; r.cc == "" || len(r.lengths) == 0 {
			t.Errorf("%s: incomplete rule", code)
		}
	}
}

func TestNormaliseEmail(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"  Tariro@Example.COM ", "tariro@example.com"},
		{"t.marufu+crm@example.co.zw", "t.marufu+crm@example.co.zw"},
		{"a@b.co", "a@b.co"},
	}
	for _, tc := range cases {
		if got, err := NormaliseEmail(tc.raw); err != nil || got != tc.want {
			t.Errorf("NormaliseEmail(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "plain", "@example.com", "a@", "a@b", "a b@example.com", "a..b@example.com", "a@-example.com",
		"a@example..com"} {
		if got, err := NormaliseEmail(bad); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("NormaliseEmail(%q) = %q, %v; want an error", bad, got, err)
		}
	}
}

func TestPhoneTailAndTags(t *testing.T) {
	if got := phoneTail("+263771234567"); got != "1234567" {
		t.Fatalf("tail = %q", got)
	}
	if got := normaliseTag("  First   Time Buyer "); got != "first time buyer" {
		t.Fatalf("tag = %q", got)
	}
}
