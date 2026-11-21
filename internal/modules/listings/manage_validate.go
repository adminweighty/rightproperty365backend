package listings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Limits of the editable fields.
const (
	MaxTitleLength       = 120
	MinTitleLength       = 5
	MaxSummaryLength     = 300
	MaxDescriptionLength = 8000
	MinDescriptionLength = 30
	MaxFeatures          = 30
	MaxRooms             = 50
	MaxGuestsLimit       = 100
	MaxMinNights         = 365
	// maxPriceDigits bounds the whole part of a price (numeric(20, 4) holds 16).
	maxPriceDigits = 15
)

// patchFields are the keys a create or PATCH body may carry.
var patchFields = []string{
	"title", "summary", "description", "deal", "category", "property_type", "price", "price_period",
	"country", "city", "suburb", "city_name", "region_name", "bedrooms", "bathrooms", "floor_area", "land_area",
	"features", "max_guests", "min_nights", "youtube_url", "latitude", "longitude", "location_precision",
}

// htmlRe finds markup: a "<" that starts a tag, a comment or a declaration.
var htmlRe = regexp.MustCompile(`<\s*[/!?a-zA-Z]`)

// plainText checks free text: no markup, no control characters (line breaks
// and tabs are allowed when multiline). It returns "" when the text is fine.
func plainText(s string, multiline bool, maxLength int) string {
	switch {
	case len([]rune(s)) > maxLength:
		return fmt.Sprintf("must be at most %d characters", maxLength)
	case htmlRe.MatchString(s):
		return "must be plain text, without HTML"
	}
	for _, c := range s {
		if c == '\n' || c == '\r' || c == '\t' {
			if !multiline {
				return "must be plain text on one line"
			}
			continue
		}
		if unicode.IsControl(c) {
			return "must not contain control characters"
		}
	}
	return ""
}

// decodePatch reads a create or PATCH body into the keys it carries. A key
// that is not a listing field is a validation error.
func decodePatch(raw map[string]json.RawMessage, fields map[string]string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range raw {
		if !slices.Contains(patchFields, k) {
			fields[k] = "is not a field of a listing"
			continue
		}
		out[k] = v
	}
	return out
}

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

// applyPatch writes the keys of a patch onto the content. Values of the wrong
// type are reported per field; null clears a field.
func applyPatch(c *Content, patch map[string]json.RawMessage, fields map[string]string) {
	str := func(key string, dst *string, trim bool) {
		v, ok := patch[key]
		if !ok {
			return
		}
		if isNull(v) {
			*dst = ""
			return
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			fields[key] = "must be a string or null"
			return
		}
		if trim {
			s = strings.TrimSpace(s)
		}
		*dst = s
	}
	num := func(key string, dst **int) {
		v, ok := patch[key]
		if !ok {
			return
		}
		if isNull(v) {
			*dst = nil
			return
		}
		var n int
		if json.Unmarshal(v, &n) != nil {
			fields[key] = "must be a whole number or null"
			return
		}
		*dst = &n
	}
	str("title", &c.Title, true)
	str("summary", &c.Summary, true)
	str("description", &c.Description, false)
	c.Description = strings.TrimSpace(c.Description)
	str("deal", &c.Deal, true)
	str("category", &c.Category, true)
	str("property_type", &c.PropertyType, true)
	str("price_period", &c.PricePeriod, true)
	str("city", &c.City, true)
	str("suburb", &c.Suburb, true)
	str("city_name", &c.CityName, true)
	str("region_name", &c.RegionName, true)
	if _, ok := patch["country"]; ok {
		str("country", &c.MarketCode, true)
		c.MarketCode = strings.ToUpper(c.MarketCode)
	}
	num("bedrooms", &c.Bedrooms)
	num("bathrooms", &c.Bathrooms)
	num("floor_area", &c.FloorArea)
	num("land_area", &c.LandArea)
	num("max_guests", &c.MaxGuests)
	num("min_nights", &c.MinNights)
	// The video: a YouTube link, parsed to its id and start time; null or
	// "" removes it. The link itself is never stored.
	if v, ok := patch["youtube_url"]; ok {
		var link string
		switch {
		case isNull(v):
			c.YouTubeVideoID, c.YouTubeStartSeconds = "", nil
		case json.Unmarshal(v, &link) != nil:
			fields["youtube_url"] = "must be a string or null"
		case strings.TrimSpace(link) == "":
			c.YouTubeVideoID, c.YouTubeStartSeconds = "", nil
		default:
			id, start, err := ParseYouTubeURL(link)
			if err != nil {
				fields["youtube_url"] = MsgNotYouTubeVideo
				break
			}
			c.YouTubeVideoID, c.YouTubeStartSeconds = id, start
		}
	}
	// Coordinates: JSON numbers with at most six decimals; null clears one
	// (validateContent then wants both or neither).
	for _, k := range []struct {
		key string
		dst **float64
	}{{"latitude", &c.Latitude}, {"longitude", &c.Longitude}} {
		v, ok := patch[k.key]
		if !ok {
			continue
		}
		if isNull(v) {
			*k.dst = nil
			continue
		}
		f, problem := parseCoordinate(v)
		if problem != "" {
			fields[k.key] = problem
			continue
		}
		*k.dst = &f
	}
	if v, ok := patch["location_precision"]; ok {
		var p string
		switch {
		case isNull(v):
			c.LocationPrecision = PrecisionExact
		case json.Unmarshal(v, &p) != nil:
			fields["location_precision"] = MsgPrecision
		default:
			c.LocationPrecision = p
			if c.LocationPrecision != PrecisionExact && c.LocationPrecision != PrecisionApproximate {
				fields["location_precision"] = MsgPrecision
			}
		}
	}
	if v, ok := patch["features"]; ok {
		var list []string
		switch {
		case isNull(v):
			c.Features = []string{}
		case json.Unmarshal(v, &list) != nil:
			fields["features"] = "must be a list of feature codes"
		default:
			c.Features = []string{}
			for _, f := range list {
				if f = strings.TrimSpace(f); f != "" && !slices.Contains(c.Features, f) {
					c.Features = append(c.Features, f)
				}
			}
		}
	}
	if v, ok := patch["price"]; ok {
		var p struct {
			Amount   *string `json:"amount"`
			Currency *string `json:"currency"`
		}
		switch {
		case isNull(v):
			c.PriceAmount = ""
		case json.Unmarshal(v, &p) != nil:
			fields["price"] = `must be {"amount": "385000.00", "currency": "USD"} or null`
		default:
			if p.Amount == nil {
				c.PriceAmount = ""
			} else {
				c.PriceAmount = strings.TrimSpace(*p.Amount)
			}
			if p.Currency != nil {
				c.Currency = strings.ToUpper(strings.TrimSpace(*p.Currency))
			}
		}
	}
}

