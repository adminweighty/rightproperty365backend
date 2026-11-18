package crm

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

// Normalisation of the identifiers duplicates and links are matched on.
// Nothing here makes a network call: the phone rules are a small table of
// the markets the platform works in first, and any number written in
// international form (+ or 00) is accepted for every other country.

var emailRe = regexp.MustCompile(`^[a-z0-9.!#$%&'*+/=?^_{|}~-]{1,64}@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// ErrInvalidEmail and ErrInvalidPhone are returned by the normalisers.
var (
	ErrInvalidEmail = errors.New("is not a valid email address")
	ErrInvalidPhone = errors.New("is not a valid phone number")
	// ErrPhoneNeedsCountry: a national number without a country to read it in.
	ErrPhoneNeedsCountry = errors.New("must start with + and the country code")
)

// NormaliseEmail trims and lower-cases an email address and checks its shape.
func NormaliseEmail(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if len(e) > 254 || !emailRe.MatchString(e) || strings.Contains(e, "..") {
		return "", ErrInvalidEmail
	}
	return e, nil
}

// phoneRule is how national numbers are written in one country.
type phoneRule struct {
	cc    string // country calling code
	trunk string // national (trunk) prefix, "" when none
	// lengths are the accepted lengths of the national significant number.
	lengths []int
}

// phoneRules are the countries whose national numbers are understood. Other
// countries' numbers must be given in international form.
var phoneRules = map[string]phoneRule{
	"ZW": {"263", "0", []int{7, 8, 9}},
	"ZA": {"27", "0", []int{9}},
	"BW": {"267", "", []int{7, 8}},
	"ZM": {"260", "0", []int{9}},
	"MZ": {"258", "", []int{8, 9}},
	"NA": {"264", "0", []int{7, 8, 9}},
	"MW": {"265", "0", []int{7, 9}},
	"KE": {"254", "0", []int{9}},
	"TZ": {"255", "0", []int{9}},
	"UG": {"256", "0", []int{9}},
	"NG": {"234", "0", []int{8, 10}},
	"GH": {"233", "0", []int{9}},
	"AE": {"971", "0", []int{8, 9}},
	"GB": {"44", "0", []int{9, 10}},
	"IE": {"353", "0", []int{7, 8, 9}},
	"US": {"1", "1", []int{10}},
	"CA": {"1", "1", []int{10}},
	"AU": {"61", "0", []int{9}},
	"IN": {"91", "0", []int{10}},
}

// PhoneRegions returns the countries whose national numbers NormalisePhone understands.
func PhoneRegions() []string {
	out := make([]string, 0, len(phoneRules))
	for code := range phoneRules {
		out = append(out, code)
	}
	slices.Sort(out)
	return out
}

// ruleFor finds the rule of an international number's calling code (the
// longest that matches; 1 to 3 digits).
func ruleFor(digits string) (phoneRule, bool) {
	for n := 3; n >= 1; n-- {
		if len(digits) <= n {
			continue
		}
		for _, code := range PhoneRegions() { // sorted: CA before US, same rule
			if r := phoneRules[code]; r.cc == digits[:n] {
				return r, true
			}
		}
	}
	return phoneRule{}, false
}

// NormalisePhone returns a phone number in E.164 (+ and 8 to 15 digits).
// A number in international form (+ or 00) is read as such; any other
// number is a national number of region (an ISO country code, the
// organisation's market), with or without its trunk prefix. Spaces, dots,
// dashes, slashes and brackets are ignored, and so is a "(0)" written after
// the country code.
func NormalisePhone(raw, region string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ErrInvalidPhone
	}
	international := false
	switch {
	case strings.HasPrefix(s, "+"):
		international, s = true, s[1:]
	case strings.HasPrefix(s, "00"):
		international, s = true, s[2:]
	}
	// "(0)" after a country code is the trunk prefix written for locals.
	if international {
		s = strings.Replace(s, "(0)", "", 1)
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case strings.ContainsRune(" .-/() ", r):
		default:
			return "", ErrInvalidPhone
		}
	}
	digits := b.String()
	if international {
		if len(digits) < 8 || len(digits) > 15 || digits[0] == '0' {
			return "", ErrInvalidPhone
		}
		if rule, ok := ruleFor(digits); ok {
			nsn := digits[len(rule.cc):]
			if !slices.Contains(rule.lengths, len(nsn)) {
				// "+44 0 20 ..." with the trunk prefix kept.
				if rule.trunk != "" && strings.HasPrefix(nsn, rule.trunk) && slices.Contains(rule.lengths, len(nsn)-len(rule.trunk)) {
					return "+" + rule.cc + nsn[len(rule.trunk):], nil
				}
				return "", ErrInvalidPhone
			}
		}
		return "+" + digits, nil
	}
	rule, ok := phoneRules[strings.ToUpper(strings.TrimSpace(region))]
	if !ok {
		return "", ErrPhoneNeedsCountry
	}
	nsn := digits
	if rule.trunk != "" && strings.HasPrefix(nsn, rule.trunk) && slices.Contains(rule.lengths, len(nsn)-len(rule.trunk)) {
		nsn = nsn[len(rule.trunk):]
	} else if strings.HasPrefix(nsn, rule.cc) && slices.Contains(rule.lengths, len(nsn)-len(rule.cc)) && !slices.Contains(rule.lengths, len(nsn)) {
		// The country code without the plus ("263 77 ...").
		nsn = nsn[len(rule.cc):]
	}
	if !slices.Contains(rule.lengths, len(nsn)) || (rule.trunk == "0" && strings.HasPrefix(nsn, "0")) {
		return "", ErrInvalidPhone
	}
	return "+" + rule.cc + nsn, nil
}

// phoneTail is the digit string Items.Candidates pre-selects phones by: the
// last 7 digits of an E.164 number.
func phoneTail(e164 string) string {
	d := strings.TrimPrefix(e164, "+")
	if len(d) > 7 {
		return d[len(d)-7:]
	}
	return d
}

// normaliseTag trims and lower-cases a tag and folds inner white space.
func normaliseTag(raw string) string {
	return strings.Join(strings.Fields(strings.ToLower(raw)), " ")
}
