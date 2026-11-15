package devseed

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/storage"
)

// The sample agency's team screens (Sample Estates, managed by Rudo Moyo
// and Ruvimbo Chari) have something to show: one pending invitation and one
// pending join request.
//
//   - InvitedAgentEmail was invited as AGENT by Rudo Moyo. Nobody knows the
//     link: only the SHA-256 of a random token is stored, as for every
//     invitation. To try accepting, invite an address again (the new link is
//     printed by the development mail log) and sign up with that address.
//   - JoiningAgentEmail is an account (password DEV_SEED_PASSWORD) whose
//     AGENT application to join Sample Estates is SUBMITTED, with its
//     required sample documents: the agency's owner or manager, or a
//     platform reviewer, can approve or decline it.
const (
	InvitedAgentEmail = "invited.agent@example.test"
	JoiningAgentEmail = "joining.agent@example.test"
	joiningAgentName  = "Jamie Joiner"
	sampleAgencySlug  = "sample-estates"
)

// Fixed IDs of the team samples.
var (
	JoiningAgentID            = id("d203")
	JoiningAgentApplicationID = id("f003")
	SampleInvitationID        = id("f201")
)

// joiningAgentDetails are the details of the sample join request. The phone
// number is from a range that is not assigned to anyone.
func joiningAgentDetails() onboarding.Details {
	return onboarding.Details{
		AgencySlug: text(sampleAgencySlug), JobTitle: text("Negotiator"), Areas: []string{"Borrowdale", "Mount Pleasant"},
		Bio: text("A sample agent used to try the join request flow."), ContactPhone: text("+263770000002"),
	}
}

// joiningAgentDocuments returns one sample document per document type an
// AGENT must attach, under fixed storage keys.
func joiningAgentDocuments(now time.Time) []sampleDocument {
	var out []sampleDocument
	n := 0
	for _, r := range onboarding.DefaultRequirements() {
		if r.Role != identity.AccountRoleAgent || !r.Required {
			continue
		}
		n++
		content, contentType, name := samplePDF(r.Document), "application/pdf", "sample-"+onboarding.Slugify(r.Document)+".pdf"
		sum := sha256.Sum256(content)
		out = append(out, sampleDocument{content: content, Document: onboarding.Document{
			ID: id(fmt.Sprintf("f3%02d", n)), ApplicationID: JoiningAgentApplicationID, Type: r.Document, FileName: name,
			ContentType: contentType, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]),
			ObjectKey:  fmt.Sprintf("%s/devseedjoin%02d", onboarding.DocumentKeyPrefix, n),
			UploadedAt: now.Add(-time.Duration(n) * time.Minute),
		}})
	}
	return out
}

// unknownTokenHash is the hash of a random token nobody keeps.
func unknownTokenHash() ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

func sampleInvitation(now time.Time, hash []byte) teams.Invitation {
	inviter := EstatesOwnerID
	created := now.Add(-24 * time.Hour)
	return teams.Invitation{
		ID: SampleInvitationID, OrganisationID: SampleEstates.ID, Email: InvitedAgentEmail, Role: auth.RoleAgent,
		JobTitle: "Lettings negotiator", Status: teams.InvitationPending, TokenHash: hash,
		InvitedBy: &inviter, InvitedByName: "Rudo Moyo", CreatedAt: created, ExpiresAt: created.Add(teams.InvitationTTL),
	}
}

// SeedMemoryTeams creates the sample invitation and join request in the
// in-memory stores. No event is emitted, so nothing is emailed.
func SeedMemoryTeams(ctx context.Context, users *identity.MemoryStore, apps *onboarding.MemoryStore, team *teams.MemoryStore,
	files storage.Storage, hasher identity.Hasher, password string, now time.Time) error {
	hash, err := hasher.Hash(password)
	if err != nil {
		return err
	}
	now = now.UTC().Truncate(time.Microsecond)
	verified := now
	// Registration creates the DRAFT application for the pending role.
	if err := users.CreateUser(ctx, identity.NewUser{
		ID: JoiningAgentID, Name: joiningAgentName, Email: JoiningAgentEmail, PasswordHash: hash, PrivacyVersion: privacyVersion,
		AccountRoles: pendingRoles(identity.AccountRoleAgent), EmailVerifiedAt: &verified, CreatedAt: now,
	}); err != nil {
		return fmt.Errorf("seed %s: %w", JoiningAgentEmail, err)
	}
	current, err := apps.Current(ctx, JoiningAgentID)
	if err != nil || len(current) != 1 {
		return errors.Join(errors.New("seed teams: the joining agent has no draft application"), err)
	}
	docs := joiningAgentDocuments(now)
	if err := storeSampleDocuments(ctx, files, docs); err != nil {
		return err
	}
	submitted := now.Add(-3 * time.Hour)
	app := current[0]
	apps.RemoveApplication(app.ID)
	app.ID = JoiningAgentApplicationID
	app.Status, app.Details, app.MarketCode, app.AgencyName = onboarding.StatusSubmitted, joiningAgentDetails(), MarketCode, SampleEstates.Name
	app.SubmittedAt, app.UpdatedAt = &submitted, submitted
	for _, d := range docs {
		doc := d.Document
		doc.ApplicationID = app.ID
		app.Documents = append(app.Documents, doc)
	}
	apps.PutApplication(app, onboarding.HistoryEntry{From: onboarding.StatusDraft, To: onboarding.StatusSubmitted, ActorID: JoiningAgentID, ActorName: joiningAgentName, At: submitted})

	tokenHash, err := unknownTokenHash()
	if err != nil {
		return err
	}
	team.PutInvitation(sampleInvitation(now, tokenHash))
	return nil
}

