package propertyrequests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maxMessageRunes     = 2000
	maxNameRunes        = 120
	maxEmailLength      = 254
	maxPrivacyVersion   = 32
	minIdempotencyKey   = 8
	maxIdempotencyKey   = 128
	canonicalUUIDLength = 36
)

var (
	// htmlTagRe matches anything that looks like an HTML/XML tag or comment.
	// Plain comparisons such as "3 < 4" are still allowed.
	htmlTagRe = regexp.MustCompile(`<[/!?]?[A-Za-z][^>]*>|<!--`)
	emailRe   = regexp.MustCompile(`^[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>".]+$`)
	e164Re    = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
)

// validated is the normalised form of an Input.
type validated struct {
	listingID      uuid.UUID
	requestType    string
	message        string
	contactMethod  string
	slot           *time.Time
	idempotencyKey string
	name           string
	email          string
	phone          string
	privacyVersion string
	customerUserID *uuid.UUID
	// stay is set for a STAY_REQUEST; its Nights are filled by the Service.
	stay *StayDetails
	// serviceID is the consultation service asked for (API 0.51.0).
	serviceID string
}

// validate normalises in and returns per-field errors (empty when valid).
// The idempotency key in the body wins over the Idempotency-Key header.
// allowed holds the codes accepted right now for the reference-data fields.
type allowed struct {
	requestTypes   []string
	contactMethods []string
}

// defaultAllowed is the built-in vocabulary, used when no Vocabulary is
// configured (unit tests).
// stayDateRe is the shape of check_in and check_out; maxStayGuests bounds guests.
var stayDateRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

const maxStayGuests = 100

func defaultAllowed() allowed {
	return allowed{
		requestTypes:   []string{TypeEnquiry, TypePropertyInformation, TypeViewingRequest, TypeConsultationRequest},
		contactMethods: []string{ContactEmail, ContactPhone, ContactWhatsApp},
	}
}

