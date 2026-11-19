package accounts

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// MemoryDeps are the in-memory stores account administration writes to.
// In Postgres it is all one transaction; here every check is made first and
// the changes are applied store by store.
type MemoryDeps struct {
	Identity *identity.MemoryStore
	// Listings, Teams and Directory are optional (nil skips that part).
	Listings  *listings.MemoryStore
	Teams     *teams.MemoryStore
	Directory *directory.MemoryStore
	// Audit receives the administrators' audit records (nil: a private log).
	Audit *adminaudit.MemoryLog
	// CancelSubscriptions ends the user's own subscriptions (deletion) and
	// reports how many; nil when there is no subscription store.
	CancelSubscriptions func(userID uuid.UUID, at time.Time) int
	// DeleteSignature removes the user's saved signature (nil: nothing to do).
	DeleteSignature func(userID uuid.UUID)
	// DeleteSavedListings removes the user's saved listings (nil: nothing to do).
	DeleteSavedListings func(userID uuid.UUID)
	// EraseApplications deletes the user's draft applications and anonymises
	// the others (nil: nothing to do).
	EraseApplications func(userID uuid.UUID, at time.Time)
	// DeleteVenueBookings removes the user's hourly venue bookings (nil:
	// nothing to do).
	DeleteVenueBookings func(userID uuid.UUID)
	// DeleteNotifications removes the user's push devices, in-app
	// notifications and notification preferences (nil: nothing to do).
	DeleteNotifications func(userID uuid.UUID)
	// ForgetTenancies handles the user's tenancy rows: their personal book's
	// DRAFT tenancies deleted, the manager name tenants see replaced, and the
	// user unlinked as creator, activator and ender (nil: nothing to do).
	ForgetTenancies func(userID uuid.UUID, deletedName string)
	// ForgetMaintenance unlinks the user from maintenance requests, their
	// timeline, photos and costs, and replaces their name where they acted
	// as a tenant (nil: nothing to do).
	ForgetMaintenance func(userID uuid.UUID, deletedName string)
	// ForgetInspections unlinks the user from inspections (inspector, creator,
	// completer, canceller) and their photos (nil: nothing to do).
	ForgetInspections func(userID uuid.UUID, deletedName string)
	// ForgetReviews unlinks the user from reviews of service providers (a
	// customer's review shows "Former customer"), reports, replies and the
	// enquiries they marked replied (nil: nothing to do).
	ForgetReviews func(userID uuid.UUID, deletedName string)
	// ForgetPackages ends the user's own listing package assignments and
	// unlinks them as the administrator who made any (nil: nothing to do).
	ForgetPackages func(userID uuid.UUID, deletedName string, at time.Time)
	// ForgetPayments unlinks the user from their online payments (kept for
	// finance) and expires their open checkouts (nil: nothing to do).
	ForgetPayments func(userID uuid.UUID, deletedName string, at time.Time)
	// ForgetLegal unlinks the user as creator and publisher of legal
	// document versions (API 0.43.0). Their acceptances are kept.
	ForgetLegal func(userID uuid.UUID, deletedName string)
	// ForgetDevelopments unlinks the user from the development projects
	// (creator, submitter, reviewer, photo uploader; API 0.44.0). The
	// projects stay with their organisation (nil: nothing to do).
	ForgetDevelopments func(userID uuid.UUID)
	// Now is the clock used for listing expiry in the detail (nil: time.Now).
	Now func() time.Time
}

// MemoryStore is the in-memory Store for development and tests.
type MemoryStore struct{ deps MemoryDeps }

