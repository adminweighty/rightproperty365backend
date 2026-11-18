package listings

import (
	"context"

	"github.com/google/uuid"
)

// CardState is a listing as another module's list of listings needs it (a
// customer's saved listings): whether it is public right now, its status as
// the advertiser would see it, and its public form.
type CardState struct {
	// Public is Record.Public at the service's clock: PUBLISHED, unexpired
	// and from a verified advertiser.
	Public bool
	// Status is Record.EffectiveStatus (EXPIRED for a PUBLISHED listing whose
	// publication period is over).
	Status string
	// Listing is the public form with the current labels. It is filled
	// whatever the status; a caller shows it only while Public is true.
	Listing Listing
}

// CardStates returns the listings among ids the catalogue knows, WHATEVER
// THEIR STATUS, keyed by ID. An unknown ID is simply absent. Availability
// summaries are not loaded.
func (s *Service) CardStates(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]CardState, error) {
	out := map[uuid.UUID]CardState{}
	if len(ids) == 0 {
		return out, nil
	}
	records, err := s.store.ByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	labels, err := s.labelSets(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	for _, r := range records {
		out[r.ID] = CardState{Public: r.Public(now), Status: r.EffectiveStatus(now), Listing: present(r, labels)}
	}
	return out, nil
}
