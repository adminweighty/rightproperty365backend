package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

// Development seed accounts. They exist only in the in-memory store and only
// when APP_ENV=development and DEV_SEED_PASSWORD is set.
const (
	SeedConsumerEmail = "consumer@example.test"
	SeedAgentEmail    = "agent@example.test"
	SeedAdminEmail    = "admin@example.test"
	SeedAgentBEmail   = "agent-b@example.test"

	SeedSampleOrganisationName = "Sample Estates"
	SeedSecondOrganisationName = "Second Street Realty"

	seedMarketCode     = "ZW"
	seedPrivacyVersion = "dev-seed"
)

// Fixed IDs so development data is stable across restarts.
var (
	SeedConsumerUserID       = uuid.MustParse("0195c0de-0000-7000-8000-00000000d001")
	SeedAgentUserID          = uuid.MustParse("0195c0de-0000-7000-8000-00000000d002")
	SeedAdminUserID          = uuid.MustParse("0195c0de-0000-7000-8000-00000000d003")
	SeedAgentBUserID         = uuid.MustParse("0195c0de-0000-7000-8000-00000000d004")
	SeedSecondOrganisationID = uuid.MustParse("0195c0de-0000-7000-8000-00000000b002")
)

// SeedDevelopment adds the development organisations and four VERIFIED users
// sharing one password:
//
//   - consumer@example.test — no memberships; account role CONSUMER
//   - agent@example.test    — AGENT at "Sample Estates" (AGENCY, market ZW),
//     the organisation sampleOrganisationID that receives requests for the
//     sample listings; account roles CONSUMER + AGENT
//   - admin@example.test    — SUPER_ADMIN; account role CONSUMER
//   - agent-b@example.test  — AGENT at a second organisation that owns no
//     listings (for scoping checks); account role AGENT
//
// Seeded account roles are all ACTIVE.
//
// The password is never logged.
func (s *MemoryStore) SeedDevelopment(ctx context.Context, hasher Hasher, password string, sampleOrganisationID uuid.UUID, now time.Time) error {
	if msg := passwordProblem(password); msg != "" {
		return fmt.Errorf("DEV_SEED_PASSWORD %s", msg)
	}
	// One hash for all four accounts keeps startup fast.
	hash, err := hasher.Hash(password)
	if err != nil {
		return err
	}
	now = now.UTC().Truncate(time.Microsecond)

	s.PutOrganisation(MemoryOrganisation{ID: sampleOrganisationID, Name: SeedSampleOrganisationName, Kind: auth.KindAgency, MarketCode: seedMarketCode})
	s.PutOrganisation(MemoryOrganisation{ID: SeedSecondOrganisationID, Name: SeedSecondOrganisationName, Kind: auth.KindAgency, MarketCode: seedMarketCode})

	for _, u := range []struct {
		id          uuid.UUID
		name, email string
		roles       []string
	}{
		{SeedConsumerUserID, "Casey Consumer", SeedConsumerEmail, []string{AccountRoleConsumer}},
		{SeedAgentUserID, "Alex Agent", SeedAgentEmail, []string{AccountRoleConsumer, AccountRoleAgent}},
		{SeedAdminUserID, "Sam Admin", SeedAdminEmail, []string{AccountRoleConsumer}},
		{SeedAgentBUserID, "Blake Agent", SeedAgentBEmail, []string{AccountRoleAgent}},
	} {
		verified := now
		accountRoles := make([]AccountRole, 0, len(u.roles))
		for _, role := range u.roles {
			accountRoles = append(accountRoles, AccountRole{Role: role, Status: AccountRoleActive})
		}
		if err := s.CreateUser(ctx, NewUser{
			ID: u.id, Name: u.name, Email: u.email, PasswordHash: hash,
			PrivacyVersion: seedPrivacyVersion, AccountRoles: accountRoles, EmailVerifiedAt: &verified, CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("seed %s: %w", u.email, err)
		}
	}

	if err := s.AddMembership(SeedAgentUserID, sampleOrganisationID, seedMarketCode, auth.RoleAgent); err != nil {
		return err
	}
	if err := s.AddMembership(SeedAgentBUserID, SeedSecondOrganisationID, seedMarketCode, auth.RoleAgent); err != nil {
		return err
	}
	return s.AddPlatformRole(SeedAdminUserID, auth.PlatformSuperAdmin, "")
}
