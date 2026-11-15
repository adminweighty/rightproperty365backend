package devseed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/storage"
)

// The lease samples: an APPROVED sample residential template for Zimbabwe
// and one for the United Kingdom, and a DRAFT lease by Tendai Ncube
// (agent@example.test) on his listing 2 with Tariro Tenant
// (tenant@example.test). The templates' legal review says plainly that
// they were NOT reviewed by a lawyer: they exist to try the screens.
var (
	SampleLeaseTemplateZW = id("e301")
	SampleLeaseTemplateGB = id("e302")
	SampleDraftLeaseID    = id("e311")
)

// SampleDraftLeaseReference is outside the range the sequence hands out first.
const SampleDraftLeaseReference = "RPL-2026-900001"

const sampleReviewNote = "SAMPLE — not reviewed by a lawyer; for demonstration on staging only."

func sampleReview() *leases.LegalReview {
	return &leases.LegalReview{ReviewerName: "SAMPLE — not reviewed by a lawyer", Organisation: "RightProperty365 sample data",
		ReviewedOn: "2026-11-01", Note: sampleReviewNote}
}

func sampleSections(extraDeposit string) []leases.Section {
	return []leases.Section{
		{Heading: "The parties and the property", Text: "This lease is made on {{agreement_date}} between {{landlord_names}} (the landlord) and " +
			"{{tenant_names}} (the tenant). The landlord lets the property at {{property_address}} to the tenant, as a home for the tenant and the " +
			"people who live with them."},
		{Heading: "Term", Text: "The lease runs {{term}}."},
		{Heading: "Rent", Text: "The tenant pays rent of {{rent_currency}} {{rent_amount}} per {{rent_period}}, due on {{payment_day}}, to the " +
			"account the landlord names in writing. The landlord gives a receipt for any payment made in cash."},
		{Heading: "Deposit", Text: "The tenant pays a deposit of {{deposit_amount}} before moving in. " + extraDeposit + "The landlord returns " +
			"it within 30 days after the lease ends, less the reasonable cost of repairing damage beyond fair wear and tear and any unpaid rent, " +
			"with a written list of every deduction."},
		{Heading: "Using and looking after the property", Text: "The tenant keeps the property clean and in good condition, uses it only as a " +
			"home, does not sublet it without the landlord's written consent, and tells the landlord promptly about any damage or repair needed."},
		{Heading: "Repairs", Text: "The landlord keeps the structure, the roof, the plumbing, the electrical installation and the fittings provided " +
			"with the property in good repair, and carries out repairs within a reasonable time after being told about them."},
		{Heading: "Access", Text: "The landlord may enter the property to inspect it or carry out repairs at a reasonable time, after giving the " +
			"tenant at least 24 hours' notice, except in an emergency."},
		{Heading: "Ending the lease", Text: "Either party may end the lease by giving the other at least {{notice_period_days}} days' notice in " +
			"writing, subject to the term above. When the lease ends, the tenant returns every key and leaves the property in the condition it " +
			"was in at the start, apart from fair wear and tear."},
		{Heading: "Special conditions", Text: "{{special_conditions}}"},
		{Heading: "General", Text: "This lease is the whole agreement between the parties about the property. A change is only valid when it " +
			"is made in writing and agreed by both parties. Notices may be given by email to the addresses in this lease."},
	}
}

func sampleClauses() []leases.Clause {
	return []leases.Clause{
		{Code: "NO_SMOKING", Title: "No smoking", Text: "Nobody smokes inside the property."},
		{Code: "PETS_ALLOWED", Title: "Pets", Text: "The tenant may keep pets agreed with the landlord in writing, and repairs any damage they cause."},
		{Code: "GARDEN_MAINTENANCE", Title: "Garden", Text: "The tenant keeps the garden tidy, cuts the grass and waters the plants."},
	}
}