// NewMemoryStore returns a store over the given in-memory stores.
func NewMemoryStore(deps MemoryDeps) *MemoryStore {
	if deps.Audit == nil {
		deps.Audit = &adminaudit.MemoryLog{}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &MemoryStore{deps: deps}
}

// AuditLog returns the administrators' audit log the store writes to.
func (s *MemoryStore) AuditLog() *adminaudit.MemoryLog { return s.deps.Audit }

func summaryOf(a identity.AdminAccount) Summary {
	x := Summary{ID: a.ID, Name: a.Name, Email: a.Email, Status: a.Status, EmailVerified: a.EmailVerifiedAt != nil, CreatedAt: a.CreatedAt,
		LastSignInAt: a.LastSignInAt, AccountRoles: []string{}, PlatformRoles: []string{}, MarketCodes: []string{}}
	for _, r := range a.AccountRoles {
		x.AccountRoles = append(x.AccountRoles, r.Role)
	}
	slices.Sort(x.AccountRoles)
	for _, r := range a.PlatformRoles {
		if !slices.Contains(x.PlatformRoles, r.Role) {
			x.PlatformRoles = append(x.PlatformRoles, r.Role)
		}
	}
	slices.Sort(x.PlatformRoles)
	for _, m := range a.AllMemberships {
		if m.Status != identity.MembershipRemoved && !slices.Contains(x.MarketCodes, m.MarketCode) {
			x.MarketCodes = append(x.MarketCodes, m.MarketCode)
		}
	}
	slices.Sort(x.MarketCodes)
	return x
}

// Search implements Store.
func (s *MemoryStore) Search(_ context.Context, f Filter) ([]Summary, int, error) {
	q := strings.ToLower(f.Q)
	var found []Summary
	for _, a := range s.deps.Identity.AdminAccounts() {
		x := summaryOf(a)
		switch {
		case q != "" && !strings.Contains(strings.ToLower(x.Name), q) && !strings.Contains(strings.ToLower(x.Email), q),
			f.Role != "" && !slices.Contains(x.AccountRoles, f.Role) && !slices.Contains(x.PlatformRoles, f.Role),
			f.Status != "" && x.Status != f.Status,
			f.Market != "" && !slices.Contains(x.MarketCodes, f.Market):
			continue
		}
		found = append(found, x)
	}
	sort.SliceStable(found, func(i, j int) bool {
		if !found[i].CreatedAt.Equal(found[j].CreatedAt) {
			return found[i].CreatedAt.After(found[j].CreatedAt)
		}
		return found[i].ID.String() > found[j].ID.String()
	})
	total := len(found)
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	start := min(max(f.Offset, 0), total)
	end := min(start+limit, total)
	return append([]Summary{}, found[start:end]...), total, nil
}

// Account implements Store.
func (s *MemoryStore) Account(_ context.Context, id uuid.UUID) (Account, error) {
	a, err := s.deps.Identity.AdminAccount(id)
	if errors.Is(err, identity.ErrNotFound) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, err
	}
	out := Account{ID: a.ID, Name: a.Name, Email: a.Email, Status: a.Status, EmailVerified: a.EmailVerifiedAt != nil, CreatedAt: a.CreatedAt,
		LastSignInAt: a.LastSignInAt, SuspendedAt: a.SuspendedAt, SuspensionReason: a.SuspensionReason, DeletedAt: a.DeletedAt,
		PlatformRoles: append([]auth.PlatformRole(nil), a.PlatformRoles...), PasswordHash: a.PasswordHash}
	for _, r := range a.AccountRoles {
		out.AccountRoles = append(out.AccountRoles, AccountRole{Role: r.Role, Status: r.Status, OnboardingStatus: r.OnboardingStatus})
	}
	for _, m := range a.AllMemberships {
		out.Memberships = append(out.Memberships, Membership(m))
	}
	if s.deps.Listings != nil {
		out.ListingCounts = s.deps.Listings.UserListingCounts(id, s.deps.Now())
	}
	normalise(&out)
	return out, nil
}

// Suspend implements Store.
func (s *MemoryStore) Suspend(ctx context.Context, x Suspension) (Account, error) {
	a, err := s.Account(ctx, x.UserID)
	if err != nil {
		return Account{}, err
	}
	if a.Status != StatusActive {
		return Account{}, ErrState
	}
	var by *uuid.UUID
	if x.Actor.UserID != uuid.Nil {
		by = &x.Actor.UserID
	}
	if _, err := s.deps.Identity.SuspendAccount(x.UserID, x.Reason, by, x.At); err != nil {
		return Account{}, ErrState
	}
	if s.deps.Listings != nil {
		s.deps.Listings.RefreshOwnerListings(x.UserID, x.At)
	}
	s.deps.Audit.Append(x.Audit)
	s.deps.Identity.EmitUserEvent(EventAccountSuspended, x.UserID, x.At)
	return s.Account(ctx, x.UserID)
}

// Reactivate implements Store.
func (s *MemoryStore) Reactivate(ctx context.Context, x Reactivation) (Account, error) {
	a, err := s.Account(ctx, x.UserID)
	if err != nil {
		return Account{}, err
	}
	if a.Status != StatusSuspended {
		return Account{}, ErrState
	}
	if err := s.deps.Identity.ReactivateAccount(x.UserID); err != nil {
		return Account{}, ErrState
	}
	if s.deps.Listings != nil {
		s.deps.Listings.RefreshOwnerListings(x.UserID, x.At)
	}
	s.deps.Audit.Append(x.Audit)
	s.deps.Identity.EmitUserEvent(EventAccountReactivated, x.UserID, x.At)
	return s.Account(ctx, x.UserID)
}

// soleOwnerOf returns the organisations where the user is the only active
// owner and someone else is still a member (the rule of soleOwnerSQL).
func (s *MemoryStore) soleOwnerOf(a Account) []string {
	var names []string
	for _, m := range a.Memberships {
		if m.Role != auth.RoleOwner || m.Status != identity.MembershipActive {
			continue
		}
		otherOwner, otherMember := false, false
		for _, o := range s.deps.Identity.OrganisationMemberships(m.OrganisationID) {
			if o.UserID == a.ID {
				continue
			}
			u, ok := s.deps.Identity.Account(o.UserID)
			if !ok {
				continue
			}
			if o.Role == auth.RoleOwner && o.Status == identity.MembershipActive && u.Status == StatusActive {
				otherOwner = true
			}
			if o.Status != identity.MembershipRemoved && u.Status != StatusDeleted {
				otherMember = true
			}
		}
		if !otherOwner && otherMember {
			names = append(names, m.OrganisationName)
		}
	}
	slices.Sort(names)
	return names
}

