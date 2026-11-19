package developments

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"rightproperty365.com/backend/internal/modules/listings"
)

// Contact details in free text. Customers reach a developer only through an
// enquiry, so an email address, a phone number or a WhatsApp link typed into
// a project's text is refused.
var (
	emailInText    = regexp.MustCompile(`[^\s@]+@[^\s@]+\.[A-Za-z]{2,}`)
	phoneInText    = regexp.MustCompile(`(?:\+|\b0)\d[\d \-()]{6,}\d`)
	whatsAppInText = regexp.MustCompile(`(?i)\bwa\.me/|whatsapp\.com/`)
	slugRe         = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	nonSlug        = regexp.MustCompile(`[^a-z0-9]+`)
	quarterRe      = regexp.MustCompile(`^([0-9]{4})-Q([1-4])$`)
	currencyRe     = regexp.MustCompile(`^[A-Z]{3}$`)
)

// MsgContactDetails is the message of free text with contact details.
const MsgContactDetails = "must not contain contact details (email addresses, phone numbers or WhatsApp links): customers reach you through enquiries"

// HasContactDetails reports whether text carries an email address, a phone
// number (one written with + or a leading 0 and nine digits or more, spaces,
// dashes and brackets allowed: "+263 77 123 4567", "0771 234 567") or a
// WhatsApp link. A price range such as "185000-240000" is not a phone.
func HasContactDetails(text string) bool {
	if emailInText.MatchString(text) || whatsAppInText.MatchString(text) {
		return true
	}
	for _, m := range phoneInText.FindAllString(text, -1) {
		digits := 0
		for _, c := range m {
			if c >= '0' && c <= '9' {
				digits++
			}
		}
		if digits >= 9 {
			return true
		}
	}
	return false
}

// checkText validates free text: the listing plain-text rule and no contact
// details. It returns "" when the text is fine.
func checkText(text string, multiline bool, maxLength int) string {
	if problem := listings.PlainText(text, multiline, maxLength); problem != "" {
		return problem
	}
	if HasContactDetails(text) {
		return MsgContactDetails
	}
	return ""
}

// Slugify is a project's default address: its name in URL form.
func Slugify(name string) string {
	base := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(base) > MaxSlugLength-4 {
		base = strings.Trim(base[:MaxSlugLength-4], "-")
	}
	if base == "" {
		base = "development"
	}
	return base
}

// Completion is a completion estimate as answered.
type Completion struct {
	// Kind is QUARTER or DATE.
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Label string `json:"label"`
}

