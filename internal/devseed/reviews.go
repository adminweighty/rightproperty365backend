package devseed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/reviews"
	"rightproperty365.com/backend/internal/modules/tenancies"
)

// Sample reviews of Sample Plumbers (API 0.36.4; fixed IDs b755, b764,
// b78x, b79x, b7a1):
//
//	RPM-2026-900005  Avondale Court (whole property), "Blocked courtyard drain", assigned to
//	                 Sample Plumbers (enquiry BE-RPM9DRNX), CLOSED 24 days ago
//	enquiries        Casey Consumer (40 days ago), Grace Mhlanga (30 days ago) and the sample
//	                 tenant (20 days ago, not reviewed: the review form shows), all replied
//	reviews          Sample Estates on the drain, 5 stars, PUBLISHED, with the provider's reply;
//	                 Casey C., 4 stars, PUBLISHED and REPORTED by the tenant (in the queue);
//	                 Grace M., 3 stars, PENDING (in the queue)
//
// Sample Plumbers' published rating is therefore "4.5" from 2 reviews.
var (
	// MaintenanceDrainID is RPM-2026-900005, the job Sample Estates reviewed.
	MaintenanceDrainID       = id("b755")
	maintenanceDrainEnquiry  = id("b764")
	maintenanceDrainRef      = "BE-RPM9DRNX"
	ReviewEnquiryConsumerID  = id("b781")
	ReviewEnquirySellerID    = id("b782")
	ReviewEnquiryTenantID    = id("b783")
	ReviewEstatesID          = id("b791")
	ReviewConsumerID         = id("b792")
	ReviewSellerID           = id("b793")
	reviewReportID           = id("b7a1")
	reviewEnquiryKeyFmt      = "seed-review-enquiry-%s"
	reviewSeedEnquiryVersion = "seed-1"
)

func accountByEmail(email string) Account {
	for _, a := range Accounts() {
		if a.Email == email {
			return a
		}
	}
	panic("devseed: no account " + email)
}