// Delete implements Store.
func (s *MemoryStore) Delete(ctx context.Context, d Deletion) (Deleted, error) {
	a, err := s.Account(ctx, d.UserID)
	if err != nil {
		return Deleted{}, err
	}
	if a.Status == StatusDeleted {
		return Deleted{}, ErrState
	}
	if names := s.soleOwnerOf(a); len(names) > 0 {
		return Deleted{}, &SoleOwnerError{Organisations: names}
	}
	var out Deleted
	if out.SessionsRevoked, err = s.deps.Identity.AnonymiseAccount(d.UserID, DeletedName, DeletedEmail(d.UserID), d.At); err != nil {
		return Deleted{}, ErrState
	}
	left := map[uuid.UUID]bool{}
	for _, m := range a.Memberships {
		if m.Status == identity.MembershipRemoved {
			continue
		}
		left[m.OrganisationID] = true
		if s.deps.Teams == nil {
			continue
		}
		org := m.OrganisationID
		_, _, err := s.deps.Teams.ChangeMember(ctx, org, d.UserID, func(teams.Member, int) (teams.MemberChange, error) {
			change := teams.MemberChange{Remove: true, Release: teams.ReleaseAll, Actor: teams.Actor{UserID: d.Actor.UserID, Name: d.Actor.Name}, At: d.At}
			if d.TeamAudit != nil {
				change.Events = []orgaudit.Event{d.TeamAudit(org, nil)}
			}
			return change, nil
		})
		if err != nil && !errors.Is(err, teams.ErrMemberNotFound) {
			return Deleted{}, err
		}
	}
	s.deps.Identity.RemoveMembershipsOf(d.UserID)
	out.OrganisationsLeft = len(left)
	if s.deps.Listings != nil {
		actorKind := listings.ActorReviewer
		if d.SelfService {
			actorKind = listings.ActorAdvertiser
		}
		var actorID *uuid.UUID
		if d.Actor.UserID != uuid.Nil {
			actorID = &d.Actor.UserID
		}
		out.ListingsWithdrawn = s.deps.Listings.WithdrawOwnerListings(d.UserID, actorID, actorKind, "The advertiser's account was deleted.", d.At)
		out.ListingsUnassigned = s.deps.Listings.UnassignAgent(d.UserID, d.At)
	}
	if s.deps.Directory != nil {
		if p, ok := s.deps.Directory.AgentProfile(d.UserID); ok {
			p.Status = "SUSPENDED"
			s.deps.Directory.Put(p)
		}
	}
	if s.deps.CancelSubscriptions != nil {
		out.SubscriptionsCanceled = s.deps.CancelSubscriptions(d.UserID, d.At)
	}
	if s.deps.DeleteSignature != nil {
		s.deps.DeleteSignature(d.UserID)
	}
	if s.deps.DeleteSavedListings != nil {
		s.deps.DeleteSavedListings(d.UserID)
	}
	if s.deps.EraseApplications != nil {
		s.deps.EraseApplications(d.UserID, d.At)
	}
	if s.deps.DeleteVenueBookings != nil {
		s.deps.DeleteVenueBookings(d.UserID)
	}
	if s.deps.DeleteNotifications != nil {
		s.deps.DeleteNotifications(d.UserID)
	}
	if s.deps.ForgetTenancies != nil {
		s.deps.ForgetTenancies(d.UserID, DeletedName)
	}
	if s.deps.ForgetMaintenance != nil {
		s.deps.ForgetMaintenance(d.UserID, DeletedName)
	}
	if s.deps.ForgetInspections != nil {
		s.deps.ForgetInspections(d.UserID, DeletedName)
	}
	if s.deps.ForgetReviews != nil {
		s.deps.ForgetReviews(d.UserID, DeletedName)
	}
	if s.deps.ForgetPackages != nil {
		s.deps.ForgetPackages(d.UserID, DeletedName, d.At)
	}
	if s.deps.ForgetPayments != nil {
		s.deps.ForgetPayments(d.UserID, DeletedName, d.At)
	}
	if s.deps.ForgetLegal != nil {
		s.deps.ForgetLegal(d.UserID, DeletedName)
	}
	if s.deps.ForgetDevelopments != nil {
		s.deps.ForgetDevelopments(d.UserID)
	}
	// Marketing consent records (API 0.37.1): deleted with the account.
	s.deps.Identity.DeleteMarketingConsents(d.UserID)
	s.deps.Audit.Append(d.Audit)
	return out, nil
}

// normalise makes every list and map of an account non-nil.
func normalise(a *Account) {
	if a.PlatformRoles == nil {
		a.PlatformRoles = []auth.PlatformRole{}
	}
	if a.AccountRoles == nil {
		a.AccountRoles = []AccountRole{}
	}
	if a.Memberships == nil {
		a.Memberships = []Membership{}
	}
	if a.ListingCounts == nil {
		a.ListingCounts = map[string]int{}
	}
}