// SampleLeaseTemplates returns the two approved sample templates.
func SampleLeaseTemplates(now time.Time) []leases.Template {
	approved := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	admin := ContentAdminID
	mk := func(id uuid.UUID, market, name, extra string) leases.Template {
		return leases.Template{ID: id, FamilyID: id, Version: 1, MarketCode: market, Name: name, LeaseKind: leases.KindResidential,
			Description: sampleReviewNote, Sections: sampleSections(extra), Clauses: sampleClauses(), Status: leases.TemplateApproved,
			LegalReview: sampleReview(), CreatedBy: &admin, CreatedByName: "Connor Content", ApprovedAt: &approved, ApprovedBy: &admin,
			ApprovedByName: "Connor Content", CreatedAt: approved, UpdatedAt: approved}
	}
	return []leases.Template{
		mk(SampleLeaseTemplateZW, "ZW", "Sample residential lease (Zimbabwe)", ""),
		mk(SampleLeaseTemplateGB, "GB", "Sample residential lease (United Kingdom)",
			"Where the law requires it, the landlord protects the deposit in an approved scheme and gives the tenant the information it requires. "),
	}
}

func sampleDraftLease(now time.Time) leases.Lease {
	created := now.Add(-2 * time.Hour)
	listing := propertyrequests.SeedListingID(2)
	org := SampleEstates.ID
	return leases.Lease{
		ID: SampleDraftLeaseID, Reference: SampleDraftLeaseReference, Status: leases.StatusDraft, TemplateID: SampleLeaseTemplateZW,
		TemplateVersion: 1, TemplateName: "Sample residential lease (Zimbabwe)", MarketCode: "ZW", LeaseKind: leases.KindResidential,
		OrganisationID: &org, OrganisationName: SampleEstates.Name, ListingID: &listing, ListingReference: "RP-ZW-10002",
		ListingTitle: "Two-bedroom garden apartment", CreatedBy: identity.SeedAgentUserID, CreatedByName: "Tendai Ncube",
		Input: leases.Input{
			TemplateID: SampleLeaseTemplateZW, ListingID: &listing, OrganisationID: &org,
			PropertyAddress: "Two-bedroom garden apartment, Avondale, Harare, Zimbabwe",
			Landlords:       []leases.Party{{Name: "Lloyd Landlord", Email: LandlordEmail}},
			Tenants:         []leases.Party{{Name: "Tariro Tenant", Email: TenantEmail}},
			StartDate:       "2026-12-01", EndDate: "2027-11-30", RentAmount: "950.00", RentCurrency: "USD", RentPeriod: leases.PeriodMonthly,
			PaymentDay: 1, DepositAmount: "950.00", NoticePeriodDays: 30, OptionalClauses: []string{"NO_SMOKING"},
		},
		Documents: map[string]leases.Document{}, CreatedAt: created, UpdatedAt: created,
	}
}

// sampleLeaseDocument renders a sample lease's PDF and stores it.
func sampleLeaseDocument(ctx context.Context, files storage.Storage, l leases.Lease, t leases.Template, mode, kind, key string) (leases.Document, leases.Layout, error) {
	snap := leases.Snapshot{Reference: l.Reference, AgreementDate: l.UpdatedAt.Format(time.DateOnly), GeneratedAt: l.UpdatedAt, CountryName: "Zimbabwe",
		OrganisationName: l.OrganisationName, PreparedBy: l.CreatedByName,
		Template: leases.SnapshotTemplate{ID: t.ID.String(), Version: t.Version, Name: t.Name, LeaseKind: t.LeaseKind, MarketCode: t.MarketCode,
			Sections: t.Sections, Clauses: t.Clauses}, Input: l.Input}
	content, layout, err := leases.RenderPDF(snap, mode)
	if err != nil {
		return leases.Document{}, leases.Layout{}, err
	}
	if err := files.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/pdf"); err != nil {
		return leases.Document{}, leases.Layout{}, fmt.Errorf("store sample lease: %w", err)
	}
	raw, _ := json.Marshal(snap)
	return leases.Document{Kind: kind, TemplateID: t.ID, TemplateVersion: t.Version, Snapshot: raw, ObjectKey: key, SHA256: leases.Checksum(content),
		Size: int64(len(content)), GeneratedAt: l.UpdatedAt}, layout, nil
}

func seedLeaseEvent(l leases.Lease, action string, at time.Time, detail map[string]any) leases.Event {
	agent := identity.SeedAgentUserID
	return leases.Event{ID: uuid.New(), LeaseID: l.ID, Action: action, ActorType: leases.ActorUser, ActorUserID: &agent, ActorName: "Tendai Ncube", Detail: detail, At: at}
}