// ReviewSamples returns the drain job (with its timeline), the enquiries
// the reviews rest on and the reviews with their report. Times are relative
// to now.
func ReviewSamples(now time.Time) (maintenance.Change, []businessenquiries.NewEnquiry, []reviews.Review, reviews.Report) {
	now = now.UTC().Truncate(time.Second)
	at := func(days, hours int) time.Time { return now.AddDate(0, 0, days).Add(time.Duration(hours) * time.Hour) }
	tp := func(t time.Time) *time.Time { return &t }
	ptr := func(u uuid.UUID) *uuid.UUID { return &u }
	plumbers := PlumbersProfile()
	owner := accountByEmail(BusinessOwnerEmail(plumbers.Slug))
	rudo := EstatesOwnerID
	market := MarketID(MarketOf(plumbers.Organisation))

	// The drain: assigned to Sample Plumbers, resolved and closed.
	drain := maintenance.Request{ID: MaintenanceDrainID, Reference: "RPM-2026-900005", Book: tenancies.OrganisationBook(SampleEstates.ID),
		PropertyID: PropertyAvondaleID, Category: "plumbing", CategoryLabel: "Plumbing", Priority: "normal", PriorityLabel: "Normal",
		Title: "Blocked courtyard drain", Description: "Water pools in the courtyard after rain.", Status: maintenance.StatusClosed,
		RaisedByKind: maintenance.ActorManager, RaisedBy: ptr(rudo), RaisedByName: "Rudo Moyo",
		Provider:  &maintenance.Provider{ProfileID: ptr(plumbers.ID), Kind: plumbers.Kind, Slug: plumbers.Slug, Name: plumbers.Name},
		EnquiryID: ptr(maintenanceDrainEnquiry), EnquiryReference: maintenanceDrainRef, Assignments: 1, AssignedAt: tp(at(-33, 0)),
		AssignedBy: ptr(rudo), AssignedByName: "Rudo Moyo", StatusChangedAt: at(-24, 0), ResolvedAt: tp(at(-26, 0)), ClosedAt: tp(at(-24, 0)),
		CreatedAt: at(-35, 0), UpdatedAt: at(-24, 0)}
	ev := func(n int, typ string, when time.Time, from, to string) maintenance.Event {
		return maintenance.Event{ID: uuid.NewSHA1(drain.ID, fmt.Appendf(nil, "event-%d", n)), RequestID: drain.ID, Type: typ,
			ActorKind: maintenance.ActorManager, ActorUserID: ptr(rudo), ActorName: "Rudo Moyo", FromStatus: from, ToStatus: to, At: when}
	}
	assigned := ev(4, maintenance.EventAssigned, at(-33, 0), "", "")
	assigned.Details = map[string]any{"provider": map[string]any{"id": plumbers.ID.String(), "kind": plumbers.Kind, "slug": plumbers.Slug,
		"name": plumbers.Name}, "enquiry_reference": maintenanceDrainRef}
	mch := maintenance.Change{Create: &drain, Events: []maintenance.Event{
		ev(1, maintenance.EventCreated, at(-35, 0), "", maintenance.StatusNew),
		ev(2, maintenance.EventStatusChanged, at(-34, 0), maintenance.StatusNew, maintenance.StatusTriaged),
		ev(3, maintenance.EventStatusChanged, at(-33, 0), maintenance.StatusTriaged, maintenance.StatusAssigned),
		assigned,
		ev(5, maintenance.EventStatusChanged, at(-30, 0), maintenance.StatusAssigned, maintenance.StatusInProgress),
		ev(6, maintenance.EventStatusChanged, at(-26, 0), maintenance.StatusInProgress, maintenance.StatusResolved),
		ev(7, maintenance.EventStatusChanged, at(-24, 0), maintenance.StatusResolved, maintenance.StatusClosed),
	}}

	enquiry := func(id uuid.UUID, ref string, customer uuid.UUID, name, email, message string, created time.Time,
		version string, by *uuid.UUID) businessenquiries.NewEnquiry {
		replied := created.Add(26 * time.Hour)
		return businessenquiries.NewEnquiry{ID: id, Reference: ref, MarketID: market, ProfileID: plumbers.ID, ProfileKind: plumbers.Kind,
			ProfileSlug: plumbers.Slug, ProfileName: plumbers.Name, Status: businessenquiries.StatusReceived, Service: "Plumbing",
			Message: message, PreferredContactMethod: businessenquiries.ContactEmail, IdempotencyKey: fmt.Sprintf(reviewEnquiryKeyFmt, id),
			RequestHash: "seed", CustomerUserID: ptr(customer), GuestName: name, GuestEmail: email, PrivacyVersion: version,
			ConsentAcceptedAt: created, Recipient: &businessenquiries.Recipient{OrganisationID: plumbers.Organisation.ID}, CreatedAt: created,
			RepliedAt: &replied, RepliedBy: by}
	}
	enquiries := []businessenquiries.NewEnquiry{
		enquiry(maintenanceDrainEnquiry, maintenanceDrainRef, rudo, "Rudo Moyo", EstatesOwnerEmail,
			maintenance.EnquiryMessage(drain, "Avondale Court", "", "ZW", "Plumbing", "Normal", ""), at(-33, 0),
			maintenance.DefaultEnquiryPrivacyVer, ptr(owner.ID)),
		enquiry(ReviewEnquiryConsumerID, "BE-RVW2CSMR", identity.SeedConsumerUserID, "Casey Consumer", identity.SeedConsumerEmail,
			"Could you quote for replacing a geyser element?", at(-40, 0), reviewSeedEnquiryVersion, ptr(owner.ID)),
		enquiry(ReviewEnquirySellerID, "BE-RVW3SLLR", SellerID, "Grace Mhlanga", SellerEmail,
			"The outside tap leaks. Can you fix it this week?", at(-30, 0), reviewSeedEnquiryVersion, ptr(owner.ID)),
		enquiry(ReviewEnquiryTenantID, "BE-RVW4TNNT", TenantID, "Tariro Tenant", TenantEmail,
			"Do you install water filters?", at(-20, 0), reviewSeedEnquiryVersion, ptr(owner.ID)),
	}

	moderator := ModeratorID
	base := func(rid uuid.UUID, kind string, user uuid.UUID, name string, basisKind string, basisID uuid.UUID, basisRef string,
		rating int, text string, created time.Time) reviews.Review {
		return reviews.Review{ID: rid, ProfileID: plumbers.ID, ProviderOrganisationID: plumbers.Organisation.ID, ProviderKind: plumbers.Kind,
			ProviderSlug: plumbers.Slug, ProviderName: plumbers.Name, ReviewerKind: kind, ReviewerUserID: ptr(user), ReviewerName: name,
			BasisKind: basisKind, BasisID: basisID, BasisReference: basisRef, Rating: rating, Text: text, Status: reviews.StatusPending,
			Version: 1, CreatedAt: created, UpdatedAt: created}
	}
	publish := func(r reviews.Review, when time.Time) reviews.Review {
		r.Status, r.PublishedAt, r.ModeratedAt, r.ModeratedBy, r.ModeratedByName, r.UpdatedAt = reviews.StatusPublished, tp(when), tp(when),
			ptr(moderator), "Morgan Moderator", when
		return r
	}

	estates := publish(base(ReviewEstatesID, reviews.ReviewerOrganisation, rudo, SampleEstates.Name, reviews.BasisMaintenance,
		MaintenanceDrainID, drain.Reference, 5,
		"Came the next morning, cleared the drain and showed the caretaker how to keep it clear. Fair price.", at(-23, 0)), at(-22, 0))
	estates.ReviewerOrganisationID, estates.AuthorName = ptr(SampleEstates.ID), "Rudo Moyo"
	estates.Reply = &reviews.Reply{Text: "Thank you, Rudo. We are glad the courtyard is dry again.", At: at(-21, 0), UpdatedAt: at(-21, 0),
		ByUserID: ptr(owner.ID), ByName: owner.Name}
	estates.UpdatedAt = at(-21, 0)

	consumer := publish(base(ReviewConsumerID, reviews.ReviewerCustomer, identity.SeedConsumerUserID, reviews.PublicName("Casey Consumer"),
		reviews.BasisEnquiry, ReviewEnquiryConsumerID, "BE-RVW2CSMR", 4,
		"Quick to answer and the quote was clear. The plumber arrived an hour late but did a tidy job.", at(-12, 0)), at(-11, 0))

	seller := base(ReviewSellerID, reviews.ReviewerCustomer, SellerID, reviews.PublicName("Grace Mhlanga"), reviews.BasisEnquiry,
		ReviewEnquirySellerID, "BE-RVW3SLLR", 3, "The leak is fixed, but it took two visits and a lot of reminders to get there.", at(-2, 0))

	report := reviews.Report{ID: reviewReportID, ReviewID: consumer.ID, ReporterUserID: ptr(TenantID),
		Reason: "This review mentions a visit I do not think happened.", Status: reviews.ReportOpen, CreatedAt: at(-1, 0)}

	return mch, enquiries, []reviews.Review{estates, consumer, seller}, report
}

