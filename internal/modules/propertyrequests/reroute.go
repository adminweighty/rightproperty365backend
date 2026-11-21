package propertyrequests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Reassigning a routing exception (API 0.49.0, FR-061, FSD 2B): a request
// nobody could receive is given by a platform administrator to an
// organisation of its market (its inbox), or to one of its members who
// holds leads. The request becomes RECEIVED, the exception is resolved, the
// organisation's audit records it, and the receiving side gets the usual
// new-request email and in-app notice (PROPERTY_REQUEST_REROUTED). The
// customer is not emailed again: they were told the request was received.

// EventRerouted is a routing exception given to an organisation.
const EventRerouted = "PROPERTY_REQUEST_REROUTED"

// TimelineRerouted is the timeline entry of a reassignment.
const TimelineRerouted = "REROUTED"

// RouteTargets checks where a routing exception may go.
type RouteTargets interface {
	// Eligible reports whether the organisation may receive requests of the
	// market (it is in that market and has at least one ACTIVE member whose
	// role holds leads) and, when userID is set, whether that user is one of
	// them (their name is returned). reason says why not.
	Eligible(ctx context.Context, organisationID, marketID uuid.UUID, userID *uuid.UUID) (userName string, ok bool, reason string, err error)
	// Candidates lists the organisations of the market that may receive
	// requests, with their members who hold leads.
	Candidates(ctx context.Context, marketID uuid.UUID) ([]RouteCandidate, error)
}

// RouteCandidate is an organisation a routing exception may be given to.
type RouteCandidate struct {
	OrganisationID uuid.UUID         `json:"organisation_id"`
	Name           string            `json:"name"`
	Kind           string            `json:"kind"`
	Members        []CandidateMember `json:"members"`
}

// CandidateMember is a member a routing exception may be given to.
type CandidateMember struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
	Role   string    `json:"role"`
}

// Candidates lists where an open routing exception may be given.
func (s *Service) Candidates(ctx context.Context, marketIDs []uuid.UUID, id uuid.UUID) ([]RouteCandidate, error) {
	if s.routeTargets == nil {
		return nil, errors.New("rerouting is not configured")
	}
	cur, err := s.store.GetRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != StatusRoutingException || (marketIDs != nil && !slices.Contains(marketIDs, cur.MarketID)) {
		return nil, ErrNotFound
	}
	return s.routeTargets.Candidates(ctx, cur.MarketID)
}

// WithRouteTargets enables the reassignment of routing exceptions.
func WithRouteTargets(t RouteTargets) Option { return func(s *Service) { s.routeTargets = t } }

// Reroute is the reassignment of a routing exception.
type Reroute struct {
	OrganisationID uuid.UUID
	UserID         *uuid.UUID
	UserName       string
	Actor          Actor
	Note           string
	At             time.Time
	Audit          orgaudit.Event
}

func reroutedPayload(r StoredRequest, rr Reroute) map[string]any {
	payload := map[string]any{"request_id": r.ID, "reference": r.Reference, "organisation_id": rr.OrganisationID,
		"actor_user_id": rr.Actor.UserID, "occurred_at": rr.At}
	if rr.UserID != nil {
		payload["assignee_user_id"] = *rr.UserID
	}
	return payload
}

