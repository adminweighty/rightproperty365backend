package markets

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Every ISO 3166-1 country is in the table once, with a well-formed code and
// currency and a time zone that loads.
func TestCountryTable(t *testing.T) {
	list := Seed()
	if len(list) != CountryCount || len(list) != len(strings.Split(strings.TrimSpace(countryTable), "\n")) {
		t.Fatalf("%d countries, want %d", len(list), CountryCount)
	}
	codeRe, currencyRe := regexp.MustCompile(`^[A-Z]{2}$`), regexp.MustCompile(`^[A-Z]{3}$`)
	codes, ids, names := map[string]bool{}, map[string]string{}, map[string]string{}
	for _, m := range list {
		if !codeRe.MatchString(m.Code) || codes[m.Code] {
			t.Fatalf("code %q is malformed or repeated", m.Code)
		}
		codes[m.Code] = true
		if !currencyRe.MatchString(m.CurrencyCode) {
			t.Fatalf("%s: currency %q is not three capital letters", m.Code, m.CurrencyCode)
		}
		if _, err := time.LoadLocation(m.Timezone); err != nil || m.Timezone == "" || m.Timezone == "Local" {
			t.Fatalf("%s: time zone %q does not load: %v", m.Code, m.Timezone, err)
		}
		if strings.TrimSpace(m.Name) != m.Name || m.Name == "" || len([]rune(m.Name)) > 60 || strings.ContainsAny(m.Name, "<>|") {
			t.Fatalf("%s: name %q", m.Code, m.Name)
		}
		if other, dup := names[Fold(m.Name)]; dup {
			t.Fatalf("%s and %s share the name %q", m.Code, other, m.Name)
		}
		names[Fold(m.Name)] = m.Code
		if other, dup := ids[m.ID.String()]; dup {
			t.Fatalf("%s and %s share the ID %s", m.Code, other, m.ID)
		}
		ids[m.ID.String()] = m.Code
		if !m.Enabled || m.Status != StatusActive {
			t.Fatalf("%s is not enabled and active: %+v", m.Code, m)
		}
	}
}

// Spot checks, and the five countries that existed before keep their IDs.
func TestKnownCountries(t *testing.T) {
	byCode := map[string]Market{}
	for _, m := range Seed() {
		byCode[m.Code] = m
	}
	for code, want := range map[string][3]string{
		"GB": {"United Kingdom", "GBP", "Europe/London"},
		"US": {"United States", "USD", "America/New_York"},
		"AE": {"United Arab Emirates", "AED", "Asia/Dubai"},
		"ZA": {"South Africa", "ZAR", "Africa/Johannesburg"},
		"IN": {"India", "INR", "Asia/Kolkata"},
		"AU": {"Australia", "AUD", "Australia/Sydney"},
		"MU": {"Mauritius", "MUR", "Indian/Mauritius"},
		"KE": {"Kenya", "KES", "Africa/Nairobi"},
		"ZW": {"Zimbabwe", "USD", "Africa/Harare"},
		"BW": {"Botswana", "BWP", "Africa/Gaborone"},
		"FR": {"France", "EUR", "Europe/Paris"},
	} {
		m := byCode[code]
		if got := [3]string{m.Name, m.CurrencyCode, m.Timezone}; got != want {
			t.Fatalf("%s = %v, want %v", code, got, want)
		}
	}
	for code, id := range map[string]string{
		"ZW": "0195c0de-0000-7000-8000-00000000a001", "BW": "0195c0de-0000-7000-8000-00000000a002",
		"ZA": "0195c0de-0000-7000-8000-00000000a003", "AE": "0195c0de-0000-7000-8000-00000000a004",
		"GB": "0195c0de-0000-7000-8000-00000000a005", "MU": "0195c0de-0000-7000-8000-0000c0de4d55",
	} {
		if got := byCode[code].ID.String(); got != id || IDFor(code).String() != id {
			t.Fatalf("%s has ID %s, want %s", code, got, id)
		}
	}
	// Zimbabwe and the United Kingdom lead; the rest follow by name, and an
	// accented name sorts by its base letter.
	list := Seed()
	if list[0].Code != "ZW" || list[1].Code != "GB" || list[2].Code != "AF" || list[3].Code != "AX" || list[4].Code != "AL" {
		t.Fatalf("order starts %s %s %s %s %s", list[0].Code, list[1].Code, list[2].Code, list[3].Code, list[4].Code)
	}
}

func TestCountriesQueryAndListingCounts(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(store, func(context.Context) (map[string]int, error) {
		return map[string]int{"ZW": 20, "GB": 3, "MU": 3, "KE": 0}, nil
	}, nil)
	codes := func(q string, only bool) string {
		list, err := h.Countries(context.Background(), q, only)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range list {
			if m.HasListings != (m.ListingCount > 0) || m.Status != StatusActive {
				t.Fatalf("%+v", m)
			}
			out = append(out, m.Code)
		}
		return strings.Join(out, ",")
	}
	if got := codes("", true); got != "ZW,MU,GB" { // most listings first, then by name
		t.Fatalf("has_listings = %s", got)
	}
	for q, want := range map[string]string{
		"king":    "GB",
		"KING":    "GB",
		"gb":      "GB",
		"reunion": "RE",
		"Réunion": "RE",
		"cote d":  "CI",
		"turkiye": "TR",
		"aland i": "AX",
		"zz":      "",
		"united":  "GB,AE,US,UM",
	} {
		if got := codes(q, false); got != want {
			t.Fatalf("q=%s = %s, want %s", q, got, want)
		}
	}
	if got := codes("ma", true); got != "MU" {
		t.Fatalf("q and has_listings = %s", got)
	}
	// A country that was switched off is not listed.
	if !store.SetEnabled("GB", false) || codes("king", false) != "" || codes("", true) != "ZW,MU" {
		t.Fatal("a disabled country is still listed")
	}
}
