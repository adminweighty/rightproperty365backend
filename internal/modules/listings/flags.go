package listings

// Duplicate and spam flags for the moderators (API 0.54.0, BR-018). They
// are computed when a moderator opens a listing or the review queue, from
// the listing's working content, and never block anything.

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Flag codes.
const (
	FlagPossibleDuplicate = "POSSIBLE_DUPLICATE"
	FlagContactInText     = "CONTACT_IN_TEXT"
	FlagBurst             = "BURST"
	FlagShouting          = "SHOUTING"

	// BurstLimit is how many listings one advertiser may create in BurstWindow
	// before the BURST flag shows.
	BurstLimit  = 10
	BurstWindow = 24 * time.Hour
)

// Flag is one moderation hint.
type Flag struct {
	Code           string  `json:"code"`
	Text           string  `json:"text"`
	OtherReference *string `json:"other_reference,omitempty"`
}

// SimilarQuery looks for other listings that may be the same property.
type SimilarQuery struct {
	ExceptID   uuid.UUID
	MarketCode string
	// Title is compared without case and extra spaces.
	Title string
	// The second rule: the same price, city, suburb and bedrooms.
	PriceAmount string
	Currency    string
	City        string
	Suburb      string
	Bedrooms    *int
}

// FlagStore answers the questions behind the flags. Archived, rejected and
// closed listings are left out of the comparison.
type FlagStore interface {
	// SimilarListings returns up to 3 references of other listings matching
	// the query's title, or its price, city, suburb and bedrooms.
	SimilarListings(ctx context.Context, q SimilarQuery) ([]string, error)
	// CreatedSince counts the listings created since `since` by the
	// organisation, or (without one) by the direct seller.
	CreatedSince(ctx context.Context, organisationID, sellerUserID *uuid.UUID, since time.Time) (int, error)
}

var (
	emailInText = regexp.MustCompile(`(?i)[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`)
	linkInText  = regexp.MustCompile(`(?i)\b(https?://|www\.)\S+`)
	phoneInText = regexp.MustCompile(`\+?\d[\d\s().-]{7,}\d`)
)

func normalTitle(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// shouting reports whether a title is mostly capitals (at least 10 letters).
func shouting(title string) bool {
	letters, upper := 0, 0
	for _, r := range title {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	return letters >= 10 && upper*10 >= letters*8
}

// textFlags are the flags read from the content alone.
func textFlags(c Content) []Flag {
	out := []Flag{}
	text := c.Title + "\n" + c.Summary + "\n" + c.Description
	if emailInText.MatchString(text) || linkInText.MatchString(text) || phoneInText.MatchString(text) {
		out = append(out, Flag{Code: FlagContactInText, Text: "The text has an email address, a phone number or a link: contact should go through the enquiry form."})
	}
	if shouting(c.Title) {
		out = append(out, Flag{Code: FlagShouting, Text: "The title is mostly in capitals."})
	}
	return out
}

// flagsOf computes a listing's flags (empty when the store cannot answer).
func (mg *Manager) flagsOf(ctx context.Context, m ManagedListing) []Flag {
	c := m.WorkingContent()
	out := textFlags(c)
	fs, ok := mg.store.(FlagStore)
	if !ok {
		return out
	}
	refs, err := fs.SimilarListings(ctx, SimilarQuery{ExceptID: m.ID, MarketCode: m.MarketCode, Title: normalTitle(c.Title),
		PriceAmount: c.PriceAmount, Currency: c.Currency, City: c.City, Suburb: c.Suburb, Bedrooms: c.Bedrooms})
	if err != nil {
		mg.logger.WarnContext(ctx, "duplicate check failed", "error", err.Error())
	}
	for _, ref := range refs {
		r := ref
		out = append(out, Flag{Code: FlagPossibleDuplicate, Text: "Another listing has the same title, or the same price, place and bedrooms.", OtherReference: &r})
	}
	if n, err := fs.CreatedSince(ctx, m.OrganisationID, m.SellerUserID, mg.clock().Add(-BurstWindow)); err == nil && n > BurstLimit {
		out = append(out, Flag{Code: FlagBurst, Text: "The advertiser created more than 10 listings in the last 24 hours."})
	}
	return out
}