// RerouteException gives a routing exception to an organisation of its
// market, or to one of its members who holds leads.
func (s *Service) RerouteException(ctx context.Context, marketIDs []uuid.UUID, actor Actor, id, organisationID uuid.UUID, userID *uuid.UUID, rawNote string) (StoredRequest, error) {
	if s.routeTargets == nil {
		return StoredRequest{}, errors.New("rerouting is not configured")
	}
	note := ""
	if strings.TrimSpace(rawNote) != "" {
		var problem string
		if note, problem = cleanText(rawNote, MaxNoteLength); problem != "" {
			return StoredRequest{}, &ValidationError{Fields: map[string]string{"note": problem}}
		}
	}
	cur, err := s.store.GetRequest(ctx, id)
	if err != nil {
		return StoredRequest{}, err
	}
	visible := func(r StoredRequest) error {
		switch {
		case marketIDs != nil && !slices.Contains(marketIDs, r.MarketID):
			return ErrNotFound
		case r.ResolvedAt != nil:
			return ErrAlreadyResolved // resolved, or rerouted (RECEIVED now)
		case r.Status != StatusRoutingException:
			return ErrNotFound
		}
		return nil
	}
	if err := visible(cur); err != nil {
		return StoredRequest{}, err
	}
	name, ok, reason, err := s.routeTargets.Eligible(ctx, organisationID, cur.MarketID, userID)
	if err != nil {
		return StoredRequest{}, fmt.Errorf("check route target: %w", err)
	}
	if !ok {
		field := "organisation_id"
		if userID != nil && strings.Contains(reason, "member") {
			field = "user_id"
		}
		return StoredRequest{}, &ValidationError{Fields: map[string]string{field: reason}}
	}
	now := s.clock()
	after := map[string]any{"routed_to_organisation": organisationID.String()}
	if userID != nil {
		after["assigned_to"], after["assigned_to_name"] = userID.String(), name
	}
	rr := Reroute{OrganisationID: organisationID, UserID: userID, UserName: name, Actor: actor, Note: note, At: now,
		Audit: orgaudit.New(organisationID, actor.UserID, actor.Name, orgaudit.ActionRequestAssigned, orgaudit.TargetPropertyRequest, cur.ID,
			cur.Reference, map[string]any{"routing": "exception"}, after, now)}
	return s.store.RerouteException(ctx, id, rr, visible)
}

// ---------------------------------------------------------------------------
// Stores
// ---------------------------------------------------------------------------

// RerouteException implements Store.
func (s *MemoryStore) RerouteException(_ context.Context, id uuid.UUID, rr Reroute, allow func(StoredRequest) error) (StoredRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.find(id)
	if !ok {
		return StoredRequest{}, ErrNotFound
	}
	cur := s.stored(req)
	if err := allow(cur); err != nil {
		return StoredRequest{}, err
	}
	w := s.workOf(req)
	org := rr.OrganisationID
	rec := Recipient{Type: RecipientOrganisation, OrganisationID: &org}
	if rr.UserID != nil {
		u := *rr.UserID
		rec.Type, rec.UserID = RecipientAgent, &u
	}
	at, by := rr.At, rr.Actor.UserID
	w.Recipient, w.Status = &rec, StatusReceived
	w.ResolvedAt, w.ResolvedBy, w.ResolutionNote, w.UpdatedAt = &at, &by, rr.Note, rr.At
	s.timeline = append(s.timeline, MemoryTimelineEntry{RequestID: id, EventType: TimelineRerouted, ActorType: ActorAdmin, CreatedAt: rr.At,
		ActorUserID: &by, ActorName: rr.Actor.Name, AssigneeName: rr.UserName})
	if rr.UserID != nil && cur.Stage == StageNewEnquiry {
		w.Stage, w.StageChangedAt, w.StageChangedBy = StageAssigned, &at, &by
	}
	if err := s.record(EventExceptionResolved, id, resolvedEventPayload(cur, Resolution{Actor: rr.Actor, Note: rr.Note, At: rr.At}), rr.At); err != nil {
		return StoredRequest{}, err
	}
	if err := s.record(EventRerouted, id, reroutedPayload(cur, rr), rr.At); err != nil {
		return StoredRequest{}, err
	}
	if s.audit != nil {
		s.audit(rr.Audit)
	}
	return s.stored(req), nil
}

