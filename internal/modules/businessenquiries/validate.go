package businessenquiries

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maxMessageRunes   = 2000
	maxServiceRunes   = 120
	maxNameRunes      = 120
	maxEmailLength    = 254
	maxSlugLength     = 80
	maxPrivacyVersion = 32
	minIdempotencyKey = 8
	maxIdempotencyKey = 128
)

var (
	// htmlTagRe matches anything that looks like an HTML/XML tag or comment.
	// Plain comparisons such as "3 < 4" are still allowed.
	htmlTagRe = regexp.MustCompile(`<[/!?]?[A-Za-z][^>]*>|<!--`)
	emailRe   = regexp.MustCompile(`^[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>".]+$`)
	e164Re    = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	slugRe    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	marketRe  = regexp.MustCompile(`^[A-Z]{2}$`)
)

// validated is the normalised form of an Input.
type validated struct {
	kind           string
	slug           string
	service        string
	message        string
	contactMethod  string
	idempotencyKey string
	name           string
	email          string
	phone          string
	privacyVersion string
	customerUserID *uuid.UUID
	// The development project the enquiry is about (API 0.44.0): set when
	// Input.Development was sent. kind and slug stay "" until it is resolved
	// when the customer did not name the recipient.
	development   bool
	devMarket     string
	devSlug       string
	devUnitTypeID *uuid.UUID
}

// validate normalises in and returns per-field errors (empty when valid).
// The idempotency key in the body wins over the Idempotency-Key header.
// allowed holds the codes accepted right now for the reference-data fields.
type allowed struct {
	kinds          []string
	contactMethods []string
}

// defaultAllowed is the built-in vocabulary, used when no Vocabulary is
// configured (unit tests).
func defaultAllowed() allowed {
	return allowed{kinds: Kinds(), contactMethods: []string{ContactEmail, ContactPhone, ContactWhatsApp}}
}

