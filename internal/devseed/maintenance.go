package devseed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/tenancies"
)

// Sample maintenance requests (fixed IDs b75x; the cost b761 and its ledger
// entry b762; the enquiry to Sample Plumbers b763).
var (
	// MaintenanceTapID is RPM-2026-900001: Flat 1's leaking tap, raised by
	// the sample tenant, ASSIGNED to Sample Plumbers (enquiry
	// MaintenanceEnquiryID) with one USD 85.00 cost.
	MaintenanceTapID = id("b751")
	// MaintenancePowerID is RPM-2026-900002: the cottage, IN_PROGRESS.
	MaintenancePowerID = id("b752")
	// MaintenanceOvenID is RPM-2026-900003: Flat 1, raised by the tenant, NEW.
	MaintenanceOvenID = id("b753")
	// MaintenanceHedgeID is RPM-2026-900004: Avondale Court as a whole, CLOSED.
	MaintenanceHedgeID = id("b754")

	MaintenanceCostID        = id("b761")
	MaintenanceCostEntryID   = id("b762")
	MaintenanceEnquiryID     = id("b763")
	maintenanceEnquiryRef    = "BE-RPM9TAPX"
	maintenancePlumbersSlug  = "sample-plumbers"
	maintenanceEnquiryKeyFmt = "seed-rpm-assign-%s"
)

// PlumbersProfile is the Sample Plumbers directory profile.
func PlumbersProfile() Profile {
	for _, p := range Profiles() {
		if p.Slug == maintenancePlumbersSlug {
			return p
		}
	}
	panic("devseed: no sample-plumbers profile")
}

