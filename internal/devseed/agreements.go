package devseed

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/storage"
)

// The agreement samples, both OUT_FOR_SIGNATURE and sent by Tendai Ncube
// (agent@example.test):
//
//   - an uploaded PDF, "Sample inventory and condition report" (drawn
//     here), signed first by Tariro Tenant (tenant@example.test, order 1),
//     then approved by Lloyd Landlord (landlord@example.test, order 2),
//     with Rudo Moyo (estates.owner@example.test) in copy;
//   - the lease RPL-2026-900002 on listing 5, sent for signing to Lloyd
//     Landlord and Tariro Tenant (both order 1), with their initials and
//     signature fields placed by the lease renderer.
//
// Nobody holds their links (only the SHA-256 of random tokens is stored). A
// signed-in recipient opens their document from "Documents to sign"
// (POST /v1/me/signing-requests/{recipientId}/link); the sender can resend
// with "remind" (in development the link is in the API log).
var (
	SampleUploadAgreementID = id("e401")
	SampleLeaseAgreementID  = id("e402")
	SampleSentLeaseID       = id("e312")
)

// SampleSentLeaseReference is the reference of the lease out for signature.
const SampleSentLeaseReference = "RPL-2026-900002"

// Sample agreement references (outside the range the sequence hands out first).
const (
	SampleUploadAgreementReference = "RPA-2026-900001"
	SampleLeaseAgreementReference  = "RPA-2026-900002"
)

// SamplePDF draws the small one-page PDF of the uploaded sample agreement.
func SamplePDF(title string) ([]byte, error) {
	f := fpdf.New("P", "pt", "A4", "")
	f.SetCatalogSort(true)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	f.SetCreationDate(at)
	f.SetModificationDate(at)
	f.AddPage()
	f.SetFont("Helvetica", "B", 18)
	f.Text(56, 80, title)
	f.SetFont("Helvetica", "", 11)
	y := 120.0
	for _, line := range []string{
		"Property: Two-bedroom garden apartment, Avondale, Harare.",
		"Living room: walls freshly painted, two windows intact, curtains clean.",
		"Kitchen: stove and fridge working, one cupboard door loose.",
		"Bedrooms: carpets clean, built-in cupboards complete.",
		"Bathroom: no leaks, tiles intact.",
		"SAMPLE document for demonstration on staging only.",
	} {
		f.Text(56, y, line)
		y += 20
	}
	f.SetDrawColor(160, 160, 160)
	for i, label := range []string{"Tenant signature", "Date", "Landlord approval"} {
		f.Rect(56, 520+float64(i)*70, 220, 40, "D")
		f.Text(56, 575+float64(i)*70, label)
	}
	var buf bytes.Buffer
	if err := f.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func randomHash() []byte {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	sum := sha256.Sum256(raw)
	return sum[:]
}

func seedEvent(a agreements.Agreement, action, actorType, actor string, at time.Time, r *agreements.Recipient) agreements.Event {
	e := agreements.Event{ID: uuid.New(), AgreementID: a.ID, Action: action, ActorType: actorType, ActorName: actor, At: at}
	if actorType == agreements.ActorUser {
		agent := identity.SeedAgentUserID
		e.ActorUserID = &agent
	}
	if r != nil {
		id := r.ID
		e.RecipientID = &id
		e.Detail = map[string]any{"to": r.Email}
	}
	return e
}

func sampleAgreement(id uuid.UUID, ref, name, source string, sent time.Time, doc agreements.Document, pages []agreements.PageSize,
	recipients []agreements.Recipient, fields []agreements.Field, leaseID *uuid.UUID) (agreements.Agreement, []agreements.Event) {
	org := SampleEstates.ID
	a := agreements.Agreement{ID: id, Reference: ref, Name: name, Status: agreements.StatusOutForSignature, Source: source, LeaseID: leaseID,
		SenderID: identity.SeedAgentUserID, SenderName: "Tendai Ncube", SenderEmail: identity.SeedAgentEmail, OrganisationID: &org,
		OrganisationName: SampleEstates.Name, ExpiresAt: sent.Add(agreements.DefaultExpiry), ReminderFrequency: agreements.ReminderNone,
		Original: &doc, Pages: pages, Recipients: recipients, Fields: fields, CreatedAt: sent.Add(-time.Hour), UpdatedAt: sent, SentAt: &sent}
	events := []agreements.Event{seedEvent(a, agreements.EventCreated, agreements.ActorUser, "Tendai Ncube", a.CreatedAt, nil),
		seedEvent(a, agreements.EventSent, agreements.ActorUser, "Tendai Ncube", sent, nil)}
	for i := range a.Recipients {
		r := &a.Recipients[i]
		if r.Status == agreements.RecipientActive {
			r.ActivatedAt = &sent
			a.Links = append(a.Links, agreements.Link{ID: uuid.New(), RecipientID: r.ID, TokenHash: randomHash(), ExpiresAt: a.ExpiresAt, CreatedAt: sent})
			events = append(events, seedEvent(a, agreements.EventEmailed, agreements.ActorSystem, "RightProperty365", sent, r))
		}
	}
	return a, events
}

// sampleUploadAgreement builds the uploaded sample (its PDF stored under key).
func sampleUploadAgreement(ctx context.Context, files storage.Storage, now time.Time) (agreements.Agreement, []agreements.Event, error) {
	content, err := SamplePDF("Sample inventory and condition report")
	if err != nil {
		return agreements.Agreement{}, nil, err
	}
	key := agreements.DocumentKeyPrefix + "/devseedupload1"
	if err := files.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/pdf"); err != nil {
		return agreements.Agreement{}, nil, fmt.Errorf("store sample agreement: %w", err)
	}
	pages, err := agreements.InspectPDF(content)
	if err != nil {
		return agreements.Agreement{}, nil, err
	}
	tenant, landlord, owner := id("e411"), id("e412"), id("e413")
	recipients := []agreements.Recipient{
		{ID: tenant, Order: 1, Role: agreements.RoleSigner, Name: "Tariro Tenant", Email: TenantEmail, Status: agreements.RecipientActive},
		{ID: landlord, Order: 2, Role: agreements.RoleApprover, Name: "Lloyd Landlord", Email: LandlordEmail, Status: agreements.RecipientWaiting},
		{ID: owner, Order: 3, Role: agreements.RoleCC, Name: "Rudo Moyo", Email: EstatesOwnerEmail, Status: agreements.RecipientCC},
	}
	fields := []agreements.Field{
		{ID: id("e421"), RecipientID: tenant, Type: agreements.FieldSignature, Page: 1, X: 56, Y: 520, W: 220, H: 40, Required: true},
		{ID: id("e422"), RecipientID: tenant, Type: agreements.FieldDateSigned, Page: 1, X: 56, Y: 590, W: 220, H: 40, Required: true},
		{ID: id("e423"), RecipientID: landlord, Type: agreements.FieldText, Page: 1, X: 56, Y: 660, W: 220, H: 40, Required: false, Label: "Comment"},
	}
	sent := now.Add(-26 * time.Hour)
	a, events := sampleAgreement(SampleUploadAgreementID, SampleUploadAgreementReference, "Sample inventory and condition report",
		agreements.SourceUpload, sent, agreements.Document{ObjectKey: key, SHA256: agreements.Checksum(content), Size: int64(len(content)),
			FileName: "inventory-and-condition-report.pdf"}, pages, recipients, fields, nil)
	return a, events, nil
}