func validate(in Input, headerKey string, ok allowed) (validated, map[string]string) {
	var v validated
	fields := map[string]string{}

	// Idempotency key.
	v.idempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if v.idempotencyKey == "" {
		v.idempotencyKey = strings.TrimSpace(headerKey)
	}
	switch {
	case v.idempotencyKey == "":
		fields["idempotency_key"] = "is required, in the body or the Idempotency-Key header"
	case len(v.idempotencyKey) < minIdempotencyKey || len(v.idempotencyKey) > maxIdempotencyKey:
		fields["idempotency_key"] = "must be between 8 and 128 characters"
	case !isPrintableASCII(v.idempotencyKey):
		fields["idempotency_key"] = "must contain only printable ASCII characters without spaces"
	}

	// Development project (optional): market + slug, and optionally a unit type.
	if in.Development != nil {
		v.development = true
		v.devMarket = strings.ToUpper(strings.TrimSpace(in.Development.Market))
		if !marketRe.MatchString(v.devMarket) {
			fields["development.market"] = "must be a two-letter country code"
		}
		v.devSlug = strings.TrimSpace(in.Development.Slug)
		if v.devSlug == "" || len(v.devSlug) > maxSlugLength || !slugRe.MatchString(v.devSlug) {
			fields["development.slug"] = "is not a valid development address"
		}
		if in.Development.UnitTypeID != nil && strings.TrimSpace(*in.Development.UnitTypeID) != "" {
			id, err := uuid.Parse(strings.TrimSpace(*in.Development.UnitTypeID))
			if err != nil {
				fields["development.unit_type_id"] = "must be the id of one of the development's unit types"
			} else {
				v.devUnitTypeID = &id
			}
		}
	}

	// Recipient: required, unless the enquiry is about a development (its
	// developer is the recipient, resolved by the service).
	switch {
	case in.Recipient == nil && v.development:
	case in.Recipient == nil:
		fields["recipient.kind"] = "is required"
		fields["recipient.slug"] = "is required"
	default:
		v.kind = in.Recipient.Kind
		switch {
		case v.kind == "":
			fields["recipient.kind"] = "is required"
		case !slices.Contains(ok.kinds, v.kind):
			// Unknown, or deactivated in the reference data.
			fields["recipient.kind"] = "must be one of " + strings.Join(ok.kinds, ", ")
		}
		v.slug = strings.TrimSpace(in.Recipient.Slug)
		switch {
		case v.slug == "":
			fields["recipient.slug"] = "is required"
		case len(v.slug) > maxSlugLength || !slugRe.MatchString(v.slug):
			fields["recipient.slug"] = "is not a valid profile address"
		}
	}

	// Service: optional, short plain text.
	if in.Service != nil {
		v.service = strings.TrimSpace(*in.Service)
		switch {
		case utf8.RuneCountInString(v.service) > maxServiceRunes:
			fields["service"] = "must be at most 120 characters"
		case !utf8.ValidString(v.service) || hasControlChars(v.service, false):
			fields["service"] = "contains characters that are not allowed"
		case htmlTagRe.MatchString(v.service):
			fields["service"] = "must be plain text without HTML"
		}
	}

	// Message: plain text only.
	v.message = strings.TrimSpace(strings.ReplaceAll(in.Message, "\r\n", "\n"))
	switch n := utf8.RuneCountInString(v.message); {
	case n == 0:
		fields["message"] = "is required"
	case n > maxMessageRunes:
		fields["message"] = "must be at most 2000 characters"
	case !utf8.ValidString(v.message) || hasControlChars(v.message, true):
		fields["message"] = "contains characters that are not allowed"
	case htmlTagRe.MatchString(v.message):
		fields["message"] = "must be plain text without HTML"
	}

	// Contact method.
	v.contactMethod = in.PreferredContactMethod
	switch {
	case in.PreferredContactMethod == "":
		fields["preferred_contact_method"] = "is required"
	case !slices.Contains(ok.contactMethods, in.PreferredContactMethod):
		fields["preferred_contact_method"] = "must be one of " + strings.Join(ok.contactMethods, ", ")
	}
	needsPhone := in.PreferredContactMethod == ContactPhone || in.PreferredContactMethod == ContactWhatsApp

	// Contact details. An authenticated customer is identified by the session:
	// name and email come from the account (anything sent in guest_contact for
	// them is ignored) and guest_contact may be omitted; only its phone is read.
	// A guest must supply guest_contact with a name and an email.
	if in.Customer != nil {
		id := in.Customer.UserID
		v.customerUserID = &id
		v.name = strings.TrimSpace(in.Customer.Name)
		v.email = strings.ToLower(strings.TrimSpace(in.Customer.Email))
	} else {
		var name, email string
		if in.GuestContact != nil {
			name, email = in.GuestContact.Name, in.GuestContact.Email
		}
		v.name = strings.TrimSpace(name)
		switch n := utf8.RuneCountInString(v.name); {
		case n == 0:
			fields["guest_contact.name"] = "is required"
		case n > maxNameRunes:
			fields["guest_contact.name"] = "must be at most 120 characters"
		case !utf8.ValidString(v.name) || hasControlChars(v.name, false) || htmlTagRe.MatchString(v.name):
			fields["guest_contact.name"] = "contains characters that are not allowed"
		}
		v.email = strings.ToLower(strings.TrimSpace(email))
		switch {
		case v.email == "":
			fields["guest_contact.email"] = "is required"
		case len(v.email) > maxEmailLength || !emailRe.MatchString(v.email):
			fields["guest_contact.email"] = "must be a valid email address"
		}
	}
	if in.GuestContact != nil {
		v.phone = strings.TrimSpace(in.GuestContact.Phone)
	}
	switch {
	case v.phone != "" && !e164Re.MatchString(v.phone):
		fields["guest_contact.phone"] = "must be in E.164 format, for example +263771234567"
	case v.phone == "" && needsPhone:
		fields["guest_contact.phone"] = "is required when the preferred contact method is PHONE or WHATSAPP"
	}

	// Consent.
	if in.Consent == nil || !in.Consent.Accepted {
		fields["consent.accepted"] = "must be true"
	}
	if in.Consent != nil {
		v.privacyVersion = strings.TrimSpace(in.Consent.PrivacyVersion)
		switch {
		case v.privacyVersion == "":
			fields["consent.privacy_version"] = "is required"
		case len(v.privacyVersion) > maxPrivacyVersion || !isPrintableASCII(v.privacyVersion):
			fields["consent.privacy_version"] = "is not a valid privacy notice version"
		}
	}

	return v, fields
}

// hash returns the hex SHA-256 of the canonicalised payload. The idempotency
// key itself is excluded: the hash answers "is this the same enquiry?". The
// customer's user ID is part of the content, so one customer can never replay
// another's key.
func (v validated) hash() string {
	customer := ""
	if v.customerUserID != nil {
		customer = v.customerUserID.String()
	}
	unitType := ""
	if v.devUnitTypeID != nil {
		unitType = v.devUnitTypeID.String()
	}
	canonical, _ := json.Marshal(struct {
		Kind           string `json:"recipient_kind"`
		Slug           string `json:"recipient_slug"`
		Service        string `json:"service"`
		Message        string `json:"message"`
		ContactMethod  string `json:"preferred_contact_method"`
		Name           string `json:"guest_name"`
		Email          string `json:"guest_email"`
		Phone          string `json:"guest_phone"`
		PrivacyVersion string `json:"privacy_version"`
		CustomerUserID string `json:"customer_user_id,omitempty"`
		DevMarket      string `json:"development_market,omitempty"`
		DevSlug        string `json:"development_slug,omitempty"`
		DevUnitType    string `json:"development_unit_type_id,omitempty"`
	}{v.kind, v.slug, v.service, v.message, v.contactMethod, v.name, v.email, v.phone, v.privacyVersion, customer,
		v.devMarket, v.devSlug, unitType})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// hasControlChars reports control characters; newlines and tabs are tolerated
// when allowWhitespace is set (multi-line messages).
func hasControlChars(s string, allowWhitespace bool) bool {
	for _, r := range s {
		if allowWhitespace && (r == '\n' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
