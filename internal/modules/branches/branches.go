// Package branches implements an organisation's branches (API 0.57.0,
// FR-002): OWNER and MANAGER create, rename and archive them, pin members to
// one, and tie listings to one. A pinned member sees only the objects with no
// branch or their own branch (auth.Principal.MayAccessBranch); the other
// modules apply that rule. Every change is in the organisation audit log.
package branches

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Audit vocabulary.
const (
	TargetBranch        = "BRANCH"
	TargetMember        = "MEMBER"
	TargetListing       = "LISTING"
	ActionCreated       = "BRANCH_CREATED"
	ActionUpdated       = "BRANCH_UPDATED"
	ActionMemberBranch  = "MEMBER_BRANCH_CHANGED"
	ActionListingBranch = "LISTING_BRANCH_CHANGED"
	MaxBranches         = 100
	maxNameRunes        = 80
)

// Errors.
var (
	ErrNotFound  = errors.New("branch not found")
	ErrNameTaken = errors.New("a branch of the organisation already has that name")
	ErrNotMember = errors.New("not a member of the organisation")
	ErrNoListing = errors.New("the listing is not the organisation's")
	ErrForbidden = errors.New("not allowed")
	ErrInactive  = errors.New("the branch is archived")
	ErrTooMany   = errors.New("too many branches")
)