// MaintenanceSamples returns the sample requests (with their timelines and
// the cost), the cost's EXPENSE ledger entry and the enquiry sent to Sample
// Plumbers. Times are relative to now.
func MaintenanceSamples(now time.Time) (maintenance.Change, ledger.Change, businessenquiries.NewEnquiry) {
	now = now.UTC().Truncate(time.Second)
	at := func(days int, hours int) time.Time {
		return now.AddDate(0, 0, days).Add(time.Duration(hours) * time.Hour)
	}
	tp := func(t time.Time) *time.Time { return &t }
	ptr := func(u uuid.UUID) *uuid.UUID { return &u }
	book := tenancies.OrganisationBook(SampleEstates.ID)
	tenant, chipo, rudo := TenantID, EstatesAgent2ID, EstatesOwnerID
	plumbers := PlumbersProfile()
	flat1, unit1, cottage := TenancyFlat1ID, UnitFlat1ID, TenancyCottageID

	ev := func(req uuid.UUID, n int, typ, kind string, who uuid.UUID, name string, when time.Time) maintenance.Event {
		return maintenance.Event{ID: uuid.NewSHA1(req, fmt.Appendf(nil, "event-%d", n)), RequestID: req, Type: typ, ActorKind: kind,
			ActorUserID: ptr(who), ActorName: name, At: when}
	}
	status := func(e maintenance.Event, from, to, note string) maintenance.Event {
		e.FromStatus, e.ToStatus, e.Text = from, to, note
		return e
	}
	created := func(e maintenance.Event) maintenance.Event { e.ToStatus = maintenance.StatusNew; return e }

	var ch maintenance.Change
	// 1. The tap: tenant-raised, triaged, assigned to Sample Plumbers, one cost.
	tap := maintenance.Request{ID: MaintenanceTapID, Reference: "RPM-2026-900001", Book: book, PropertyID: PropertyAvondaleID,
		UnitID: ptr(unit1), TenancyID: ptr(flat1), Category: "plumbing", CategoryLabel: "Plumbing", Priority: "high", PriorityLabel: "High",
		Title: "Kitchen tap leaking", Description: "The kitchen tap drips all the time and the cupboard under the sink is wet.",
		Status: maintenance.StatusAssigned, RaisedByKind: maintenance.ActorTenant, RaisedBy: ptr(tenant), RaisedByName: "Tariro Tenant",
		Provider:  &maintenance.Provider{ProfileID: ptr(plumbers.ID), Kind: plumbers.Kind, Slug: plumbers.Slug, Name: plumbers.Name},
		EnquiryID: ptr(MaintenanceEnquiryID), EnquiryReference: maintenanceEnquiryRef, Assignments: 1, AssignedAt: tp(at(-5, 3)),
		AssignedBy: ptr(chipo), AssignedByName: "Chipo Dube", StatusChangedAt: at(-5, 3), CreatedAt: at(-6, 0), UpdatedAt: at(-2, 0)}
	comment := ev(tap.ID, 2, maintenance.EventComment, maintenance.ActorTenant, tenant, "Tariro Tenant", at(-6, 1))
	comment.Text = "I have put a bowl under it for now."
	managerComment := ev(tap.ID, 4, maintenance.EventComment, maintenance.ActorManager, chipo, "Chipo Dube", at(-5, 2))
	managerComment.Text = "Thank you. A plumber will contact you to arrange a visit."
	assigned := ev(tap.ID, 6, maintenance.EventAssigned, maintenance.ActorManager, chipo, "Chipo Dube", at(-5, 3))
	assigned.Details = map[string]any{"provider": map[string]any{"id": plumbers.ID.String(), "kind": plumbers.Kind, "slug": plumbers.Slug,
		"name": plumbers.Name}, "enquiry_reference": maintenanceEnquiryRef}
	cost := maintenance.Cost{ID: MaintenanceCostID, RequestID: tap.ID, Amount: "85.00", Currency: "USD", Description: "Replace kitchen tap washer",
		Method: ledger.MethodBank, IncurredOn: at(-2, 0).Format(time.DateOnly), LedgerEntryID: MaintenanceCostEntryID, RecordedBy: ptr(chipo),
		RecordedByName: "Chipo Dube", RecordedAt: at(-2, 0)}
	costEv := ev(tap.ID, 7, maintenance.EventCostAdded, maintenance.ActorManager, chipo, "Chipo Dube", at(-2, 0))
	costEv.Details = map[string]any{"cost_id": cost.ID.String(), "amount": cost.Amount, "currency": cost.Currency, "description": cost.Description}
	ch.Events = append(ch.Events,
		created(ev(tap.ID, 1, maintenance.EventCreated, maintenance.ActorTenant, tenant, "Tariro Tenant", at(-6, 0))), comment,
		status(ev(tap.ID, 3, maintenance.EventStatusChanged, maintenance.ActorManager, chipo, "Chipo Dube", at(-5, 1)),
			maintenance.StatusNew, maintenance.StatusTriaged, ""),
		managerComment,
		status(ev(tap.ID, 5, maintenance.EventStatusChanged, maintenance.ActorManager, chipo, "Chipo Dube", at(-5, 3)),
			maintenance.StatusTriaged, maintenance.StatusAssigned, ""),
		assigned, costEv)
	ch.Costs = []maintenance.Cost{cost}

	// 2. The cottage's power: manager-raised, in progress.
	power := maintenance.Request{ID: MaintenancePowerID, Reference: "RPM-2026-900002", Book: book, PropertyID: PropertyCottageID,
		TenancyID: ptr(cottage), Category: "electrical", CategoryLabel: "Electrical", Priority: "urgent",
		PriorityLabel: "Urgent (safety, no water or power)", Title: "No power in the kitchen",
		Description: "The kitchen circuit trips as soon as it is reset.", Status: maintenance.StatusInProgress,
		RaisedByKind: maintenance.ActorManager, RaisedBy: ptr(chipo), RaisedByName: "Chipo Dube", StatusChangedAt: at(-1, 0),
		CreatedAt: at(-2, 2), UpdatedAt: at(-1, 0)}
	ch.Events = append(ch.Events,
		created(ev(power.ID, 1, maintenance.EventCreated, maintenance.ActorManager, chipo, "Chipo Dube", at(-2, 2))),
		status(ev(power.ID, 2, maintenance.EventStatusChanged, maintenance.ActorManager, chipo, "Chipo Dube", at(-2, 3)),
			maintenance.StatusNew, maintenance.StatusTriaged, ""),
		status(ev(power.ID, 3, maintenance.EventStatusChanged, maintenance.ActorManager, chipo, "Chipo Dube", at(-1, 0)),
			maintenance.StatusTriaged, maintenance.StatusInProgress, "Our own electrician is on it."))

	// 3. The oven: tenant-raised, new.
	oven := maintenance.Request{ID: MaintenanceOvenID, Reference: "RPM-2026-900003", Book: book, PropertyID: PropertyAvondaleID,
		UnitID: ptr(unit1), TenancyID: ptr(flat1), Category: "appliances", CategoryLabel: "Appliances", Priority: "normal",
		PriorityLabel: "Normal", Title: "Oven door hinge broken", Description: "The oven door no longer closes on the left side.",
		Status: maintenance.StatusNew, RaisedByKind: maintenance.ActorTenant, RaisedBy: ptr(tenant), RaisedByName: "Tariro Tenant",
		StatusChangedAt: at(0, -3), CreatedAt: at(0, -3), UpdatedAt: at(0, -3)}
	ch.Events = append(ch.Events, created(ev(oven.ID, 1, maintenance.EventCreated, maintenance.ActorTenant, tenant, "Tariro Tenant", at(0, -3))))

	// 4. The hedge: the whole property, closed.
	hedge := maintenance.Request{ID: MaintenanceHedgeID, Reference: "RPM-2026-900004", Book: book, PropertyID: PropertyAvondaleID,
		Category: "garden", CategoryLabel: "Garden and grounds", Priority: "low", PriorityLabel: "Low",
		Title: "Overgrown hedge on the boundary", Status: maintenance.StatusClosed, RaisedByKind: maintenance.ActorManager,
		RaisedBy: ptr(rudo), RaisedByName: "Rudo Moyo", StatusChangedAt: at(-20, 0), ResolvedAt: tp(at(-21, 0)), ClosedAt: tp(at(-20, 0)),
		CreatedAt: at(-30, 0), UpdatedAt: at(-20, 0)}
	ch.Events = append(ch.Events,
		created(ev(hedge.ID, 1, maintenance.EventCreated, maintenance.ActorManager, rudo, "Rudo Moyo", at(-30, 0))),
		status(ev(hedge.ID, 2, maintenance.EventStatusChanged, maintenance.ActorManager, rudo, "Rudo Moyo", at(-29, 0)),
			maintenance.StatusNew, maintenance.StatusTriaged, ""),
		status(ev(hedge.ID, 3, maintenance.EventStatusChanged, maintenance.ActorManager, rudo, "Rudo Moyo", at(-25, 0)),
			maintenance.StatusTriaged, maintenance.StatusInProgress, "The caretaker will cut it."),
		status(ev(hedge.ID, 4, maintenance.EventStatusChanged, maintenance.ActorManager, rudo, "Rudo Moyo", at(-21, 0)),
			maintenance.StatusInProgress, maintenance.StatusResolved, ""),
		status(ev(hedge.ID, 5, maintenance.EventStatusChanged, maintenance.ActorManager, rudo, "Rudo Moyo", at(-20, 0)),
			maintenance.StatusResolved, maintenance.StatusClosed, ""))
	ch.Requests = []maintenance.Request{tap, power, oven, hedge}

	property, listing := PropertyAvondaleID, propertyrequests.SeedListingID(2)
	entry := ledger.Entry{ID: MaintenanceCostEntryID, Book: ledger.OrganisationBook(SampleEstates.ID), Type: ledger.TypeExpense,
		Direction: ledger.Out, Category: ledger.CategoryMaintenance, Method: cost.Method, Amount: cost.Amount, Currency: cost.Currency,
		EffectiveDate: cost.IncurredOn, RecordedAt: cost.RecordedAt, AuthorUserID: ptr(chipo), AuthorName: "Chipo Dube",
		Status: ledger.StatusUnverified, ListingID: &listing, PropertyID: &property, Reference: tap.Reference,
		Description: "Maintenance " + tap.Reference + ": " + cost.Description}

	enquiry := businessenquiries.NewEnquiry{ID: MaintenanceEnquiryID, Reference: maintenanceEnquiryRef,
		MarketID: MarketID(MarketOf(plumbers.Organisation)), ProfileID: plumbers.ID, ProfileKind: plumbers.Kind, ProfileSlug: plumbers.Slug,
		ProfileName: plumbers.Name, Status: businessenquiries.StatusReceived, Service: "Plumbing",
		Message: maintenance.EnquiryMessage(tap, "Avondale Court", "Flat 1", "ZW", "Plumbing", "High",
			"Please call the office to arrange a visit."),
		PreferredContactMethod: businessenquiries.ContactEmail, IdempotencyKey: fmt.Sprintf(maintenanceEnquiryKeyFmt, tap.ID),
		RequestHash: "seed", CustomerUserID: ptr(chipo), GuestName: "Chipo Dube", GuestEmail: EstatesAgent2Email,
		PrivacyVersion: maintenance.DefaultEnquiryPrivacyVer, ConsentAcceptedAt: at(-5, 3),
		Recipient: &businessenquiries.Recipient{OrganisationID: plumbers.Organisation.ID}, CreatedAt: at(-5, 3)}
	return ch, ledger.Change{Entries: []ledger.Entry{entry}}, enquiry
}

