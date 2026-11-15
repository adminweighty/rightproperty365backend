package leases

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	emailRe    = regexp.MustCompile(`^[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>".]+$`)
	amountRe   = regexp.MustCompile(`^\d{1,12}(\.\d{1,2})?$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	phoneRe    = regexp.MustCompile(`^\+?[0-9 ()-]{6,30}$`)
	clauseRe   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,39}$`)
	marketRe   = regexp.MustCompile(`^[A-Z]{2}$`)
)

// cleanText trims a plain-text value and checks it: valid UTF-8, no markup,
// no control characters (line breaks only when multiline), at most max
// characters. It returns the cleaned value and a problem ("" when fine).
func cleanText(raw string, max int, multiline bool) (string, string) {
	v := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	switch {
	case !utf8.ValidString(v):
		return "", "contains characters that are not allowed"
	case utf8.RuneCountInString(v) > max:
		return "", fmt.Sprintf("must be at most %d characters", max)
	case strings.ContainsAny(v, "<>"):
		return "", "must be plain text (no < or >)"
	}
	for _, r := range v {
		if r == '\n' && multiline {
			continue
		}
		if r == '\t' && multiline {
			continue
		}
		if unicode.IsControl(r) {
			return "", "contains characters that are not allowed"
		}
	}
	return v, ""
}

// required is cleanText for a value that must not be empty.
func required(fields map[string]string, key, raw string, max int, multiline bool) string {
	v, problem := cleanText(raw, max, multiline)
	switch {
	case problem != "":
		fields[key] = problem
	case v == "":
		fields[key] = "is required"
	}
	return v
}

func placeholderProblem(text string) string {
	unknown, malformed := UnknownPlaceholders(text)
	switch {
	case len(unknown) > 0:
		quoted := make([]string, 0, len(unknown))
		for _, u := range unknown {
			quoted = append(quoted, "{{"+u+"}}")
		}
		return "uses unknown placeholders: " + strings.Join(quoted, ", ") + " (see the variables list)"
	case malformed:
		return "has a {{ or }} that is not part of a placeholder"
	}
	return ""
}

// TemplateInput is the body of POST and PATCH /v1/admin/lease-templates.
// Nil fields stay (PATCH); on POST market_code, name, lease_kind and
// sections are required.
type TemplateInput struct {
	MarketCode      *string    `json:"market_code"`
	Name            *string    `json:"name"`
	LeaseKind       *string    `json:"lease_kind"`
	Description     *string    `json:"description"`
	Sections        *[]Section `json:"sections"`
	OptionalClauses *[]Clause  `json:"optional_clauses"`
}