// countryHasCities reports whether the country has reference cities.
func (g Geography) countryHasCities(country string) bool {
	for city := range g.CityRegion {
		if g.countryOfCity(city) == country {
			return true
		}
	}
	for city := range g.CityCountry {
		if g.countryOfCity(city) == country {
			return true
		}
	}
	return false
}

// normalize keeps the content consistent after a patch, for the fields the
// patch did not set itself: choosing a short stay makes it a rental priced
// per night, leaving one clears its guest and night numbers, a sale has no
// price period, a new country clears the places of the old one, a city code
// clears free text and a suburb of another city, and free text clears the
// codes. A value the patch sets explicitly is never changed here; a conflict
// between explicit values is reported by validateContent.
func normalize(before Content, c *Content, touched map[string]bool, g Geography) {
	if touched["category"] && c.Category != before.Category {
		if c.Category == CategoryShortStay {
			if !touched["deal"] {
				c.Deal = DealRent
			}
			if !touched["price_period"] {
				c.PricePeriod = PricePeriodNight
			}
		} else {
			if !touched["max_guests"] {
				c.MaxGuests = nil
			}
			if !touched["min_nights"] {
				c.MinNights = nil
			}
			if c.PricePeriod == PricePeriodNight && !touched["price_period"] {
				c.PricePeriod = ""
				if c.Deal == DealRent {
					c.PricePeriod = PricePeriodMonth
				}
			}
		}
	}
	if touched["deal"] && c.Deal != before.Deal && !touched["price_period"] {
		switch {
		case c.Deal == DealRent && c.PricePeriod == "":
			c.PricePeriod = PricePeriodMonth
			if c.Category == CategoryShortStay {
				c.PricePeriod = PricePeriodNight
			}
		case c.Deal != DealRent:
			c.PricePeriod = ""
		}
	}
	if touched["country"] && c.MarketCode != before.MarketCode {
		if !touched["city"] {
			c.City = ""
		}
		if !touched["suburb"] {
			c.Suburb = ""
		}
		if !touched["city_name"] {
			c.CityName = ""
		}
		if !touched["region_name"] {
			c.RegionName = ""
		}
		if info, ok := g.Countries[c.MarketCode]; ok && !touched["price"] && c.PriceAmount == "" && info.Currency != "" {
			c.Currency = info.Currency
		}
	}
	if touched["city"] && c.City != "" {
		if !touched["city_name"] {
			c.CityName = ""
		}
		if !touched["region_name"] {
			c.RegionName = ""
		}
		if !touched["suburb"] && c.Suburb != "" && g.SuburbCity[c.Suburb] != c.City {
			c.Suburb = ""
		}
	}
	if touched["city_name"] && c.CityName != "" {
		if !touched["city"] {
			c.City = ""
		}
		if !touched["suburb"] {
			c.Suburb = ""
		}
	}
}

