package applications

import (
	"context"

	"github.com/google/uuid"
)

// Notice is what the application emails need, read at send time (outbox
// payloads carry IDs only).
type Notice struct {
	Reference        string
	Status           string
	ListingID        uuid.UUID
	ListingTitle     string
	ListingReference string
	// The applicant (nil after account deletion: nobody to write to).
	ApplicantUserID *uuid.UUID
	ApplicantName   string
	ApplicantEmail  string
	// Who receives it: the listing's organisation (and its assigned agent),
	// or its direct owner.
	OrganisationID *uuid.UUID
	AgentUserID    *uuid.UUID
	OwnerUserID    *uuid.UUID
	// InfoMessage is the latest request for more information.
	InfoMessage string
}

// Notice loads the data behind an application outbox event.
func (s *Service) Notice(ctx context.Context, id uuid.UUID) (Notice, error) {
	a, err := s.store.Application(ctx, id)
	if err != nil {
		return Notice{}, err
	}
	n := Notice{Reference: a.Reference, Status: a.Status, ListingID: a.ListingID, ListingTitle: a.Listing.Title, ListingReference: a.Listing.Reference,
		ApplicantUserID: a.ApplicantUserID, ApplicantName: a.ApplicantName, OrganisationID: a.OrganisationID, OwnerUserID: a.OwnerUserID}
	known, err := s.listings.Listings(ctx, []uuid.UUID{a.ListingID})
	if err != nil {
		return Notice{}, err
	}
	if l, ok := known[a.ListingID]; ok {
		n.ListingTitle, n.ListingReference, n.AgentUserID = l.Title, l.Reference, l.AgentUserID
	}
	if a.ApplicantUserID != nil && s.people != nil {
		name, email, ok, err := s.people(ctx, *a.ApplicantUserID)
		if err != nil {
			return Notice{}, err
		}
		if ok {
			n.ApplicantName, n.ApplicantEmail = name, email
		}
	}
	events, err := s.store.Events(ctx, a.ID)
	if err != nil {
		return Notice{}, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == EventInfoRequested {
			n.InfoMessage = events[i].Message
			break
		}
	}
	return n, nil
}
