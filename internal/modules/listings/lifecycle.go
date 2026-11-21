package listings

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

// The owner's lifecycle actions (API 0.48.0, FSD section 3):
//
//	PUBLISHED ──pause──> PAUSED ──resume──> PUBLISHED (EXPIRED if the period ended)
//	PUBLISHED | PAUSED | EXPIRED ──mark sold──> SOLD (a sale)
//	                             ──mark let──> LET (anything let: rent, short stay, venue)
//	                             ──close──> CLOSED (withdrawn, any listing)
//
// A paused listing is hidden from the public but keeps its place in the
// package (it may come back at any moment) and its publication period keeps
// running: pausing never extends it. SOLD, LET and CLOSED free the place;
// relist starts a new publication cycle through review, as after expiry.

// Owner actions of the lifecycle (ManagedView.actions).
const (
	ActPause    = "pause"
	ActResume   = "resume"
	ActMarkSold = "mark_sold"
	ActMarkLet  = "mark_let"
	ActClose    = "close"
)

// CodeTypeConflict: mark sold on a listing to let, or mark let on a sale.
const CodeTypeConflict = "LISTING_TYPE_CONFLICT"

// ErrTypeConflict is CodeTypeConflict.
var ErrTypeConflict = fmt.Errorf("listing type conflict")

// finalFrom are the states a listing can be marked sold, let or closed from.
var finalFrom = []string{StatusPublished, StatusPaused, StatusExpired}

// relistFrom are the states relist accepts.
var relistFrom = []string{StatusArchived, StatusExpired, StatusSold, StatusLet, StatusClosed}

// lifecycleActions are the lifecycle actions allowed for a listing whose
// effective status is status.
func lifecycleActions(m ManagedListing, status string) []string {
	var out []string
	switch status {
	case StatusPublished:
		out = append(out, ActPause)
	case StatusPaused:
		out = append(out, ActResume)
	}
	if slices.Contains(finalFrom, status) {
		if m.Deal == DealRent {
			out = append(out, ActMarkLet)
		} else {
			out = append(out, ActMarkSold)
		}
		out = append(out, ActClose)
	}
	return out
}

// lifecycle is one owner action: the states it accepts, the state it
// leaves, its timeline action and its audit event.
type lifecycle struct {
	from   []string
	to     string
	action string
	event  string
	// merge folds a pending revision into the listing (as archive does), for
	// the final states: a relist then starts from the latest content.
	merge bool
}

var lifecycles = map[string]lifecycle{
	ActPause:    {from: []string{StatusPublished}, to: StatusPaused, action: ActionPaused, event: "listing.paused"},
	ActResume:   {from: []string{StatusPaused}, to: StatusPublished, action: ActionResumed, event: "listing.resumed"},
	ActMarkSold: {from: finalFrom, to: StatusSold, action: ActionMarkedSold, event: "listing.sold", merge: true},
	ActMarkLet:  {from: finalFrom, to: StatusLet, action: ActionMarkedLet, event: "listing.let", merge: true},
	ActClose:    {from: finalFrom, to: StatusClosed, action: ActionClosed, event: "listing.closed", merge: true},
}

// Pause hides a published listing; it keeps its place and its period.
func (mg *Manager) Pause(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedView, error) {
	return mg.lifecycle(ctx, p, id, ActPause)
}

// Resume shows a paused listing again (EXPIRED when its period ended).
func (mg *Manager) Resume(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedView, error) {
	return mg.lifecycle(ctx, p, id, ActResume)
}

// MarkSold records that a listing for sale is sold.
func (mg *Manager) MarkSold(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedView, error) {
	return mg.lifecycle(ctx, p, id, ActMarkSold)
}

// MarkLet records that a listing to let is let.
func (mg *Manager) MarkLet(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedView, error) {
	return mg.lifecycle(ctx, p, id, ActMarkLet)
}

// Close withdraws a listing from the market.
func (mg *Manager) Close(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedView, error) {
	return mg.lifecycle(ctx, p, id, ActClose)
}

func (mg *Manager) lifecycle(ctx context.Context, p *auth.Principal, id uuid.UUID, act string) (ManagedView, error) {
	lc := lifecycles[act]
	m, err := mg.editable(ctx, p, id)
	if err != nil {
		return ManagedView{}, err
	}
	now := mg.clock()
	from := m.EffectiveStatus(now)
	if !slices.Contains(lc.from, from) {
		return ManagedView{}, conflict(fmt.Sprintf("A listing that is %s cannot be %s.", strings.ToLower(StatusLabel(from)), pastTense(act)))
	}
	switch {
	case act == ActMarkSold && m.Deal == DealRent:
		return ManagedView{}, ErrTypeConflict
	case act == ActMarkLet && m.Deal != DealRent:
		return ManagedView{}, ErrTypeConflict
	}
	expect := m.UpdatedAt
	wasPublic := m.Public(now)
	if lc.merge {
		mergeRevision(&m)
	}
	to := lc.to
	if act == ActResume && !m.ExpiresAt.After(now) {
		to = StatusExpired
	}
	m.Status, m.UpdatedAt = to, now
	ch := Change{Entry: mg.entry(m, p, ActorAdvertiser, lc.action, from, to, "", "", now)}
	switch {
	case wasPublic && !m.Public(now):
		ch.Events = append(ch.Events, indexEvent(EventListingUnpublished, m.ID))
	case !wasPublic && m.Public(now):
		ch.Events = append(ch.Events, indexEvent(EventListingPublished, m.ID))
	}
	if err := mg.save(ctx, m, expect, ch); err != nil {
		return ManagedView{}, err
	}
	mg.audit(ctx, lc.event, p, m.ID)
	return mg.ownerView(ctx, p, m.ID)
}

func pastTense(act string) string {
	switch act {
	case ActPause:
		return "paused"
	case ActResume:
		return "resumed"
	case ActMarkSold:
		return "marked sold"
	case ActMarkLet:
		return "marked let"
	}
	return "closed"
}
