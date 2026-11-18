package venues

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// BookingNotice is what the booking emails need, read at send time (outbox
// payloads carry identifiers only).
type BookingNotice struct {
	Booking Booking
	// Who hosts it: the organisation (and the listing's assigned agent), or
	// the direct owner.
	OrganisationID *uuid.UUID
	AgentUserID    *uuid.UUID
	OwnerUserID    *uuid.UUID
}

// Notice loads the data behind a venue booking outbox event.
func (s *Service) Notice(ctx context.Context, id uuid.UUID) (BookingNotice, error) {
	b, err := s.d.Store.Booking(ctx, id)
	if err != nil {
		return BookingNotice{}, err
	}
	n := BookingNotice{Booking: b, OrganisationID: b.OrganisationID, OwnerUserID: b.SellerUserID}
	if s.d.Listings != nil {
		l, err := s.d.Listings.Listing(ctx, b.ListingID)
		switch {
		case err == nil:
			n.AgentUserID = l.AgentUserID
		case !errors.Is(err, ErrListingNotFound):
			return BookingNotice{}, err
		}
	}
	return n, nil
}
