package devseed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Sample questionnaire and applications (fixed IDs, a1xx..a4xx).
var (
	// RentalFormV1ID is version 1 of Sample Estates' "Rental application":
	// RETIRED, with the tenant's SUBMITTED application on it, so the old
	// questions stay visible. RentalFormV2ID is version 2, PUBLISHED and
	// attached to two rent listings.
	RentalFormV1ID = id("a101")
	RentalFormV2ID = id("a102")
	// SubmittedApplicationID (on v1) and DraftApplicationID (on v2) belong
	// to the sample tenant (tenant@example.test).
	SubmittedApplicationID = id("a201")
	DraftApplicationID     = id("a202")
	// SubmittedApplicationReference is the seeded application's reference.
	SubmittedApplicationReference = "RPA-APP-2026-900001"
	sampleApplicationFileID       = id("a301")
)

// SampleApplicationFileKey is where the seeded proof of income is stored.
const SampleApplicationFileKey = "applications/seed0195c0de0000a301"

// applicationPDF is a tiny valid PDF ("Sample payslip").
var applicationPDF = []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
	"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 300 144]>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n")

func num(n float64) *float64 { return &n }
func count(n int) *int       { return &n }

// rentalSections returns the questions of version 1 or 2: version 2 asks
// about smoking and a previous landlord, and words the household question
// differently.
func rentalSections(version int) []applications.Section {
	yesNo := []applications.Option{{Value: "yes", Label: "Yes"}, {Value: "no", Label: "No"}}
	occupants := "How many people will live in the property?"
	if version == 2 {
		occupants = "How many adults and children will live there?"
	}
	about := applications.Section{Key: "about", Title: "About you", Description: str("We use this to see whether the property suits you."),
		Fields: []applications.Field{
			{Key: "move_in_date", Type: applications.TypeDate, Label: "When would you like to move in?", Required: true},
			{Key: "occupants", Type: applications.TypeNumber, Label: occupants, Required: true, Min: num(1), Max: num(12)},
			{Key: "employment_status", Type: applications.TypeSingleChoice, Label: "What is your employment status?", Required: true,
				Options: []applications.Option{{Value: "employed", Label: "Employed"}, {Value: "self_employed", Label: "Self-employed"},
					{Value: "student", Label: "Student"}, {Value: "other", Label: "Other"}}},
			{Key: "monthly_income", Type: applications.TypeNumber, Label: "Monthly household income (USD)", Help: str("Before tax. Optional."), Min: num(0)},
		}}
	household := applications.Section{Key: "household", Title: "Your household", Fields: []applications.Field{
		{Key: "has_pets", Type: applications.TypeSingleChoice, Label: "Do you have pets?", Required: true, Options: yesNo},
		{Key: "pets_details", Type: applications.TypeLongText, Label: "Describe them", Help: str("Kind, size and how many."), Required: true,
			MaxLength: count(1000), VisibleIf: &applications.VisibleIf{Field: "has_pets", Equals: str("yes")}},
	}}
	if version == 2 {
		household.Fields = append(household.Fields,
			applications.Field{Key: "smoker", Type: applications.TypeSingleChoice, Label: "Does anybody in the household smoke?", Required: true, Options: yesNo})
	}
	documents := applications.Section{Key: "documents", Title: "Documents", Description: str("PDF, JPEG or PNG, up to 10 MB each. Only the agency can open them."),
		Fields: []applications.Field{
			{Key: "proof_of_income", Type: applications.TypeFile, Label: "Proof of income (a recent payslip or bank letter)", Required: true,
				Accept: []string{applications.FilePDF, applications.FileJPG, applications.FilePNG}, MaxFiles: count(2)},
		}}
	out := []applications.Section{about, household, documents}
	if version == 2 {
		out = append(out, applications.Section{Key: "references", Title: "References", Fields: []applications.Field{
			{Key: "previous_landlord", Type: applications.TypeShortText, Label: "Previous landlord or agent (name and town)", MaxLength: count(200)},
		}})
	}
	return out
}

// ApplicationSamples is the sample data of the questionnaires and applications.
type ApplicationSamples struct {
	Forms        []applications.Form
	Intakes      []applications.Intake
	Applications []applications.Application
	Events       map[uuid.UUID][]applications.Event
	Files        []applications.File
}