func validate(in Input, headerKey string, now time.Time, ok allowed) (validated, map[string]string) {
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

	// Listing.
	raw := strings.TrimSpace(in.ListingID)
	if id, err := uuid.Parse(raw); raw == "" {
		fields["listing_id"] = "is required"
	} else if err != nil || len(raw) != canonicalUUIDLength {
		fields["listing_id"] = "must be a UUID"
	} else {
		v.listingID = id
	}

	// Request type.
	v.requestType = in.RequestType
	switch {
	case in.RequestType == "":
		fields["request_type"] = "is required"
	case !slices.Contains(ok.requestTypes, in.RequestType):
		// Unknown, or deactivated in the reference data.
		fields["request_type"] = "must be one of " + strings.Join(ok.requestTypes, ", ")
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

	// Preferred slot.
	if in.ServiceID != nil && strings.TrimSpace(*in.ServiceID) != "" {
		raw := strings.TrimSpace(*in.ServiceID)
		switch {
		case in.RequestType != TypeConsultationRequest:
			fields["service_id"] = "is only allowed for CONSULTATION_REQUEST"
		case in.PreferredSlotUTC == nil || strings.TrimSpace(*in.PreferredSlotUTC) == "":
			fields["service_id"] = "needs preferred_slot_utc"
		case len(raw) != 36:
			fields["service_id"] = "must be a UUID"
		default:
			if _, err := uuid.Parse(raw); err != nil {
				fields["service_id"] = "must be a UUID"
			} else {
				v.serviceID = strings.ToLower(raw)
			}
		}
	}
	if in.PreferredSlotUTC != nil && strings.TrimSpace(*in.PreferredSlotUTC) != "" {
		slotAllowed := in.RequestType == TypeViewingRequest || in.RequestType == TypeConsultationRequest
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*in.PreferredSlotUTC))
		switch {
		case !slotAllowed:
			fields["preferred_slot_utc"] = "is only allowed for VIEWING_REQUEST and CONSULTATION_REQUEST"
		case err != nil:
			fields["preferred_slot_utc"] = "must be an RFC 3339 timestamp"
		case !t.After(now):
			fields["preferred_slot_utc"] = "must be in the future"
		default:
			utc := t.UTC()
			v.slot = &utc
		}
	}

	// A request to stay names its dates and guests; no other type may. Only
	// their shape is checked here: the listing decides the rest (StayChecker).
	checkIn, checkOut := "", ""
	if in.CheckIn != nil {
		checkIn = strings.TrimSpace(*in.CheckIn)
	}
	if in.CheckOut != nil {
		checkOut = strings.TrimSpace(*in.CheckOut)
	}
	if in.RequestType == TypeStayRequest {
		for field, value := range map[string]string{"check_in": checkIn, "check_out": checkOut} {
			switch {
			case value == "":
				fields[field] = "is required for STAY_REQUEST"
			case !stayDateRe.MatchString(value):
				fields[field] = "must be a date such as 2026-12-24"
			}
		}
		switch {
		case in.Guests == nil:
			fields["guests"] = "is required for STAY_REQUEST"
		case *in.Guests < 1 || *in.Guests > maxStayGuests:
			fields["guests"] = "must be a whole number from 1 to 100"
		default:
			v.stay = &StayDetails{CheckIn: checkIn, CheckOut: checkOut, Guests: *in.Guests}
		}
	} else {
		if checkIn != "" {
			fields["check_in"] = "is only allowed for STAY_REQUEST"
		}
		if checkOut != "" {
			fields["check_out"] = "is only allowed for STAY_REQUEST"
		}
		if in.Guests != nil {
			fields["guests"] = "is only allowed for STAY_REQUEST"
		}
	}

	// Contact details. An authenticated customer is identified by the session:
	// name and email come from the account (anything sent in guest_contact for
	// them is ignored) and guest_contact may be omitted; only its phone is read.
	// A guest must supply guest_contact.
	if in.Customer != nil {
		id := in.Customer.UserID
		v.customerUserID = &id
		v.name = strings.TrimSpace(in.Customer.Name)
		v.email = strings.ToLower(strings.TrimSpace(in.Customer.Email))
		if in.GuestContact != nil {
			v.phone = strings.TrimSpace(in.GuestContact.Phone)
		}
		switch {
		case v.phone != "" && !e164Re.MatchString(v.phone):
			fields["guest_contact.phone"] = "must be in E.164 format, for example +263771234567"
		case v.phone == "" && (in.PreferredContactMethod == ContactPhone || in.PreferredContactMethod == ContactWhatsApp):
			fields["guest_contact.phone"] = "is required when the preferred contact method is PHONE or WHATSAPP"
		}
	} else if in.GuestContact == nil {
		fields["guest_contact"] = "is required"
	} else {
		v.name = strings.TrimSpace(in.GuestContact.Name)
		switch n := utf8.RuneCountInString(v.name); {
		case n == 0:
			fields["guest_contact.name"] = "is required"
		case n > maxNameRunes:
			fields["guest_contact.name"] = "must be at most 120 characters"
		case !utf8.ValidString(v.name) || hasControlChars(v.name, false) || htmlTagRe.MatchString(v.name):
			fields["guest_contact.name"] = "contains characters that are not allowed"
		}

		v.email = strings.ToLower(strings.TrimSpace(in.GuestContact.Email))
		switch {
		case v.email == "":
			fields["guest_contact.email"] = "is required"
		case len(v.email) > maxEmailLength || !emailRe.MatchString(v.email):
			fields["guest_contact.email"] = "must be a valid email address"
		}

		v.phone = strings.TrimSpace(in.GuestContact.Phone)
		switch {
		case v.phone != "" && !e164Re.MatchString(v.phone):
			fields["guest_contact.phone"] = "must be in E.164 format, for example +263771234567"
		case v.phone == "" && (in.PreferredContactMethod == ContactPhone || in.PreferredContactMethod == ContactWhatsApp):
			fields["guest_contact.phone"] = "is required when the preferred contact method is PHONE or WHATSAPP"
		}
	}

	// Consent.
	if in.Consent == nil {
		fields["consent"] = "is required"
	} else {
		v.privacyVersion = strings.TrimSpace(in.Consent.PrivacyVersion)
		switch {
		case v.privacyVersion == "":
			fields["consent.privacy_version"] = "is required"
		case len(v.privacyVersion) > maxPrivacyVersion || !isPrintableASCII(v.privacyVersion):
			fields["consent.privacy_version"] = "is not a valid privacy notice version"
		}
		if !in.Consent.Accepted {
			fields["consent.accepted"] = "must be true"
		}
	}

	return v, fields
}

// hash returns the hex SHA-256 of the canonicalised payload. The idempotency
// key itself is excluded: the hash answers "is this the same request?".
// Field order is fixed by the struct, so the encoding is deterministic. The
// customer's user ID is part of the content (omitted for guests, which keeps
// guest hashes unchanged), so one customer can never replay another's key.
func (v validated) hash() string {
	customer := ""
	if v.customerUserID != nil {
		customer = v.customerUserID.String()
	}
	slot := ""
	if v.slot != nil {
		slot = v.slot.Format(time.RFC3339Nano)
	}
	var stay StayDetails
	if v.stay != nil {
		stay = *v.stay
	}
	canonical, _ := json.Marshal(struct {
		ListingID      string `json:"listing_id"`
		RequestType    string `json:"request_type"`
		Message        string `json:"message"`
		ContactMethod  string `json:"preferred_contact_method"`
		Slot           string `json:"preferred_slot_utc"`
		Name           string `json:"guest_name"`
		Email          string `json:"guest_email"`
		Phone          string `json:"guest_phone"`
		PrivacyVersion string `json:"privacy_version"`
		CustomerUserID string `json:"customer_user_id,omitempty"`
		// Omitted for every type but STAY_REQUEST, which keeps the hashes
		// of all other requests as they were.
		CheckIn  string `json:"check_in,omitempty"`
		CheckOut string `json:"check_out,omitempty"`
		Guests   int    `json:"guests,omitempty"`
		// Omitted without a service, which keeps earlier hashes.
		ServiceID string `json:"service_id,omitempty"`
	}{v.listingID.String(), v.requestType, v.message, v.contactMethod, slot, v.name, v.email, v.phone, v.privacyVersion, customer,
		stay.CheckIn, stay.CheckOut, stay.Guests, v.serviceID})
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
