package identity

import "github.com/google/uuid"

// Identifiers of the original development seed accounts. The full sample data
// set (organisations, people, listings) lives in internal/devseed; these stay
// here because tests across the module refer to them.
const (
	SeedConsumerEmail = "consumer@example.test"
	SeedAgentEmail    = "agent@example.test"
	SeedAdminEmail    = "admin@example.test"
	SeedAgentBEmail   = "agent-b@example.test"

	SeedSecondOrganisationName = "Second Street Realty"
)

// Fixed IDs so development data is stable across restarts.
var (
	SeedConsumerUserID       = uuid.MustParse("0195c0de-0000-7000-8000-00000000d001")
	SeedAgentUserID          = uuid.MustParse("0195c0de-0000-7000-8000-00000000d002")
	SeedAdminUserID          = uuid.MustParse("0195c0de-0000-7000-8000-00000000d003")
	SeedAgentBUserID         = uuid.MustParse("0195c0de-0000-7000-8000-00000000d004")
	SeedSecondOrganisationID = uuid.MustParse("0195c0de-0000-7000-8000-00000000b002")
)
