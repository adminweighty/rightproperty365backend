package appointments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReleaseNote is the timeline note of an appointment that went back to the
// organisation because its assigned member left the team.
const ReleaseNote = "Returned to the organisation: the assigned member left the team"

// ReleaseMember gives the organisation's open appointments assigned to the
// user back to the organisation (nobody assigned), each with an UNASSIGNED
// timeline entry, and returns how many there were. No email is sent. It is
// the in-memory half of removing or suspending a member.
//
// The overlap rule (one professional, no two overlapping CONFIRMED
// appointments) is checked when an appointment is confirmed or assigned; an
// appointment released here keeps its time even when the organisation
// already has another one then, and a manager reassigns it.
func (s *MemoryStore) ReleaseMember(organisationID, userID, actorID uuid.UUID, at time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range s.order {
		a := s.byID[id]
		if !a.Open() || a.OrganisationID == nil || *a.OrganisationID != organisationID || a.AssignedUserID == nil || *a.AssignedUserID != userID {
			continue
		}
		a.AssignedUserID, a.RecipientType, a.UpdatedAt = nil, RecipientOrganisation, at
		s.byID[id] = a
		actor := actorID
		s.timeline = append(s.timeline, TimelineEntry{
			ID: uuid.New(), AppointmentID: id, Event: TimelineUnassigned, ActorType: PartyProfessional, ActorUserID: &actor,
			FromStatus: a.Status, ToStatus: a.Status, Note: ReleaseNote, At: at,
		})
		n++
	}
	return n
}

// ReleaseMemberTx is ReleaseMember in the caller's Postgres transaction.
func ReleaseMemberTx(ctx context.Context, tx pgx.Tx, organisationID, userID, actorID uuid.UUID, at time.Time) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		WITH released AS (
			UPDATE appointments SET recipient_type = 'ORGANISATION', recipient_user_id = NULL, updated_at = $3
			WHERE recipient_organisation_id = $1 AND recipient_user_id = $2
			  AND status IN ('REQUESTED', 'CONFIRMED', 'RESCHEDULE_REQUESTED')
			RETURNING id, status
		), entries AS (
			INSERT INTO appointment_timeline (id, appointment_id, event, actor_type, actor_user_id, from_status, to_status, note, created_at)
			SELECT gen_random_uuid(), r.id, $4, $5, $6, r.status, r.status, $7, $3 FROM released r
			RETURNING 1
		)
		SELECT count(*) FROM entries`,
		organisationID, userID, at, TimelineUnassigned, PartyProfessional, actorID, ReleaseNote).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("release the member's appointments: %w", err)
	}
	return n, nil
}