// ParseCompletion checks a completion estimate ("2027-Q3" or "2027-06-30")
// and returns how it is shown; ok is false when it is neither.
func ParseCompletion(v string) (Completion, bool) {
	if m := quarterRe.FindStringSubmatch(v); m != nil {
		year, _ := strconv.Atoi(m[1])
		if year < 1900 || year > 2200 {
			return Completion{}, false
		}
		return Completion{Kind: "QUARTER", Value: v, Label: "Q" + m[2] + " " + m[1]}, true
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil || t.Year() < 1900 || t.Year() > 2200 {
		return Completion{}, false
	}
	return Completion{Kind: "DATE", Value: v, Label: t.Format("2 January 2006")}, true
}

func completionOf(v string) *Completion {
	if v == "" {
		return nil
	}
	c, ok := ParseCompletion(v)
	if !ok {
		return nil
	}
	return &c
}

// patch is a decoded PATCH or create body: the keys it carries.
type patch map[string]json.RawMessage

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

// keys checks that every key is allowed.
func (p patch) keys(allowed []string, fields map[string]string) {
	for k := range p {
		ok := false
		for _, a := range allowed {
			if a == k {
				ok = true
				break
			}
		}
		if !ok {
			fields[k] = "is not a field here"
		}
	}
}

// str reads a string or null ("" for null). ok is false when the key is absent.
func (p patch) str(key string, fields map[string]string) (string, bool) {
	raw, present := p[key]
	if !present {
		return "", false
	}
	if isNull(raw) {
		return "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		fields[key] = "must be a string or null"
		return "", true
	}
	return strings.TrimSpace(s), true
}

// integer reads a whole number or null in [lo, hi].
func (p patch) integer(key string, lo, hi int, fields map[string]string) (*int, bool) {
	raw, present := p[key]
	if !present {
		return nil, false
	}
	if isNull(raw) {
		return nil, true
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		fields[key] = "must be a whole number or null"
		return nil, true
	}
	v, err := strconv.Atoi(n.String())
	if err != nil {
		fields[key] = "must be a whole number or null"
		return nil, true
	}
	if v < lo || v > hi {
		fields[key] = fmt.Sprintf("must be from %d to %d", lo, hi)
		return nil, true
	}
	return &v, true
}

// coordinate reads a number or null.
func (p patch) coordinate(key string, fields map[string]string) (*float64, bool) {
	raw, present := p[key]
	if !present {
		return nil, false
	}
	if isNull(raw) {
		return nil, true
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		fields[key] = listings.MsgCoordinateNumber
		return nil, true
	}
	return &f, true
}

// boolean reads true or false.
func (p patch) boolean(key string, fields map[string]string) (bool, bool) {
	raw, present := p[key]
	if !present {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		fields[key] = "must be true or false"
	}
	return b, true
}

// Fields a body may carry.
var (
	projectFields = []string{"name", "slug", "market", "status", "completion_estimate", "summary", "description",
		"city", "suburb", "city_name", "latitude", "longitude", "location_precision", "amenities", "investment_highlights", "youtube_url"}
	phaseFields    = []string{"name", "status", "completion_estimate", "position"}
	unitTypeFields = []string{"name", "phase_id", "bedrooms", "bathrooms", "size_min_m2", "size_max_m2", "price_on_request",
		"price_from", "price_to", "currency", "units_total", "units_available", "position"}
)

func checkStatus(key, v string, fields map[string]string) {
	if StatusLabel(v) == "" {
		fields[key] = "must be one of " + strings.Join(Statuses(), ", ")
	}
}

func checkCompletion(key, v string, fields map[string]string) {
	if v == "" {
		return
	}
	if _, ok := ParseCompletion(v); !ok {
		fields[key] = `must be a quarter such as "2027-Q3", a date such as "2027-06-30", or null`
	}
}

// applyPhase writes the phase fields of a body onto ph.
func applyPhase(p patch, ph *Phase, fields map[string]string) {
	p.keys(phaseFields, fields)
	if v, ok := p.str("name", fields); ok {
		ph.Name = v
	}
	if v, ok := p.str("status", fields); ok {
		ph.Status = v
	}
	if v, ok := p.str("completion_estimate", fields); ok {
		ph.Completion = v
	}
	if v, ok := p.integer("position", 0, 1000, fields); ok && v != nil {
		ph.Position = *v
	}
	switch {
	case ph.Name == "":
		fields["name"] = "is required"
	default:
		if problem := checkText(ph.Name, false, MaxPhaseNameLength); problem != "" {
			fields["name"] = problem
		}
	}
	if ph.Status == "" {
		ph.Status = StatusPlanned
	}
	checkStatus("status", ph.Status, fields)
	checkCompletion("completion_estimate", ph.Completion, fields)
}

// applyUnitType writes the unit-type fields of a body onto u. The phase
// move (phase_id) is the caller's.
func applyUnitType(p patch, u *UnitType, fields map[string]string) {
	p.keys(unitTypeFields, fields)
	if v, ok := p.str("name", fields); ok {
		u.Name = v
	}
	if v, ok := p.integer("bedrooms", 0, MaxRoomCount, fields); ok {
		u.Bedrooms = v
	}
	if v, ok := p.integer("bathrooms", 0, MaxRoomCount, fields); ok {
		u.Bathrooms = v
	}
	if v, ok := p.integer("size_min_m2", 1, MaxSizeM2, fields); ok {
		u.SizeMin = v
	}
	if v, ok := p.integer("size_max_m2", 1, MaxSizeM2, fields); ok {
		u.SizeMax = v
	}
	if v, ok := p.integer("units_total", 0, 1_000_000, fields); ok {
		u.UnitsTotal = v
	}
	if v, ok := p.integer("units_available", 0, 1_000_000, fields); ok {
		u.UnitsAvailable = v
	}
	if v, ok := p.integer("position", 0, 1000, fields); ok && v != nil {
		u.Position = *v
	}
	if v, ok := p.boolean("price_on_request", fields); ok {
		u.PriceOnRequest = v
		if v {
			// "Price on request" clears the price unless the body sets one
			// (which is then refused below).
			u.PriceFrom, u.PriceTo, u.Currency = "", "", ""
		}
	}
	for _, f := range []struct {
		key string
		dst *string
	}{{"price_from", &u.PriceFrom}, {"price_to", &u.PriceTo}} {
		v, ok := p.str(f.key, fields)
		if !ok {
			continue
		}
		if v == "" {
			*f.dst = ""
			continue
		}
		r, err := listings.ParseAmount(v)
		if err != nil {
			fields[f.key] = `must be a decimal amount such as "185000" or "185000.50", or null`
			continue
		}
		*f.dst = listings.FormatAmount(r)
	}
	if v, ok := p.str("currency", fields); ok {
		u.Currency = strings.ToUpper(v)
	}

	if u.Name == "" {
		fields["name"] = "is required"
	} else if problem := checkText(u.Name, false, MaxUnitTypeNameLength); problem != "" {
		fields["name"] = problem
	}
	if u.SizeMin != nil && u.SizeMax != nil && *u.SizeMin > *u.SizeMax {
		fields["size_max_m2"] = "must not be smaller than size_min_m2"
	}
	if u.UnitsTotal != nil && u.UnitsAvailable != nil && *u.UnitsAvailable > *u.UnitsTotal {
		fields["units_available"] = "must not be more than units_total"
	}
	if u.PriceOnRequest {
		for key, v := range map[string]string{"price_from": u.PriceFrom, "price_to": u.PriceTo, "currency": u.Currency} {
			if v != "" {
				fields[key] = "must be null when the price is on request"
			}
		}
		return
	}
	if _, bad := fields["price_from"]; !bad && u.PriceFrom == "" {
		fields["price_from"] = "is required unless price_on_request is true"
	}
	if u.Currency == "" {
		fields["currency"] = "is required with a price"
	} else if !currencyRe.MatchString(u.Currency) {
		fields["currency"] = "must be an ISO 4217 code such as USD"
	}
	if u.PriceFrom != "" && u.PriceTo != "" {
		from, _ := listings.ParseAmount(u.PriceFrom)
		to, _ := listings.ParseAmount(u.PriceTo)
		if from != nil && to != nil && to.Cmp(from) < 0 {
			fields["price_to"] = "must not be lower than price_from"
		}
	}
}