// RerouteException implements Store: the recipient, the RECEIVED status, the
// resolution, the timeline, the events and the organisation audit, together.
func (s *PostgresStore) RerouteException(ctx context.Context, id uuid.UUID, rr Reroute, allow func(StoredRequest) error) (StoredRequest, error) {
	err := s.locked(ctx, id, allow, func(tx pgx.Tx, cur StoredRequest) error {
		recipientType := RecipientOrganisation
		if rr.UserID != nil {
			recipientType = RecipientAgent
		}
		var note *string
		if rr.Note != "" {
			note = &rr.Note
		}
		if _, err := tx.Exec(ctx, `
			UPDATE property_requests SET status = 'RECEIVED', recipient_type = $2, recipient_organisation_id = $3, recipient_user_id = $4,
				exception_resolved_at = $5, exception_resolved_by = $6, exception_resolution_note = $7, updated_at = $5
			WHERE id = $1`, id, recipientType, rr.OrganisationID, rr.UserID, rr.At, rr.Actor.UserID, note); err != nil {
			return fmt.Errorf("reroute request: %w", err)
		}
		detail := map[string]any{"organisation_id": rr.OrganisationID.String()}
		if rr.UserID != nil {
			detail["assignee_user_id"], detail["assignee_name"] = rr.UserID.String(), rr.UserName
		}
		if err := insertTimeline(ctx, tx, id, TimelineRerouted, ActorAdmin, rr.Actor, detail, rr.At); err != nil {
			return err
		}
		if rr.UserID != nil && cur.Stage == StageNewEnquiry {
			if _, err := tx.Exec(ctx, `UPDATE property_requests SET stage = 'ASSIGNED', stage_changed_at = $2, stage_changed_by = $3 WHERE id = $1`,
				id, rr.At, rr.Actor.UserID); err != nil {
				return fmt.Errorf("update stage: %w", err)
			}
		}
		if err := insertOutbox(ctx, tx, EventExceptionResolved, id, resolvedEventPayload(cur, Resolution{Actor: rr.Actor, Note: rr.Note, At: rr.At}), rr.At); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, EventRerouted, id, reroutedPayload(cur, rr), rr.At); err != nil {
			return err
		}
		return orgaudit.Insert(ctx, tx, rr.Audit)
	})
	if err != nil {
		return StoredRequest{}, err
	}
	return s.GetRequest(ctx, id)
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// Candidates serves GET /v1/admin/routing-exceptions/{id}/candidates.
func (h *Handler) Candidates(w http.ResponseWriter, r *http.Request) {
	_, markets, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	out, err := h.svc.Candidates(r.Context(), markets, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"organisations": out})
}

// RerouteException serves POST /v1/admin/routing-exceptions/{id}/assign:
// {"organisation_id": "...", "user_id": "..."|null, "note": "..."}.
func (h *Handler) RerouteException(w http.ResponseWriter, r *http.Request) {
	p, markets, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	var in struct {
		OrganisationID string  `json:"organisation_id"`
		UserID         *string `json:"user_id"`
		Note           string  `json:"note"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	org, err := uuid.Parse(strings.TrimSpace(in.OrganisationID))
	if err != nil {
		h.fail(w, r, &ValidationError{Fields: map[string]string{"organisation_id": "must be an organisation id"}})
		return
	}
	var user *uuid.UUID
	if in.UserID != nil && strings.TrimSpace(*in.UserID) != "" {
		u, err := uuid.Parse(strings.TrimSpace(*in.UserID))
		if err != nil {
			h.fail(w, r, &ValidationError{Fields: map[string]string{"user_id": "must be a user id or null"}})
			return
		}
		user = &u
	}
	req, err := h.svc.RerouteException(r.Context(), markets, Actor{UserID: p.UserID, Name: p.Name}, id, org, user, in.Note)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Audit line: who sent which request where. No customer data.
	h.logger.InfoContext(r.Context(), "routing exception reassigned",
		slog.String("audit", "routing_exception_rerouted"), slog.String("property_request_id", req.ID.String()),
		slog.String("organisation_id", org.String()), slog.String("actor_user_id", p.UserID.String()))
	views, err := h.exceptionViews(r.Context(), []StoredRequest{req})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"request": views[0]})
}