// SeedMemoryLeases creates the lease samples in memory.
func SeedMemoryLeases(ctx context.Context, store *leases.MemoryStore, files storage.Storage, now time.Time) error {
	now = now.UTC().Truncate(time.Microsecond)
	templates := SampleLeaseTemplates(now)
	for _, t := range templates {
		store.PutTemplate(t)
	}
	l := sampleDraftLease(now)
	doc, _, err := sampleLeaseDocument(ctx, files, l, templates[0], leases.ModeDraft, leases.DocumentDraft, leases.ObjectKeyPrefix+"/devseeddraft1")
	if err != nil {
		return err
	}
	l.Documents[leases.DocumentDraft] = doc
	store.PutLease(l, seedLeaseEvent(l, leases.EventCreated, l.CreatedAt, nil),
		seedLeaseEvent(l, leases.EventPDFGenerated, l.CreatedAt, map[string]any{"kind": doc.Kind, "sha256": doc.SHA256}))
	return nil
}

// seedPostgresLeases creates the same samples in Postgres (needs migration
// 0022). A sample that is already there is left alone.
func seedPostgresLeases(ctx context.Context, tx pgx.Tx, files storage.Storage, now time.Time) error {
	templates := SampleLeaseTemplates(now)
	for _, t := range templates {
		sections, _ := json.Marshal(t.Sections)
		clauses, _ := json.Marshal(t.Clauses)
		review, _ := json.Marshal(t.LegalReview)
		if _, err := tx.Exec(ctx, `
			INSERT INTO lease_templates (id, family_id, version, market_code, name, lease_kind, description, sections, optional_clauses, status,
				legal_review, created_by, created_by_name, approved_at, approved_by, approved_by_name, created_at, updated_at)
			SELECT $1, $1, 1, $2, $3, $4, $5, $6, $7, 'APPROVED', $8, $9, $10, $11, $9, $10, $11, $11
			WHERE NOT EXISTS (SELECT 1 FROM lease_templates WHERE id = $1)`,
			t.ID, t.MarketCode, t.Name, t.LeaseKind, t.Description, string(sections), string(clauses), string(review),
			t.CreatedBy, t.CreatedByName, *t.ApprovedAt); err != nil {
			return fmt.Errorf("seed lease template %s: %w", t.MarketCode, err)
		}
	}
	l := sampleDraftLease(now.UTC().Truncate(time.Microsecond))
	input, _ := json.Marshal(l.Input)
	tag, err := tx.Exec(ctx, `
		INSERT INTO leases (id, reference, status, template_id, template_version, template_name, market_code, lease_kind, organisation_id,
			organisation_name, listing_id, listing_reference, listing_title, created_by, created_by_name, input_snapshot, created_at, updated_at)
		SELECT $1, $2, 'DRAFT', $3, 1, $4, 'ZW', 'RESIDENTIAL', $5, $6, $7, $8, $9, $10, $11, $12, $13, $13
		WHERE NOT EXISTS (SELECT 1 FROM leases WHERE id = $1 OR reference = $2)`,
		l.ID, l.Reference, l.TemplateID, l.TemplateName, l.OrganisationID, l.OrganisationName, l.ListingID, l.ListingReference, l.ListingTitle,
		l.CreatedBy, l.CreatedByName, string(input), l.CreatedAt)
	if err != nil {
		return fmt.Errorf("seed draft lease: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	doc, _, err := sampleLeaseDocument(ctx, files, l, templates[0], leases.ModeDraft, leases.DocumentDraft, leases.ObjectKeyPrefix+"/devseeddraft1")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO lease_documents (id, lease_id, kind, template_id, template_version, input_snapshot, object_key, sha256, size_bytes, created_at)
		VALUES ($1, $2, 'DRAFT', $3, 1, $4, $5, $6, $7, $8)`,
		uuid.New(), l.ID, l.TemplateID, string(doc.Snapshot), doc.ObjectKey, doc.SHA256, doc.Size, l.CreatedAt); err != nil {
		return fmt.Errorf("seed draft lease document: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO lease_events (id, lease_id, action, actor_type, actor_user_id, actor_name, created_at)
		VALUES ($1, $2, 'CREATED', 'USER', $3, 'Tendai Ncube', $4)`, uuid.New(), l.ID, l.CreatedBy, l.CreatedAt); err != nil {
		return fmt.Errorf("seed draft lease event: %w", err)
	}
	return nil
}