// seedPostgresTeams creates the same samples in Postgres (needs migration
// 0021). Like the onboarding demo, a sample that is already there is left
// alone: if it was decided or accepted in this database, that stands.
//
// NOTE: compiled but never run, like the rest of SeedPostgres.
func seedPostgresTeams(ctx context.Context, tx pgx.Tx, files storage.Storage, hash string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO users (id, email, full_name, status, password_hash, email_verified_at, created_at, updated_at)
		VALUES ($1, $2, $3, 'ACTIVE', $4, $5, $5, $5)
		ON CONFLICT (id) DO UPDATE
		SET email = EXCLUDED.email, full_name = EXCLUDED.full_name, status = 'ACTIVE', password_hash = EXCLUDED.password_hash,
		    email_verified_at = COALESCE(users.email_verified_at, EXCLUDED.email_verified_at), updated_at = EXCLUDED.updated_at`,
		JoiningAgentID, JoiningAgentEmail, joiningAgentName, hash, now); err != nil {
		return fmt.Errorf("seed user %s: %w", JoiningAgentEmail, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_consents (id, user_id, consent_type, version, accepted_at, created_at)
		SELECT $1, $2, $3, $4, $5, $5
		WHERE NOT EXISTS (SELECT 1 FROM user_consents WHERE user_id = $2 AND consent_type = $3)`,
		uuid.New(), JoiningAgentID, identity.ConsentPrivacyNotice, privacyVersion, now); err != nil {
		return fmt.Errorf("seed consent for %s: %w", JoiningAgentEmail, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_account_roles (id, user_id, role, status, created_at, updated_at)
		VALUES ($1, $2, $3, 'PENDING_VERIFICATION', $4, $4) ON CONFLICT (user_id, role) DO NOTHING`,
		uuid.New(), JoiningAgentID, identity.AccountRoleAgent, now); err != nil {
		return fmt.Errorf("seed account role for %s: %w", JoiningAgentEmail, err)
	}

	submitted := now.Add(-3 * time.Hour)
	d := joiningAgentDetails()
	tag, err := tx.Exec(ctx, `
		INSERT INTO onboarding_applications (id, user_id, role, status, agency_slug, agency_name, job_title, bio, areas, contact_phone,
			market_code, submitted_at, created_at, updated_at)
		SELECT $1, $2, $3, 'SUBMITTED', $4, $5, $6, $7, $8, $9, $10, $11, $12, $11
		WHERE NOT EXISTS (SELECT 1 FROM onboarding_applications WHERE user_id = $2 AND role = $3)`,
		JoiningAgentApplicationID, JoiningAgentID, identity.AccountRoleAgent, d.AgencySlug, SampleEstates.Name, d.JobTitle, d.Bio,
		d.Areas, d.ContactPhone, MarketCode, submitted, now)
	if err != nil {
		return fmt.Errorf("seed join request: %w", err)
	}
	if tag.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO onboarding_history (id, application_id, from_status, to_status, actor_user_id, created_at)
			VALUES ($1, $2, 'DRAFT', 'SUBMITTED', $3, $4)`, uuid.New(), JoiningAgentApplicationID, JoiningAgentID, submitted); err != nil {
			return fmt.Errorf("seed join request history: %w", err)
		}
		docs := joiningAgentDocuments(now)
		for _, doc := range docs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO onboarding_documents (id, application_id, document_code, file_name, content_type, size_bytes, sha256, object_key, uploaded_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (id) DO NOTHING`,
				doc.ID, doc.ApplicationID, doc.Type, doc.FileName, doc.ContentType, doc.Size, doc.SHA256, doc.ObjectKey, doc.UploadedAt); err != nil {
				return fmt.Errorf("seed join request document %s: %w", doc.Type, err)
			}
		}
		if err := storeSampleDocuments(ctx, files, docs); err != nil {
			return err
		}
	}

	tokenHash, err := unknownTokenHash()
	if err != nil {
		return err
	}
	inv := sampleInvitation(now, tokenHash)
	if _, err := tx.Exec(ctx, `
		INSERT INTO organisation_invitations (id, organisation_id, email, role, job_title, token_hash, status, invited_by, invited_by_name,
			expires_at, created_at, updated_at)
		SELECT $1, $2, $3, $4, $5, $6, 'PENDING', $7, $8, $9, $10, $10
		WHERE NOT EXISTS (SELECT 1 FROM organisation_invitations WHERE id = $1)
		  AND NOT EXISTS (SELECT 1 FROM organisation_invitations WHERE organisation_id = $2 AND email = $3 AND status = 'PENDING')`,
		inv.ID, inv.OrganisationID, inv.Email, inv.Role, inv.JobTitle, inv.TokenHash, inv.InvitedBy, inv.InvitedByName,
		inv.ExpiresAt, inv.CreatedAt); err != nil {
		return fmt.Errorf("seed invitation: %w", err)
	}
	return nil
}
