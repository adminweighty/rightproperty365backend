package listings

import (
	"context"

	"github.com/google/uuid"
)

// What other modules reuse of the listing rules (the development showcase,
// API 0.44.0): the coordinates (migration 0028), the YouTube tour (0027),
// the plain-text rule and the public listing cards.

// PublicLocationOf is the location the public sees of stored coordinates
// (nil without them): rounded to ApproximateDecimals when APPROXIMATE.
func PublicLocationOf(lat, lon *float64, precision string) *Location {
	return publicLocation(lat, lon, precision)
}

// CheckCoordinates validates coordinates and their precision exactly as a
// listing's (both or neither, ranges, six decimals, not 0,0), adding field
// messages under "latitude", "longitude" and "location_precision".
func CheckCoordinates(lat, lon *float64, precision string, fields map[string]string) {
	validateLocation(Content{Latitude: lat, Longitude: lon, LocationPrecision: precision}, fields)
}

// VideoOf is the public video of a YouTube id (nil when it is not one).
func VideoOf(id string, start *int) *Video { return videoOf(id, start) }

// PlainText checks free text as a listing's: no markup, no control
// characters (line breaks and tabs only when multiline), at most maxLength
// characters. It returns "" when the text is fine.
func PlainText(s string, multiline bool, maxLength int) string {
	return plainText(s, multiline, maxLength)
}

// PublicByIDs returns the public cards of those listings that are public
// right now (PUBLISHED, unexpired, from a verified advertiser: Record.Public),
// in no particular order.
func (s *Service) PublicByIDs(ctx context.Context, ids []uuid.UUID) ([]Listing, error) {
	if len(ids) == 0 {
		return []Listing{}, nil
	}
	now := s.now().UTC()
	records, err := s.store.PublishedByIDs(ctx, ids, now)
	if err != nil {
		return nil, err
	}
	public := records[:0]
	for _, r := range records {
		if r.Public(now) {
			public = append(public, r)
		}
	}
	return s.Present(ctx, public)
}