// validateContent checks the content against the field rules and the
// reference data, and that it is consistent (the database's CHECK
// constraints hold at every save, not only at submission). Fields that are
// simply missing are not errors here: Completeness reports them.
func (s *Service) validateContent(ctx context.Context, c Content, g Geography, fields map[string]string) error {
	for _, t := range []struct {
		field, value string
		multiline    bool
		max          int
	}{
		{"title", c.Title, false, MaxTitleLength}, {"summary", c.Summary, false, MaxSummaryLength},
		{"description", c.Description, true, MaxDescriptionLength},
	} {
		if problem := plainText(t.value, t.multiline, t.max); problem != "" {
			fields[t.field] = problem
		}
	}
	q := Query{Deal: c.Deal, Category: c.Category, PropertyType: c.PropertyType, City: c.City, Suburb: c.Suburb, Features: c.Features}
	codeFields, err := s.CheckCodes(ctx, q)
	if err != nil {
		return err
	}
	for k, v := range codeFields {
		fields[k] = v
	}
	if len(c.Features) > MaxFeatures {
		fields["features"] = fmt.Sprintf("at most %d features", MaxFeatures)
	}

	// Numbers.
	for _, n := range []struct {
		field    string
		value    *int
		min, max int
	}{
		{"bedrooms", c.Bedrooms, 0, MaxRooms}, {"bathrooms", c.Bathrooms, 0, MaxRooms},
		{"floor_area", c.FloorArea, 0, maxArea}, {"land_area", c.LandArea, 0, maxArea},
		{"max_guests", c.MaxGuests, 1, MaxGuestsLimit}, {"min_nights", c.MinNights, 1, MaxMinNights},
	} {
		if n.value != nil && (*n.value < n.min || *n.value > n.max) {
			fields[n.field] = fmt.Sprintf("must be a whole number from %d to %d", n.min, n.max)
		}
	}

	// Video (the database's CHECK constraints of migration 0027).
	if c.YouTubeVideoID != "" && !ValidYouTubeID(c.YouTubeVideoID) {
		fields["youtube_url"] = MsgNotYouTubeVideo
	}
	if s := c.YouTubeStartSeconds; s != nil && (*s < 0 || *s > MaxYouTubeStartSeconds || c.YouTubeVideoID == "") {
		fields["youtube_url"] = MsgNotYouTubeVideo
	}

	// Coordinates (the CHECK constraints of migration 0028, and (0, 0)).
	// Whether they lie inside the listing's country is not checked.
	validateLocation(c, fields)

	// Price.
	if c.PriceAmount != "" {
		amount, err := ParseAmount(c.PriceAmount)
		switch {
		case err != nil:
			fields["price"] = "amount must be a decimal amount such as 385000 or 950.50"
		case !new(big.Rat).Mul(amount, big.NewRat(100, 1)).IsInt():
			fields["price"] = "amount must have at most two decimal places"
		case len(new(big.Int).Quo(amount.Num(), amount.Denom()).String()) > maxPriceDigits:
			fields["price"] = "amount is too large"
		}
	}
	if c.Currency != "" {
		known := false
		for _, info := range g.Countries {
			if info.Currency == c.Currency || slices.Contains(info.Currencies, c.Currency) {
				known = true
				break
			}
		}
		if !currencyRe.MatchString(c.Currency) || !known {
			fields["price"] = "currency must be the ISO 4217 code of a country's currency, such as USD"
		}
	}

	// Consistency of the deal, category, price period and stay numbers.
	switch c.PricePeriod {
	case "", PricePeriodMonth, PricePeriodNight:
	default:
		fields["price_period"] = "must be month, night or null"
	}
	switch {
	case c.Category == CategoryShortStay && c.Deal != "" && c.Deal != DealRent:
		fields["deal"] = "must be rent for a short stay"
	case c.Category == CategoryShortStay && c.PricePeriod != "" && c.PricePeriod != PricePeriodNight:
		fields["price_period"] = "must be night for a short stay"
	case c.Category != CategoryShortStay && c.PricePeriod == PricePeriodNight:
		fields["price_period"] = "only a short stay is priced per night"
	case c.Deal != "" && c.Deal != DealRent && c.PricePeriod != "":
		fields["price_period"] = "must be null for a sale"
	}
	if c.Category != CategoryShortStay {
		for _, f := range []string{"max_guests", "min_nights"} {
			if (f == "max_guests" && c.MaxGuests != nil) || (f == "min_nights" && c.MinNights != nil) {
				fields[f] = "is for short stays only"
			}
		}
	}

	// Place: the country, then a city code where the country has reference
	// cities and free text where it has none.
	if c.MarketCode != "" {
		if _, ok := g.Countries[c.MarketCode]; !countryRe.MatchString(c.MarketCode) || !ok {
			fields["country"] = "is not a known country; see GET /v1/countries"
			return nil
		}
	}
	hasCities := c.MarketCode != "" && g.countryHasCities(c.MarketCode)
	switch {
	case c.City != "" && (c.CityName != "" || c.RegionName != ""):
		fields["city_name"] = "cannot be combined with a city code"
	case c.City != "" && c.MarketCode != "" && g.countryOfCity(c.City) != c.MarketCode && fields["city"] == "":
		fields["city"] = "is not a city of that country"
	case c.City == "" && c.Suburb != "":
		fields["suburb"] = "needs a city"
	case c.CityName != "" && hasCities:
		fields["city_name"] = "this country has reference cities: choose a city code from GET /v1/reference/cities?market=" + c.MarketCode
	}
	if c.Suburb != "" && c.City != "" && fields["suburb"] == "" && g.SuburbCity[c.Suburb] != c.City {
		fields["suburb"] = "is not a suburb of that city"
	}
	for _, f := range []struct{ field, value string }{{"city_name", c.CityName}, {"region_name", c.RegionName}} {
		if problem := CheckPlaceName(f.value); problem != "" && fields[f.field] == "" {
			fields[f.field] = problem
		}
	}
	return nil
}

