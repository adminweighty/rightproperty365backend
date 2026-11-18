package app

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/teams"
)

// CRMService builds the agent CRM's service (the API, the worker and the
// in-memory development process share it).
func CRMService(store crm.Store, items crm.Items, listingStore listings.Store, identityStore identity.Store, ref reference.Store,
	team teams.Store, logger *slog.Logger, now func() time.Time) *crm.Service {
	d := crm.Deps{
		Store: store, Items: items, Listings: crmListings{store: listingStore}, People: assignees{identity: identityStore},
		Vocabulary: reference.NewService(ref, nil), Logger: logger, Now: now,
	}
	if team != nil {
		d.Members = func(ctx context.Context, organisationID uuid.UUID) ([]crm.Assignee, error) {
			members, err := team.Members(ctx, organisationID)
			if err != nil {
				return nil, err
			}
			out := make([]crm.Assignee, 0, len(members))
			for _, m := range members {
				if m.EffectiveStatus() == teams.MemberActive {
					out = append(out, crm.Assignee{UserID: m.UserID, Name: m.Name, Role: m.Role})
				}
			}
			slices.SortStableFunc(out, func(a, b crm.Assignee) int { return strings.Compare(a.Name, b.Name) })
			return out, nil
		}
	}
	return crm.NewService(d)
}

// crmListings looks listings up for the CRM.
type crmListings struct{ store listings.Store }

// Listings implements crm.Listings.
func (l crmListings) Listings(ctx context.Context, ids []uuid.UUID) ([]crm.ListingRef, error) {
	records, err := l.store.ByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]crm.ListingRef, 0, len(records))
	for _, r := range records {
		out = append(out, crm.ListingRef{ID: r.ID, Title: r.Title, Reference: r.Reference, Slug: r.Slug, OrganisationID: r.OrganisationID,
			SellerUserID: r.SellerUserID, MarketCode: r.MarketCode})
	}
	return out, nil
}

// OwnerMarket implements crm.Listings: the market of the direct owner's
// most recent listing.
func (l crmListings) OwnerMarket(ctx context.Context, userID uuid.UUID) (string, error) {
	records, err := l.store.Owned(ctx, nil, &userID)
	if err != nil || len(records) == 0 {
		return "", err
	}
	return records[0].MarketCode, nil
}

// marketCodeOf returns the code of a market ID ("" when unknown).
func marketCodeOf(id uuid.UUID) string {
	for _, m := range markets.Seed() {
		if m.ID == id {
			return m.Code
		}
	}
	return ""
}

// MemoryCRMItems reads the items the CRM links from the in-memory stores
// (development and tests). Postgres uses crm.PostgresItems.
type MemoryCRMItems struct {
	Requests     *propertyrequests.MemoryStore
	Enquiries    *businessenquiries.MemoryStore
	Appointments *appointments.MemoryStore
	Identity     identity.Store
	Listings     listings.Store
	// Applications are the submitted applications (API 0.32.0; nil: none).
	Applications *applications.MemoryStore
}

func (m MemoryCRMItems) accountEmail(ctx context.Context, userID *uuid.UUID) string {
	if userID == nil || m.Identity == nil {
		return ""
	}
	u, err := m.Identity.UserByID(ctx, *userID)
	if err != nil || !u.Verified() {
		return ""
	}
	return u.Email
}