// RentListings returns the first two published, verified rent listings of
// Sample Estates (not short stays): the listings the sample form is attached to.
func RentListings(now time.Time) []listings.Record {
	var out []listings.Record
	for _, r := range ListingRecords(now) {
		if r.OrganisationID != nil && *r.OrganisationID == SampleEstates.ID && r.Deal == listings.DealRent &&
			r.Category != listings.CategoryShortStay && r.Status == listings.StatusPublished && r.Advertiser.Verified {
			out = append(out, r)
			if len(out) == 2 {
				break
			}
		}
	}
	return out
}

// ApplicationSamplesAt returns the samples, relative to now.
func ApplicationSamplesAt(now time.Time) ApplicationSamples {
	now = now.UTC().Truncate(time.Second)
	at := func(days int) time.Time { return now.AddDate(0, 0, days) }
	tp := func(t time.Time) *time.Time { return &t }
	org := SampleEstates.ID
	owner := EstatesOwnerID
	base := applications.Form{FamilyID: RentalFormV1ID, OrganisationID: &org, MarketCode: MarketCode, Name: "Rental application",
		Purpose:       "To check that the property suits you and that you can afford the rent, before a lease is offered.",
		PrivacyNotice: "Sample Estates reads your answers and documents only to decide on this application. They are kept for one year after a decision and then deleted. Zimbabwe's Cyber and Data Protection Act applies.",
		RetentionDays: 365, CreatedBy: &owner, CreatedByName: "Rudo Moyo"}
	v1 := base
	v1.ID, v1.Version, v1.Status, v1.Sections = RentalFormV1ID, 1, applications.FormRetired, rentalSections(1)
	v1.CreatedAt, v1.UpdatedAt, v1.PublishedAt, v1.RetiredAt = at(-40), at(-10), tp(at(-39)), tp(at(-10))
	v2 := base
	v2.ID, v2.Version, v2.Status, v2.Sections = RentalFormV2ID, 2, applications.FormPublished, rentalSections(2)
	v2.CreatedAt, v2.UpdatedAt, v2.PublishedAt = at(-11), at(-10), tp(at(-10))

	out := ApplicationSamples{Forms: []applications.Form{v1, v2}, Events: map[uuid.UUID][]applications.Event{}}
	rent := RentListings(now)
	for _, r := range rent {
		out.Intakes = append(out.Intakes, applications.Intake{ListingID: r.ID, FormID: RentalFormV2ID, Status: applications.IntakeOpen, UpdatedBy: &owner, UpdatedAt: at(-10)})
	}
	if len(rent) < 2 {
		return out
	}
	card := func(r listings.Record) saved.Snapshot {
		slug := r.Slug
		return saved.Snapshot{Slug: &slug, Reference: r.Reference, Title: r.Title, Price: r.Price, Category: listings.Coded{Code: r.Category},
			Country: listings.Country{Code: r.MarketCode}}
	}
	tenant := TenantID
	submittedAt := at(-12)
	sub := applications.Application{ID: SubmittedApplicationID, Reference: SubmittedApplicationReference, ListingID: rent[0].ID,
		FormID: RentalFormV1ID, FormVersion: 1, ApplicantUserID: &tenant, ApplicantName: "Tariro Tenant", OrganisationID: &org,
		Status: applications.StatusSubmitted, Listing: card(rent[0]), CreatedAt: at(-13), UpdatedAt: submittedAt,
		SubmittedAt: &submittedAt, LastSubmittedAt: &submittedAt,
		Answers: applications.Answers{"move_in_date": now.AddDate(0, 1, 0).Format("2006-01-02"), "occupants": float64(2),
			"employment_status": "employed", "monthly_income": float64(1500), "has_pets": "yes",
			"pets_details": "One small, house-trained dog.", "proof_of_income": []string{sampleApplicationFileID.String()}}}
	sum := sha256.Sum256(applicationPDF)
	out.Files = []applications.File{{ID: sampleApplicationFileID, ApplicationID: sub.ID, FieldKey: "proof_of_income", FileName: "Payslip October.pdf",
		ContentType: "application/pdf", Size: int64(len(applicationPDF)), SHA256: hex.EncodeToString(sum[:]), ObjectKey: SampleApplicationFileKey, UploadedAt: at(-13)}}
	out.Events[sub.ID] = []applications.Event{
		{ID: id("a401"), ApplicationID: sub.ID, Kind: applications.EventCreated, ToStatus: applications.StatusDraft, ActorType: applications.ActorApplicant,
			ActorUserID: &tenant, ActorName: "Tariro Tenant", At: at(-13)},
		{ID: id("a402"), ApplicationID: sub.ID, Kind: applications.EventSubmitted, FromStatus: applications.StatusDraft, ToStatus: applications.StatusSubmitted,
			ActorType: applications.ActorApplicant, ActorUserID: &tenant, ActorName: "Tariro Tenant", At: submittedAt},
	}
	draft := applications.Application{ID: DraftApplicationID, ListingID: rent[1].ID, FormID: RentalFormV2ID, FormVersion: 2, ApplicantUserID: &tenant,
		ApplicantName: "Tariro Tenant", OrganisationID: &org, Status: applications.StatusDraft, Listing: card(rent[1]), CreatedAt: at(-1), UpdatedAt: at(-1),
		Answers: applications.Answers{"occupants": float64(1), "has_pets": "no"}}
	out.Events[draft.ID] = []applications.Event{{ID: id("a403"), ApplicationID: draft.ID, Kind: applications.EventCreated, ToStatus: applications.StatusDraft,
		ActorType: applications.ActorApplicant, ActorUserID: &tenant, ActorName: "Tariro Tenant", At: at(-1)}}
	out.Applications = []applications.Application{sub, draft}
	return out
}