// Missing is a field a listing still needs before it can be submitted.
type Missing struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Completeness says whether a listing can be submitted for review.
type Completeness struct {
	Ready   bool      `json:"ready"`
	Missing []Missing `json:"missing"`
}

// completeness lists what the content and photos still lack.
func completeness(c Content, photos []StoredPhoto, alts map[string]string, g Geography) Completeness {
	out := Completeness{Missing: []Missing{}}
	need := func(field, message string) {
		out.Missing = append(out.Missing, Missing{Field: field, Message: message})
	}
	if len([]rune(c.Title)) < MinTitleLength {
		need("title", fmt.Sprintf("Add a title of at least %d characters.", MinTitleLength))
	}
	if len([]rune(c.Description)) < MinDescriptionLength {
		need("description", fmt.Sprintf("Add a description of at least %d characters.", MinDescriptionLength))
	}
	if c.Deal == "" {
		need("deal", "Choose whether the property is for sale or to rent.")
	}
	if c.Category == "" {
		need("category", "Choose a category.")
	}
	if c.PropertyType == "" {
		need("property_type", "Choose a property type.")
	}
	if amount, err := ParseAmount(c.PriceAmount); c.PriceAmount == "" || err != nil || amount.Sign() <= 0 {
		need("price", "Add a price.")
	}
	if c.Currency == "" {
		need("price", "Choose the currency of the price.")
	}
	if c.Deal == DealRent && c.PricePeriod == "" {
		need("price_period", "Say whether the rent is per month or per night.")
	}
	switch {
	case c.MarketCode == "":
		need("country", "Choose the country.")
	case g.countryHasCities(c.MarketCode) && c.City == "":
		need("city", "Choose the city.")
	case !g.countryHasCities(c.MarketCode) && c.City == "" && c.CityName == "":
		need("city_name", "Type the city or town.")
	}
	if c.Category == CategoryShortStay && c.MaxGuests == nil {
		need("max_guests", "Say how many guests the place sleeps.")
	}
	// A video tour is optional (BR-013): no video is never missing. Nor are
	// coordinates: without them the listing is just not on the map.
	if len(photos) < MinPhotosToSubmit {
		need("photos", fmt.Sprintf("Add at least %d photo.", MinPhotosToSubmit))
	}
	for _, p := range photos {
		alt := p.Alt
		if a, ok := alts[p.ID.String()]; ok {
			alt = a
		}
		if strings.TrimSpace(alt) == "" {
			need("photos", "Describe every photo (alternative text).")
			break
		}
	}
	out.Ready = len(out.Missing) == 0
	return out
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugFor is a listing's public address: its title in URL form and its
// reference, which makes it unique ("three-bedroom-house-zw-482913").
func slugFor(title, reference string) string {
	base := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(base) > 80 {
		base = strings.Trim(base[:80], "-")
	}
	if base == "" {
		base = "listing"
	}
	suffix := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(strings.TrimPrefix(reference, "RP-")), "-"), "-")
	return base + "-" + suffix
}