// applyTemplateInput writes in onto t and validates the result.
// knownMarket says whether a country code exists.
func applyTemplateInput(t *Template, in TemplateInput, creating bool, knownMarket func(string) bool) error {
	fields := map[string]string{}
	if in.MarketCode != nil {
		code := strings.ToUpper(strings.TrimSpace(*in.MarketCode))
		switch {
		case !creating && code != t.MarketCode:
			fields["market_code"] = "cannot change: a template is for one country; create a new template for another"
		case !marketRe.MatchString(code) || !knownMarket(code):
			fields["market_code"] = "must be a known country code (ISO 3166-1 alpha-2)"
		default:
			t.MarketCode = code
		}
	} else if creating {
		fields["market_code"] = "is required"
	}
	if in.Name != nil {
		t.Name = required(fields, "name", *in.Name, MaxTemplateName, false)
	} else if creating {
		fields["name"] = "is required"
	}
	if in.LeaseKind != nil {
		if *in.LeaseKind != KindResidential && *in.LeaseKind != KindCommercial {
			fields["lease_kind"] = "must be RESIDENTIAL or COMMERCIAL"
		} else {
			t.LeaseKind = *in.LeaseKind
		}
	} else if creating {
		fields["lease_kind"] = "is required"
	}
	if in.Description != nil {
		v, problem := cleanText(*in.Description, 1000, true)
		if problem != "" {
			fields["description"] = problem
		}
		t.Description = v
	}
	if in.Sections != nil {
		sections := *in.Sections
		if len(sections) == 0 || len(sections) > MaxSections {
			fields["sections"] = fmt.Sprintf("must have from 1 to %d sections", MaxSections)
		}
		out := make([]Section, 0, len(sections))
		for i, s := range sections {
			key := "sections[" + strconv.Itoa(i) + "]"
			heading := required(fields, key+".heading", s.Heading, MaxHeading, false)
			text := required(fields, key+".text", s.Text, MaxSectionText, true)
			if p := placeholderProblem(heading); p != "" {
				fields[key+".heading"] = p
			}
			if p := placeholderProblem(text); p != "" {
				fields[key+".text"] = p
			}
			out = append(out, Section{Heading: heading, Text: text})
		}
		t.Sections = out
	} else if creating {
		fields["sections"] = "is required"
	}
	if in.OptionalClauses != nil {
		clauses := *in.OptionalClauses
		if len(clauses) > MaxClauses {
			fields["optional_clauses"] = fmt.Sprintf("must have at most %d clauses", MaxClauses)
		}
		out := make([]Clause, 0, len(clauses))
		var codes []string
		for i, c := range clauses {
			key := "optional_clauses[" + strconv.Itoa(i) + "]"
			code := strings.TrimSpace(c.Code)
			switch {
			case !clauseRe.MatchString(code):
				fields[key+".code"] = "must be 2 to 40 characters: capital letters, digits and _ (starting with a letter)"
			case slices.Contains(codes, code):
				fields[key+".code"] = "is used by another clause"
			}
			codes = append(codes, code)
			title := required(fields, key+".title", c.Title, MaxHeading, false)
			text := required(fields, key+".text", c.Text, MaxSectionText, true)
			if p := placeholderProblem(text); p != "" {
				fields[key+".text"] = p
			}
			out = append(out, Clause{Code: code, Title: title, Text: text})
		}
		t.Clauses = out
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

// checkLegalReview validates the legal review of an approval.
func checkLegalReview(in LegalReview, today time.Time) (LegalReview, error) {
	fields := map[string]string{}
	out := LegalReview{}
	out.ReviewerName = required(fields, "legal_review.reviewer_name", in.ReviewerName, MaxNameRunes, false)
	out.Organisation = required(fields, "legal_review.organisation", in.Organisation, 200, false)
	if d, err := time.Parse(time.DateOnly, strings.TrimSpace(in.ReviewedOn)); err != nil {
		fields["legal_review.reviewed_on"] = "must be a date (YYYY-MM-DD)"
	} else if d.After(today) {
		fields["legal_review.reviewed_on"] = "cannot be in the future"
	} else {
		out.ReviewedOn = d.Format(time.DateOnly)
	}
	note, problem := cleanText(in.Note, 2000, true)
	if problem != "" {
		fields["legal_review.note"] = problem
	}
	out.Note = note
	if len(fields) > 0 {
		return LegalReview{}, &ValidationError{Fields: fields}
	}
	return out, nil
}

// LeaseInput is the body of POST and PATCH /v1/me/leases. Nil fields stay
// (PATCH); listing_id and organisation_id are only read on POST.
type LeaseInput struct {
	TemplateID        *string   `json:"template_id"`
	ListingID         *string   `json:"listing_id"`
	OrganisationID    *string   `json:"organisation_id"`
	PropertyAddress   *string   `json:"property_address"`
	Landlords         *[]Party  `json:"landlords"`
	Tenants           *[]Party  `json:"tenants"`
	StartDate         *string   `json:"start_date"`
	EndDate           *string   `json:"end_date"`
	Periodic          *bool     `json:"periodic"`
	RentAmount        *string   `json:"rent_amount"`
	RentCurrency      *string   `json:"rent_currency"`
	RentPeriod        *string   `json:"rent_period"`
	PaymentDay        *int      `json:"payment_day"`
	DepositAmount     *string   `json:"deposit_amount"`
	NoticePeriodDays  *int      `json:"notice_period_days"`
	OptionalClauses   *[]string `json:"optional_clauses"`
	SpecialConditions *string   `json:"special_conditions"`
}

// merge writes the fields that in sets onto base (template, listing and
// organisation are handled by the service).
func (in LeaseInput) merge(base Input) Input {
	out := base
	if in.PropertyAddress != nil {
		out.PropertyAddress = *in.PropertyAddress
	}
	if in.Landlords != nil {
		out.Landlords = append([]Party{}, (*in.Landlords)...)
	}
	if in.Tenants != nil {
		out.Tenants = append([]Party{}, (*in.Tenants)...)
	}
	if in.StartDate != nil {
		out.StartDate = *in.StartDate
	}
	if in.EndDate != nil {
		out.EndDate = *in.EndDate
	}
	if in.Periodic != nil {
		out.Periodic = *in.Periodic
	}
	if in.RentAmount != nil {
		out.RentAmount = *in.RentAmount
	}
	if in.RentCurrency != nil {
		out.RentCurrency = *in.RentCurrency
	}
	if in.RentPeriod != nil {
		out.RentPeriod = *in.RentPeriod
	}
	if in.PaymentDay != nil {
		out.PaymentDay = *in.PaymentDay
	}
	if in.DepositAmount != nil {
		out.DepositAmount = *in.DepositAmount
	}
	if in.NoticePeriodDays != nil {
		out.NoticePeriodDays = *in.NoticePeriodDays
	}
	if in.OptionalClauses != nil {
		out.OptionalClauses = append([]string{}, (*in.OptionalClauses)...)
	}
	if in.SpecialConditions != nil {
		out.SpecialConditions = *in.SpecialConditions
	}
	return out
}

func checkAmount(fields map[string]string, key, raw string, requiredValue bool) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		if requiredValue {
			fields[key] = "is required"
		}
		return ""
	}
	if !amountRe.MatchString(v) {
		fields[key] = "must be a decimal number with at most two decimals, for example 950.00"
		return v
	}
	if f, err := strconv.ParseFloat(v, 64); err != nil || f <= 0 {
		fields[key] = "must be more than 0"
	}
	return v
}

