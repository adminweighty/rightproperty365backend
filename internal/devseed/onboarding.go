package devseed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Demo accounts for trying the onboarding flow. Unlike the other sample
// accounts their professional role is NOT active: it waits for an
// administrator.
//
// The seeded professionals (agents, agency owners, business owners) are
// already approved and are deliberately left WITHOUT onboarding applications:
// their roles were granted by the seed, not by a review.
const (
	// PendingProviderEmail has a SERVICE_PROVIDER application that is
	// SUBMITTED, with details and one sample document per required type.
	PendingProviderEmail = "pending.provider@example.test"
	// PendingAgentEmail has an AGENT application that is still a DRAFT.
	PendingAgentEmail = "pending.agent@example.test"
)

// Fixed IDs of the demo accounts and of the submitted application.
var (
	PendingProviderID            = id("d201")
	PendingAgentID               = id("d202")
	PendingProviderApplicationID = id("f001")
	pendingAgentApplicationID    = id("f002")
)

// OnboardingAccount is one onboarding demo account.
type OnboardingAccount struct {
	ID     uuid.UUID
	Name   string
	Email  string
	Role   string
	Status string
}

// OnboardingAccounts returns the two demo accounts.
func OnboardingAccounts() []OnboardingAccount {
	return []OnboardingAccount{
		{PendingProviderID, "Pamela Provider", PendingProviderEmail, identity.AccountRoleServiceProvider, onboarding.StatusSubmitted},
		{PendingAgentID, "Peter Pending", PendingAgentEmail, identity.AccountRoleAgent, onboarding.StatusDraft},
	}
}

func text(s string) *string { return &s }

// pendingProviderDetails are the details of the submitted demo application.
// The phone number is from a range that is not assigned to anyone.
func pendingProviderDetails() onboarding.Details {
	return onboarding.Details{
		BusinessName: text("Sample Roofing Repairs"), Tagline: text("Roof repairs and waterproofing in Harare"),
		Description: text("A sample business used to try the verification flow. Leak tracing, tile and sheet replacement, and waterproofing."),
		City:        text("harare"), Areas: []string{"Borrowdale", "Avondale"}, Services: []string{"Leak repairs", "Waterproofing"},
		Trade: text(Code(Sample().firstTrade())), RegistrationNumber: text("SAMPLE-0001"), ContactPhone: text("+263770000001"),
	}
}

// firstTrade returns the first service trade used by the sample profiles, so
// the demo application always names a trade that exists as reference data.
func (d SampleData) firstTrade() string {
	for _, p := range d.Profiles {
		if p.Category != "" {
			return p.Category
		}
	}
	return ""
}

// samplePDF is a tiny file that starts like a PDF. It is a placeholder, not
// a real document.
func samplePDF(title string) []byte {
	return []byte("%PDF-1.4\n% RightProperty365 development sample: " + title + "\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")
}

// samplePNG is a tiny generated image.
func samplePNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{R: 0x1f, G: 0x4f, B: 0xd8, A: 0xff})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

type sampleDocument struct {
	onboarding.Document
	content []byte
}

// pendingProviderDocuments returns one sample document per document type a
// SERVICE_PROVIDER must attach. Their storage keys are fixed, so seeding
// again overwrites the same files instead of piling up new ones.
func pendingProviderDocuments(now time.Time) []sampleDocument {
	var out []sampleDocument
	n := 0
	for _, r := range onboarding.DefaultRequirements() {
		if r.Role != identity.AccountRoleServiceProvider || !r.Required {
			continue
		}
		n++
		content, contentType, name := samplePDF(r.Document), "application/pdf", "sample-"+onboarding.Slugify(r.Document)+".pdf"
		if r.Document == "PROOF_OF_IDENTITY" {
			content, contentType, name = samplePNG(), "image/png", "sample-proof-of-identity.png"
		}
		sum := sha256.Sum256(content)
		out = append(out, sampleDocument{content: content, Document: onboarding.Document{
			ID: id(fmt.Sprintf("f1%02d", n)), ApplicationID: PendingProviderApplicationID, Type: r.Document, FileName: name,
			ContentType: contentType, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]),
			ObjectKey:  fmt.Sprintf("%s/devseed%02d", onboarding.DocumentKeyPrefix, n),
			UploadedAt: now.Add(-time.Duration(n) * time.Minute),
		}})
	}
	return out
}

func storeSampleDocuments(ctx context.Context, files storage.Storage, docs []sampleDocument) error {
	for _, d := range docs {
		if err := files.Put(ctx, d.ObjectKey, bytes.NewReader(d.content), d.Size, d.ContentType); err != nil {
			return fmt.Errorf("seed onboarding document %s: %w", d.Type, err)
		}
	}
	return nil
}

func pendingRoles(role string) []identity.AccountRole {
	return []identity.AccountRole{
		{Role: identity.AccountRoleConsumer, Status: identity.AccountRoleActive},
		{Role: role, Status: identity.AccountRolePendingVerification},
	}
}