// SeedMemoryApplications stores the samples in memory (no events, no audit).
// The seeded file's object is written by SeedApplicationFiles.
func SeedMemoryApplications(store *applications.MemoryStore, now time.Time) {
	s := ApplicationSamplesAt(now)
	for _, f := range s.Forms {
		store.PutForm(f)
	}
	for _, in := range s.Intakes {
		store.PutIntakeRecord(in)
	}
	for _, a := range s.Applications {
		var files []applications.File
		for _, f := range s.Files {
			if f.ApplicationID == a.ID {
				files = append(files, f)
			}
		}
		store.PutApplication(a, s.Events[a.ID], files)
	}
}

// SeedApplicationFiles writes the seeded application file to the storage.
func SeedApplicationFiles(ctx context.Context, files storage.Storage) error {
	if err := files.Put(ctx, SampleApplicationFileKey, bytes.NewReader(applicationPDF), int64(len(applicationPDF)), "application/pdf"); err != nil {
		return fmt.Errorf("seed application file: %w", err)
	}
	return nil
}

// seedPostgresApplications replaces the samples (needs migration 0031).
func seedPostgresApplications(ctx context.Context, tx pgx.Tx, files storage.Storage, now time.Time) error {
	s := ApplicationSamplesAt(now)
	appIDs := []uuid.UUID{SubmittedApplicationID, DraftApplicationID}
	formIDs := []uuid.UUID{RentalFormV1ID, RentalFormV2ID}
	// The trigger keeps a form with submissions: drop the applications first,
	// the seeded ones and any made since on the sample forms (on a re-run
	// against staging, testers' applications would otherwise block it).
	for _, q := range []string{
		`DELETE FROM contact_links WHERE kind = 'APPLICATION' AND item_id IN
			(SELECT id FROM applications WHERE id = ANY($1) OR form_id = ANY($2))`,
		`DELETE FROM applications WHERE id = ANY($1) OR form_id = ANY($2)`,
	} {
		if _, err := tx.Exec(ctx, q, appIDs, formIDs); err != nil {
			return fmt.Errorf("seed applications: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM listing_application_intake WHERE form_id = ANY($1)`, formIDs); err != nil {
		return fmt.Errorf("seed applications: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM forms WHERE id = ANY($1)`, formIDs); err != nil {
		return fmt.Errorf("seed applications: %w", err)
	}
	for _, f := range s.Forms {
		if err := applications.InsertFormTx(ctx, tx, f); err != nil {
			return err
		}
	}
	for _, in := range s.Intakes {
		if err := applications.PutIntakeTx(ctx, tx, in); err != nil {
			return err
		}
	}
	for _, a := range s.Applications {
		if err := applications.InsertApplicationTx(ctx, tx, a); err != nil {
			return err
		}
		for _, e := range s.Events[a.ID] {
			if err := applications.InsertEventTx(ctx, tx, e); err != nil {
				return err
			}
		}
	}
	for _, f := range s.Files {
		if err := applications.InsertFileTx(ctx, tx, f); err != nil {
			return err
		}
	}
	if files != nil {
		return SeedApplicationFiles(ctx, files)
	}
	return nil
}