// SeedMemoryReviews stores the samples in memory: the drain job, the
// enquiries (enquiries nil: not stored, for tests that count enquiries) and
// the reviews with their report. After the maintenance samples.
func SeedMemoryReviews(ctx context.Context, store *reviews.MemoryStore, maint *maintenance.MemoryStore,
	enquiries *businessenquiries.MemoryStore, now time.Time) error {
	mch, enqs, list, report := ReviewSamples(now)
	if err := maint.Put(maintenance.Change{Create: mch.Create}); err != nil {
		return err
	}
	if err := maint.Put(maintenance.Change{Events: mch.Events}); err != nil {
		return err
	}
	if enquiries != nil {
		for _, e := range enqs {
			if _, _, err := enquiries.Create(ctx, e); err != nil {
				return err
			}
		}
	}
	for _, r := range list {
		r := r
		if err := store.Put(reviews.Change{Create: &r}); err != nil {
			return err
		}
	}
	return store.Put(reviews.Change{Reports: []reviews.Report{report}})
}

// seedPostgresReviews writes the samples (needs migration 0039; after the
// maintenance samples). Re-runnable: the drain job and the reviews (with
// their report) are replaced, their dates being relative to now; the
// enquiries are inserted once and keep their first dates.
func seedPostgresReviews(ctx context.Context, tx pgx.Tx, now time.Time) error {
	mch, enqs, list, report := ReviewSamples(now)
	reviewIDs := make([]uuid.UUID, 0, len(list))
	for _, r := range list {
		reviewIDs = append(reviewIDs, r.ID)
	}
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`DELETE FROM provider_reviews WHERE id = ANY($1)`, reviewIDs},
		{`DELETE FROM maintenance_requests WHERE id = $1`, MaintenanceDrainID},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.arg); err != nil {
			return fmt.Errorf("seed reviews: %w", err)
		}
	}
	for _, e := range enqs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO business_enquiries (id, reference, market_id, profile_id, status, service, message, preferred_contact_method,
				idempotency_key, request_hash, customer_user_id, guest_name, guest_email, consent_privacy_version, consent_accepted_at,
				recipient_organisation_id, created_at, updated_at, replied_at, replied_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $17, $18, $19)
			ON CONFLICT (id) DO NOTHING`,
			e.ID, e.Reference, e.MarketID, e.ProfileID, e.Status, e.Service, e.Message, e.PreferredContactMethod,
			e.IdempotencyKey, e.RequestHash, e.CustomerUserID, e.GuestName, e.GuestEmail, e.PrivacyVersion, e.ConsentAcceptedAt,
			e.Recipient.OrganisationID, e.CreatedAt, e.RepliedAt, e.RepliedBy); err != nil {
			return fmt.Errorf("seed review enquiry: %w", err)
		}
	}
	if err := maintenance.ApplyTx(ctx, tx, maintenance.Change{Create: mch.Create}); err != nil {
		return err
	}
	if err := maintenance.ApplyTx(ctx, tx, maintenance.Change{Events: mch.Events}); err != nil {
		return err
	}
	for _, r := range list {
		r := r
		if err := reviews.ApplyTx(ctx, tx, reviews.Change{Create: &r}); err != nil {
			return err
		}
	}
	return reviews.ApplyTx(ctx, tx, reviews.Change{Reports: []reviews.Report{report}})
}