func (m MemoryCRMItems) all(ctx context.Context) ([]crm.Item, error) {
	var out []crm.Item
	if m.Requests != nil {
		reqs := m.Requests.StoredRequests()
		titles := map[uuid.UUID]string{}
		if m.Listings != nil && len(reqs) > 0 {
			var ids []uuid.UUID
			for _, r := range reqs {
				if !slices.Contains(ids, r.ListingID) {
					ids = append(ids, r.ListingID)
				}
			}
			records, err := m.Listings.ByIDs(ctx, ids)
			if err != nil {
				return nil, err
			}
			for _, r := range records {
				titles[r.ID] = r.Title
			}
		}
		for _, r := range reqs {
			listing := r.ListingID
			it := crm.Item{Kind: crm.KindPropertyRequest, ID: r.ID, Reference: r.Reference, Type: r.RequestType, Status: r.Stage,
				Title: titles[r.ListingID], ListingID: &listing, MarketCode: marketCodeOf(r.MarketID), Name: r.ContactName,
				Email: r.ContactEmail, Phone: r.ContactPhone, AccountEmail: m.accountEmail(ctx, r.CustomerUserID), CreatedAt: r.CreatedAt}
			if rc := r.Recipient; rc != nil {
				if rc.OrganisationID != nil {
					it.OrganisationID, it.AssignedUserID = rc.OrganisationID, rc.UserID
				} else if rc.Type == propertyrequests.RecipientSeller {
					it.DirectOwnerUserID = rc.UserID
				}
			}
			out = append(out, it)
		}
	}
	if m.Enquiries != nil {
		for _, e := range m.Enquiries.Enquiries() {
			it := crm.Item{Kind: crm.KindBusinessEnquiry, ID: e.ID, Reference: e.Reference, Type: e.Service, Status: e.Status,
				Title: e.ProfileName, MarketCode: marketCodeOf(e.MarketID), Name: e.GuestName, Email: e.GuestEmail, Phone: e.GuestPhone,
				AccountEmail: m.accountEmail(ctx, e.CustomerUserID), CreatedAt: e.CreatedAt}
			if rc := e.Recipient; rc != nil {
				org := rc.OrganisationID
				it.OrganisationID, it.AssignedUserID = &org, rc.UserID
			}
			out = append(out, it)
		}
	}
	if m.Appointments != nil {
		for _, a := range m.Appointments.All() {
			title := a.ListingTitle
			if title == "" {
				title = a.WithName
			}
			it := crm.Item{Kind: crm.KindAppointment, ID: a.ID, Reference: a.Reference, Type: a.Type, Status: a.Status, Title: title,
				ListingID: a.ListingID, MarketCode: marketCodeOf(a.MarketID), Name: a.CustomerName, Email: a.CustomerEmail,
				Phone: a.CustomerPhone, AccountEmail: m.accountEmail(ctx, a.CustomerUserID), CreatedAt: a.CreatedAt}
			if a.OrganisationID != nil {
				it.OrganisationID, it.AssignedUserID = a.OrganisationID, a.AssignedUserID
			} else if a.RecipientType == appointments.RecipientSeller {
				it.DirectOwnerUserID = a.AssignedUserID
			}
			out = append(out, it)
		}
	}
	if m.Applications != nil {
		list := m.Applications.AllApplications()
		agents := map[uuid.UUID]*uuid.UUID{}
		if m.Listings != nil && len(list) > 0 {
			var ids []uuid.UUID
			for _, a := range list {
				if !slices.Contains(ids, a.ListingID) {
					ids = append(ids, a.ListingID)
				}
			}
			records, err := m.Listings.ByIDs(ctx, ids)
			if err != nil {
				return nil, err
			}
			for _, r := range records {
				agents[r.ID] = r.AgentUserID
			}
		}
		for _, a := range list {
			if a.Status == applications.StatusDraft {
				continue
			}
			listing := a.ListingID
			it := crm.Item{Kind: crm.KindApplication, ID: a.ID, Reference: a.Reference, Type: "APPLICATION", Status: a.Status,
				Title: a.Listing.Title, ListingID: &listing, Name: a.ApplicantName, CreatedAt: a.CreatedAt}
			if a.LastSubmittedAt != nil {
				it.CreatedAt = *a.LastSubmittedAt
			}
			if a.ApplicantUserID != nil && m.Identity != nil {
				if u, err := m.Identity.UserByID(ctx, *a.ApplicantUserID); err == nil {
					it.Name, it.Email = u.Name, u.Email
					if u.Verified() {
						it.AccountEmail = u.Email
					}
				}
			}
			if a.OrganisationID != nil {
				it.OrganisationID, it.AssignedUserID = a.OrganisationID, agents[a.ListingID]
			} else {
				it.DirectOwnerUserID = a.OwnerUserID
			}
			out = append(out, it)
		}
	}
	return out, nil
}

// Item implements crm.Items.
func (m MemoryCRMItems) Item(ctx context.Context, kind string, id uuid.UUID) (crm.Item, error) {
	all, err := m.all(ctx)
	if err != nil {
		return crm.Item{}, err
	}
	for _, it := range all {
		if it.Kind == kind && it.ID == id {
			return it, nil
		}
	}
	return crm.Item{}, crm.ErrItemNotFound
}

// Candidates implements crm.Items.
func (m MemoryCRMItems) Candidates(ctx context.Context, book crm.Book, emails, phoneTails []string) ([]crm.Item, error) {
	all, err := m.all(ctx)
	if err != nil {
		return nil, err
	}
	var out []crm.Item
	for _, it := range all {
		b, ok := it.Book()
		if !ok || !b.Same(book) {
			continue
		}
		email := strings.ToLower(strings.TrimSpace(it.Email))
		account := strings.ToLower(it.AccountEmail)
		var d strings.Builder
		for _, r := range it.Phone {
			if r >= '0' && r <= '9' {
				d.WriteRune(r)
			}
		}
		tail := d.String()
		if len(tail) > 7 {
			tail = tail[len(tail)-7:]
		}
		if slices.Contains(emails, email) || (account != "" && slices.Contains(emails, account)) || (tail != "" && slices.Contains(phoneTails, tail)) {
			out = append(out, it)
		}
	}
	return out, nil
}

// RunCRMJobs runs the CRM's reminder sweep in this process (the worker's
// job; the in-memory development API runs it itself).
func RunCRMJobs(ctx context.Context, svc *crm.Service) { svc.RunReminderSweeps(ctx) }

// errNoCRM is returned by NewRouter without a contact store.
var errNoCRM = errors.New("app: no contact store")