func sampleSentLease(now time.Time) leases.Lease {
	l := sampleDraftLease(now)
	listing := propertyrequests.SeedListingID(5)
	sent := now.Add(-3 * time.Hour)
	l.ID, l.Reference, l.Status, l.ListingID, l.ListingReference, l.ListingTitle = SampleSentLeaseID, SampleSentLeaseReference,
		leases.StatusSentForSigning, &listing, "RP-ZW-10005", "Three-bedroom townhouse"
	l.Input.ListingID, l.Input.PropertyAddress = &listing, "Three-bedroom townhouse, Mount Pleasant, Harare, Zimbabwe"
	l.Input.RentAmount, l.Input.DepositAmount, l.Input.StartDate, l.Input.EndDate = "1600.00", "1600.00", "2027-01-01", "2027-12-31"
	l.CreatedAt, l.UpdatedAt, l.SentAt = sent.Add(-time.Hour), sent, &sent
	agent := identity.SeedAgentUserID
	l.SentBy = &agent
	l.Documents = map[string]leases.Document{}
	return l
}

// sampleLeaseAgreement renders the sent lease's signing copy and builds its agreement.
func sampleLeaseAgreement(ctx context.Context, files storage.Storage, l leases.Lease, t leases.Template) (leases.Document, agreements.Agreement, []agreements.Event, error) {
	doc, layout, err := sampleLeaseDocument(ctx, files, l, t, leases.ModeSigning, leases.DocumentSigning, leases.ObjectKeyPrefix+"/devseedsigning2")
	if err != nil {
		return leases.Document{}, agreements.Agreement{}, nil, err
	}
	keys := map[string]uuid.UUID{"LANDLORD-1": id("e431"), "TENANT-1": id("e432")}
	recipients := []agreements.Recipient{
		{ID: keys["LANDLORD-1"], Order: 1, Role: agreements.RoleSigner, Name: "Lloyd Landlord", Email: LandlordEmail, Label: "Landlord 1", Status: agreements.RecipientActive},
		{ID: keys["TENANT-1"], Order: 1, Role: agreements.RoleSigner, Name: "Tariro Tenant", Email: TenantEmail, Label: "Tenant 1", Status: agreements.RecipientActive},
	}
	var pages []agreements.PageSize
	for _, p := range layout.Pages {
		pages = append(pages, agreements.PageSize{Width: p.Width, Height: p.Height})
	}
	var fields []agreements.Field
	for i, f := range layout.Fields {
		fields = append(fields, agreements.Field{ID: uuid.NewSHA1(SampleLeaseAgreementID, []byte(fmt.Sprint(i))), RecipientID: keys[fmt.Sprintf("%s-%d", f.Role, f.Position)],
			Type: f.Type, Page: f.Page, X: f.X, Y: f.Y, W: f.W, H: f.H, Required: true})
	}
	a, events := sampleAgreement(SampleLeaseAgreementID, SampleLeaseAgreementReference, "Lease "+l.Reference+": "+l.Input.PropertyAddress,
		agreements.SourceLease, *l.SentAt, agreements.Document{ObjectKey: doc.ObjectKey, SHA256: doc.SHA256, Size: doc.Size, FileName: l.Reference + ".pdf"},
		pages, recipients, fields, &l.ID)
	return doc, a, events, nil
}