// SeedMemoryOnboarding creates the two onboarding demo accounts in the
// in-memory stores, with the sample documents written to files. No event is
// emitted, so no email is sent for the seeded application.
func SeedMemoryOnboarding(ctx context.Context, users *identity.MemoryStore, store *onboarding.MemoryStore, files storage.Storage,
	hasher identity.Hasher, password string, now time.Time) error {
	hash, err := hasher.Hash(password)
	if err != nil {
		return err
	}
	now = now.UTC().Truncate(time.Microsecond)
	for _, a := range OnboardingAccounts() {
		verified := now
		// Registration creates the DRAFT application for the pending role.
		if err := users.CreateUser(ctx, identity.NewUser{
			ID: a.ID, Name: a.Name, Email: a.Email, PasswordHash: hash, PrivacyVersion: privacyVersion,
			AccountRoles: pendingRoles(a.Role), EmailVerifiedAt: &verified, CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("seed %s: %w", a.Email, err)
		}
	}
	apps, err := store.Current(ctx, PendingProviderID)
	if err != nil || len(apps) != 1 {
		return errors.Join(errors.New("seed onboarding: the demo provider has no draft application"), err)
	}
	docs := pendingProviderDocuments(now)
	if err := storeSampleDocuments(ctx, files, docs); err != nil {
		return err
	}
	submitted := now.Add(-2 * time.Hour)
	// The demo application has a fixed ID, so links to it survive a restart.
	app := apps[0]
	store.RemoveApplication(app.ID)
	app.ID = PendingProviderApplicationID
	app.Status, app.Details, app.MarketCode, app.SubmittedAt, app.UpdatedAt = onboarding.StatusSubmitted, pendingProviderDetails(), MarketCode, &submitted, submitted
	for _, d := range docs {
		doc := d.Document
		doc.ApplicationID = app.ID
		app.Documents = append(app.Documents, doc)
	}
	store.PutApplication(app, onboarding.HistoryEntry{From: onboarding.StatusDraft, To: onboarding.StatusSubmitted, ActorID: PendingProviderID, ActorName: "Pamela Provider", At: submitted})
	return nil
}

// seedPostgresOnboarding creates the same two demo accounts in Postgres.
//
// NOTE: compiled but never run, like the rest of SeedPostgres.
func seedPostgresOnboarding(ctx context.Context, tx pgx.Tx, files storage.Storage, hash string, now time.Time) error {
	submitted := now.Add(-2 * time.Hour)
	for _, a := range OnboardingAccounts() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (id, email, full_name, status, password_hash, email_verified_at, created_at, updated_at)
			VALUES ($1, $2, $3, 'ACTIVE', $4, $5, $5, $5)
			ON CONFLICT (id) DO UPDATE
			SET email = EXCLUDED.email, full_name = EXCLUDED.full_name, status = 'ACTIVE', password_hash = EXCLUDED.password_hash,
			    email_verified_at = COALESCE(users.email_verified_at, EXCLUDED.email_verified_at), updated_at = EXCLUDED.updated_at`,
			a.ID, a.Email, a.Name, hash, now); err != nil {
			return fmt.Errorf("seed user %s: %w", a.Email, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_consents (id, user_id, consent_type, version, accepted_at, created_at)
			SELECT $1, $2, $3, $4, $5, $5
			WHERE NOT EXISTS (SELECT 1 FROM user_consents WHERE user_id = $2 AND consent_type = $3)`,
			uuid.New(), a.ID, identity.ConsentPrivacyNotice, privacyVersion, now); err != nil {
			return fmt.Errorf("seed consent for %s: %w", a.Email, err)
		}
		// The account roles are only created, never reset: if the demo
		// application was decided in this database, that decision stands.
		for _, r := range pendingRoles(a.Role) {
			if _, err := tx.Exec(ctx, `
				INSERT INTO user_account_roles (id, user_id, role, status, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $5) ON CONFLICT (user_id, role) DO NOTHING`,
				uuid.New(), a.ID, r.Role, r.Status, now); err != nil {
				return fmt.Errorf("seed account role %s for %s: %w", r.Role, a.Email, err)
			}
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO onboarding_applications (id, user_id, role, status, created_at, updated_at)
		SELECT $1, $2, $3, 'DRAFT', $4, $4
		WHERE NOT EXISTS (SELECT 1 FROM onboarding_applications WHERE user_id = $2 AND role = $3)`,
		pendingAgentApplicationID, PendingAgentID, identity.AccountRoleAgent, now); err != nil {
		return fmt.Errorf("seed agent application: %w", err)
	}

	d := pendingProviderDetails()
	tag, err := tx.Exec(ctx, `
		INSERT INTO onboarding_applications (id, user_id, role, status, business_name, tagline, description, city_code, areas, services,
			trade_code, registration_number, contact_phone, market_code, submitted_at, created_at, updated_at)
		SELECT $1, $2, $3, 'SUBMITTED', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $14
		WHERE NOT EXISTS (SELECT 1 FROM onboarding_applications WHERE user_id = $2 AND role = $3)`,
		PendingProviderApplicationID, PendingProviderID, identity.AccountRoleServiceProvider, d.BusinessName, d.Tagline, d.Description,
		d.City, d.Areas, d.Services, d.Trade, d.RegistrationNumber, d.ContactPhone, MarketCode, submitted, now)
	if err != nil {
		return fmt.Errorf("seed provider application: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // already there (and perhaps already decided): leave it alone
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO onboarding_history (id, application_id, from_status, to_status, actor_user_id, created_at)
		VALUES ($1, $2, 'DRAFT', 'SUBMITTED', $3, $4)`, uuid.New(), PendingProviderApplicationID, PendingProviderID, submitted); err != nil {
		return fmt.Errorf("seed provider history: %w", err)
	}
	docs := pendingProviderDocuments(now)
	for _, doc := range docs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO onboarding_documents (id, application_id, document_code, file_name, content_type, size_bytes, sha256, object_key, uploaded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (id) DO NOTHING`,
			doc.ID, doc.ApplicationID, doc.Type, doc.FileName, doc.ContentType, doc.Size, doc.SHA256, doc.ObjectKey, doc.UploadedAt); err != nil {
			return fmt.Errorf("seed provider document %s: %w", doc.Type, err)
		}
	}
	return storeSampleDocuments(ctx, files, docs)
}