func checkParties(fields map[string]string, key string, parties []Party, seen map[string]string) []Party {
	if len(parties) == 0 || len(parties) > MaxParties {
		fields[key] = fmt.Sprintf("must list from 1 to %d people", MaxParties)
	}
	out := make([]Party, 0, len(parties))
	for i, p := range parties {
		k := key + "[" + strconv.Itoa(i) + "]"
		name := required(fields, k+".name", p.Name, MaxNameRunes, false)
		email := strings.ToLower(strings.TrimSpace(p.Email))
		switch {
		case email == "":
			fields[k+".email"] = "is required"
		case len(email) > 254 || !emailRe.MatchString(email):
			fields[k+".email"] = "must be a valid email address"
		case seen[email] != "":
			fields[k+".email"] = "is already used by " + seen[email] + ": every party needs their own email address to sign"
		default:
			seen[email] = k
		}
		phone := strings.TrimSpace(p.Phone)
		if phone != "" && !phoneRe.MatchString(phone) {
			fields[k+".phone"] = "must be a phone number (digits, spaces, + ( ) -)"
		}
		out = append(out, Party{Name: name, Email: email, Phone: phone})
	}
	return out
}

// checkInput validates and cleans a complete lease input against its
// template. Every mandatory field is checked on every save, so a draft is
// always complete enough to render and send.
func checkInput(in Input, t Template) (Input, map[string]string) {
	fields := map[string]string{}
	out := in
	out.PropertyAddress = required(fields, "property_address", in.PropertyAddress, MaxAddressRunes, true)
	seen := map[string]string{}
	out.Landlords = checkParties(fields, "landlords", in.Landlords, seen)
	out.Tenants = checkParties(fields, "tenants", in.Tenants, seen)

	start, err := time.Parse(time.DateOnly, strings.TrimSpace(in.StartDate))
	if err != nil {
		fields["start_date"] = "must be a date (YYYY-MM-DD)"
	} else {
		out.StartDate = start.Format(time.DateOnly)
	}
	out.EndDate = strings.TrimSpace(in.EndDate)
	switch {
	case in.Periodic && out.EndDate != "":
		fields["end_date"] = "must be empty for a periodic lease"
	case !in.Periodic && out.EndDate == "":
		fields["end_date"] = "is required (or set periodic to true)"
	case !in.Periodic:
		end, err := time.Parse(time.DateOnly, out.EndDate)
		if err != nil {
			fields["end_date"] = "must be a date (YYYY-MM-DD)"
		} else if fields["start_date"] == "" && !end.After(start) {
			fields["end_date"] = "must be after start_date"
		}
	}

	out.RentAmount = checkAmount(fields, "rent_amount", in.RentAmount, true)
	out.RentCurrency = strings.ToUpper(strings.TrimSpace(in.RentCurrency))
	if !currencyRe.MatchString(out.RentCurrency) {
		fields["rent_currency"] = "must be a currency code (ISO 4217), for example USD"
	}
	switch in.RentPeriod {
	case PeriodMonthly, PeriodAnnually:
		if in.PaymentDay < 1 || in.PaymentDay > 31 {
			fields["payment_day"] = "must be a day of the month, from 1 to 31"
		}
	case PeriodWeekly:
		if in.PaymentDay < 1 || in.PaymentDay > 7 {
			fields["payment_day"] = "must be a day of the week, from 1 (Monday) to 7 (Sunday)"
		}
	default:
		fields["rent_period"] = "must be MONTHLY, WEEKLY or ANNUALLY"
	}
	out.DepositAmount = checkAmount(fields, "deposit_amount", in.DepositAmount, false)
	if in.NoticePeriodDays < 1 || in.NoticePeriodDays > MaxNoticeDays {
		fields["notice_period_days"] = fmt.Sprintf("must be a whole number of days from 1 to %d", MaxNoticeDays)
	}
	out.OptionalClauses = []string{}
	for i, code := range in.OptionalClauses {
		key := "optional_clauses[" + strconv.Itoa(i) + "]"
		switch {
		case slices.Contains(out.OptionalClauses, code):
			fields[key] = "is listed twice"
		default:
			if _, ok := t.Clause(code); !ok {
				fields[key] = "is not an optional clause of this template"
			}
		}
		out.OptionalClauses = append(out.OptionalClauses, code)
	}
	special, problem := cleanText(in.SpecialConditions, MaxSpecialRunes, true)
	if problem != "" {
		fields["special_conditions"] = problem
	}
	out.SpecialConditions = special
	return out, fields
}

// SignInput is the body of POST /v1/signing/{token}/sign.
type SignInput struct {
	// Code may be a JSON string or number (identity.ParseCodeInput).
	Code           json.RawMessage `json:"code"`
	SignatureType  string          `json:"signature_type"`
	TypedName      string          `json:"typed_name"`
	DrawnPNGBase64 string          `json:"drawn_png_base64"`
	Consent        bool            `json:"consent"`
}