// ValidationError carries per-field messages.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// Branch is one branch.
type Branch struct {
	ID             uuid.UUID
	OrganisationID uuid.UUID
	Name           string
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Store persists branches and the pins and ties.
type Store interface {
	List(ctx context.Context, organisationID uuid.UUID) ([]Branch, error)
	Get(ctx context.Context, organisationID, id uuid.UUID) (Branch, error)
	// Create inserts a branch with its audit (ErrNameTaken, ErrTooMany).
	Create(ctx context.Context, b Branch, audit orgaudit.Event) error
	// Update writes the name and the active flag with its audit (ErrNameTaken).
	Update(ctx context.Context, b Branch, audit orgaudit.Event) error
	// PinMember sets (or clears) a member's branch with its audit
	// (ErrNotMember). It returns the member's name.
	PinMember(ctx context.Context, organisationID, userID uuid.UUID, branch *uuid.UUID, audit func(name string) orgaudit.Event) error
	// TieListing sets (or clears) a listing's branch with its audit
	// (ErrNoListing when the listing is not the organisation's).
	TieListing(ctx context.Context, organisationID, listingID uuid.UUID, branch *uuid.UUID, audit func(label string) orgaudit.Event) error
	// Pins returns the organisation's pinned members: user -> branch.
	Pins(ctx context.Context, organisationID uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
}

// Service holds the rules.
type Service struct {
	store Store
	now   func() time.Time
}

// NewService builds the service.
func NewService(store Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now}
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

func member(p *auth.Principal, org uuid.UUID) bool {
	return p != nil && slices.Contains(p.OrganisationIDs(), org)
}

// List returns the organisation's branches (any member).
func (s *Service) List(ctx context.Context, p *auth.Principal, org uuid.UUID) ([]Branch, error) {
	if !member(p, org) {
		return nil, ErrForbidden
	}
	return s.store.List(ctx, org)
}

func cleanName(raw string) (string, map[string]string) {
	name := strings.Join(strings.Fields(raw), " ")
	switch {
	case name == "":
		return "", map[string]string{"name": "is required"}
	case len([]rune(name)) > maxNameRunes:
		return "", map[string]string{"name": "must be at most 80 characters"}
	}
	return name, nil
}

// Create adds a branch (OWNER, MANAGER).
func (s *Service) Create(ctx context.Context, p *auth.Principal, org uuid.UUID, name string) (Branch, error) {
	if !p.MayAssignWork(org) {
		return Branch{}, ErrForbidden
	}
	name, f := cleanName(name)
	if f != nil {
		return Branch{}, &ValidationError{Fields: f}
	}
	now := s.clock()
	id, err := uuid.NewV7()
	if err != nil {
		return Branch{}, err
	}
	b := Branch{ID: id, OrganisationID: org, Name: name, Active: true, CreatedAt: now, UpdatedAt: now}
	audit := orgaudit.New(org, p.UserID, p.Name, ActionCreated, TargetBranch, id, name, nil, map[string]any{"name": name}, now)
	if err := s.store.Create(ctx, b, audit); err != nil {
		return Branch{}, err
	}
	return b, nil
}

// Update renames or archives a branch (OWNER, MANAGER).
func (s *Service) Update(ctx context.Context, p *auth.Principal, org, id uuid.UUID, name *string, active *bool) (Branch, error) {
	if !p.MayAssignWork(org) {
		return Branch{}, ErrForbidden
	}
	cur, err := s.store.Get(ctx, org, id)
	if err != nil {
		return Branch{}, err
	}
	next := cur
	if name != nil {
		n, f := cleanName(*name)
		if f != nil {
			return Branch{}, &ValidationError{Fields: f}
		}
		next.Name = n
	}
	if active != nil {
		next.Active = *active
	}
	next.UpdatedAt = s.clock()
	audit := orgaudit.New(org, p.UserID, p.Name, ActionUpdated, TargetBranch, id, next.Name,
		map[string]any{"name": cur.Name, "active": cur.Active}, map[string]any{"name": next.Name, "active": next.Active}, next.UpdatedAt)
	if err := s.store.Update(ctx, next, audit); err != nil {
		return Branch{}, err
	}
	return next, nil
}

// usable checks a branch to assign: the organisation's and active.
func (s *Service) usable(ctx context.Context, org uuid.UUID, branch *uuid.UUID) error {
	if branch == nil {
		return nil
	}
	b, err := s.store.Get(ctx, org, *branch)
	if errors.Is(err, ErrNotFound) {
		return &ValidationError{Fields: map[string]string{"branch_id": "is not a branch of this organisation"}}
	}
	if err != nil {
		return err
	}
	if !b.Active {
		return &ValidationError{Fields: map[string]string{"branch_id": "is archived"}}
	}
	return nil
}

func idText(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// PinMember pins a member to a branch, or unpins them (OWNER, MANAGER).
func (s *Service) PinMember(ctx context.Context, p *auth.Principal, org, user uuid.UUID, branch *uuid.UUID) error {
	if !p.MayAssignWork(org) {
		return ErrForbidden
	}
	if err := s.usable(ctx, org, branch); err != nil {
		return err
	}
	now := s.clock()
	return s.store.PinMember(ctx, org, user, branch, func(name string) orgaudit.Event {
		return orgaudit.New(org, p.UserID, p.Name, ActionMemberBranch, TargetMember, user, name, nil, map[string]any{"branch_id": idText(branch)}, now)
	})
}

// TieListing ties a listing to a branch, or unties it (OWNER, MANAGER).
func (s *Service) TieListing(ctx context.Context, p *auth.Principal, org, listing uuid.UUID, branch *uuid.UUID) error {
	if !p.MayAssignWork(org) {
		return ErrForbidden
	}
	if err := s.usable(ctx, org, branch); err != nil {
		return err
	}
	now := s.clock()
	return s.store.TieListing(ctx, org, listing, branch, func(label string) orgaudit.Event {
		return orgaudit.New(org, p.UserID, p.Name, ActionListingBranch, TargetListing, listing, label, nil, map[string]any{"branch_id": idText(branch)}, now)
	})
}

// Pins returns user -> branch for the organisation (any member).
func (s *Service) Pins(ctx context.Context, p *auth.Principal, org uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	if !member(p, org) {
		return nil, ErrForbidden
	}
	return s.store.Pins(ctx, org)
}

// ResolveName returns the active branch of the organisation with that name
// (case-insensitive), for the ledger.
func ResolveName(ctx context.Context, store Store, org uuid.UUID, name string) (*Branch, error) {
	list, err := store.List(ctx, org)
	if err != nil {
		return nil, err
	}
	for _, b := range list {
		if b.Active && strings.EqualFold(b.Name, strings.TrimSpace(name)) {
			return &b, nil
		}
	}
	return nil, nil
}