// SeedMemoryMaintenance stores the sample requests in memory, with the
// cost's ledger entry and the enquiry to Sample Plumbers. After the
// tenancies, the ledger and the directory profiles.
func SeedMemoryMaintenance(ctx context.Context, store *maintenance.MemoryStore, ledgerStore *ledger.MemoryStore,
	enquiries *businessenquiries.MemoryStore, now time.Time) error {
	ch, lch, enq := MaintenanceSamples(now)
	if err := ledgerStore.Put(lch); err != nil {
		return err
	}
	if enquiries != nil { // nil: the enquiry is not stored (tests that count enquiries)
		if _, _, err := enquiries.Create(ctx, enq); err != nil {
			return err
		}
	}
	for _, r := range ch.Requests {
		r := r
		if err := store.Put(maintenance.Change{Create: &r}); err != nil {
			return err
		}
	}
	return store.Put(maintenance.Change{Events: ch.Events, Costs: ch.Costs})
}

// seedPostgresMaintenance writes the samples (needs migration 0037; after
// the tenancies and the ledger). The seeded requests are replaced (their
// dates are relative to now); the cost's ledger entry is posted once and
// never changed, and the enquiry is inserted once.
func seedPostgresMaintenance(ctx context.Context, tx pgx.Tx, now time.Time) error {
	ch, lch, enq := MaintenanceSamples(now)
	ids := []uuid.UUID{MaintenanceTapID, MaintenancePowerID, MaintenanceOvenID, MaintenanceHedgeID}
	for _, q := range []string{
		`DELETE FROM maintenance_costs WHERE request_id = ANY($1)`,
		`DELETE FROM maintenance_requests WHERE id = ANY($1)`,
	} {
		if _, err := tx.Exec(ctx, q, ids); err != nil {
			return fmt.Errorf("seed maintenance: %w", err)
		}
	}
	var date string
	err := tx.QueryRow(ctx, `SELECT effective_date::text FROM ledger_entries WHERE id = $1`, MaintenanceCostEntryID).Scan(&date)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := ledger.ApplyTx(ctx, tx, lch); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("seed maintenance cost: %w", err)
	default:
		ch.Costs[0].IncurredOn = date // posted once, with its first date
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO business_enquiries (id, reference, market_id, profile_id, status, service, message, preferred_contact_method,
			idempotency_key, request_hash, customer_user_id, guest_name, guest_email, consent_privacy_version, consent_accepted_at,
			recipient_organisation_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $17)
		ON CONFLICT (id) DO NOTHING`,
		enq.ID, enq.Reference, enq.MarketID, enq.ProfileID, enq.Status, enq.Service, enq.Message, enq.PreferredContactMethod,
		enq.IdempotencyKey, enq.RequestHash, enq.CustomerUserID, enq.GuestName, enq.GuestEmail, enq.PrivacyVersion, enq.ConsentAcceptedAt,
		enq.Recipient.OrganisationID, enq.CreatedAt); err != nil {
		return fmt.Errorf("seed maintenance enquiry: %w", err)
	}
	for _, r := range ch.Requests {
		r := r
		if err := maintenance.ApplyTx(ctx, tx, maintenance.Change{Create: &r}); err != nil {
			return err
		}
	}
	return maintenance.ApplyTx(ctx, tx, maintenance.Change{Events: ch.Events, Costs: ch.Costs})
}