// SeedMemoryAgreements creates the agreement samples (and the lease out
// for signature) in memory. Call it after SeedMemoryLeases.
func SeedMemoryAgreements(ctx context.Context, store *agreements.MemoryStore, leaseStore *leases.MemoryStore, files storage.Storage, now time.Time) error {
	now = now.UTC().Truncate(time.Microsecond)
	a, events, err := sampleUploadAgreement(ctx, files, now)
	if err != nil {
		return err
	}
	store.PutAgreement(a, events...)
	l := sampleSentLease(now)
	doc, la, levents, err := sampleLeaseAgreement(ctx, files, l, SampleLeaseTemplates(now)[0])
	if err != nil {
		return err
	}
	l.Documents[leases.DocumentSigning] = doc
	l.AgreementID = &la.ID
	leaseStore.PutLease(l, seedLeaseEvent(l, leases.EventCreated, l.CreatedAt, nil), seedLeaseEvent(l, leases.EventSent, *l.SentAt, nil))
	store.PutAgreement(la, levents...)
	return nil
}

// SeedPostgresAgreements creates the same samples in Postgres (needs
// migration 0023), after SeedPostgres committed. A sample that is already
// there is left alone.
func SeedPostgresAgreements(ctx context.Context, pool *pgxpool.Pool, files storage.Storage, now time.Time) error {
	now = now.UTC().Truncate(time.Microsecond)
	store := agreements.NewPostgresStore(pool)
	if _, err := store.Agreement(ctx, SampleUploadAgreementID); errors.Is(err, agreements.ErrNotFound) {
		a, events, err := sampleUploadAgreement(ctx, files, now)
		if err != nil {
			return err
		}
		if err := store.CreateAgreement(ctx, a, events); err != nil {
			return fmt.Errorf("seed sample agreement: %w", err)
		}
	} else if err != nil {
		return err
	}
	if _, err := store.Agreement(ctx, SampleLeaseAgreementID); !errors.Is(err, agreements.ErrNotFound) {
		return err
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM leases WHERE id = $1 OR reference = $2)`, SampleSentLeaseID, SampleSentLeaseReference).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	l := sampleSentLease(now)
	doc, a, events, err := sampleLeaseAgreement(ctx, files, l, SampleLeaseTemplates(now)[0])
	if err != nil {
		return err
	}
	input, _ := json.Marshal(l.Input)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO leases (id, reference, status, template_id, template_version, template_name, market_code, lease_kind, organisation_id,
			organisation_name, listing_id, listing_reference, listing_title, created_by, created_by_name, input_snapshot, sent_at, sent_by,
			created_at, updated_at)
		VALUES ($1, $2, 'SENT_FOR_SIGNING', $3, 1, $4, 'ZW', 'RESIDENTIAL', $5, $6, $7, $8, $9, $10, $11, $12, $13, $10, $14, $13)`,
		l.ID, l.Reference, l.TemplateID, l.TemplateName, l.OrganisationID, l.OrganisationName, l.ListingID, l.ListingReference, l.ListingTitle,
		l.CreatedBy, l.CreatedByName, string(input), *l.SentAt, l.CreatedAt); err != nil {
		return fmt.Errorf("seed sent lease: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO lease_documents (id, lease_id, kind, template_id, template_version, input_snapshot, object_key, sha256, size_bytes, created_at)
		VALUES ($1, $2, 'SIGNING', $3, 1, $4, $5, $6, $7, $8)`,
		uuid.New(), l.ID, l.TemplateID, string(doc.Snapshot), doc.ObjectKey, doc.SHA256, doc.Size, *l.SentAt); err != nil {
		return fmt.Errorf("seed sent lease document: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if err := store.CreateAgreement(ctx, a, events); err != nil {
		return fmt.Errorf("seed lease agreement: %w", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE leases SET agreement_id = $2 WHERE id = $1`, l.ID, a.ID); err != nil {
		return fmt.Errorf("link sample lease to its agreement: %w", err)
	}
	return nil
}
