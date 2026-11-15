package devseed

import (
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/auth"
)

// This file completes the sample accounts so that every account role and
// every platform administrator role has one verified, working account
// (docs/sample-accounts.md lists them all). The roles the earlier samples
// already cover keep their accounts and emails:
//
//	CONSUMER          consumer@example.test
//	AGENT             agent@example.test (and others)
//	AGENCY_OWNER      estates.owner@example.test (and others)
//	PRIVATE_SELLER    seller@example.test
//	PROPERTY_MANAGER  sample-property-management@example.test
//	SOLICITOR         sample-law-chambers@example.test
//	HOST              sample-falls-stays@example.test
//	DEVELOPER         sample-developments@example.test
//	SERVICE_PROVIDER  sample-plumbers@example.test
//	SUPER_ADMIN       admin@example.test
//
// The accounts below follow the seeker-only rule: the tenant holds TENANT
// only; every professional holds its professional role only.

// Emails of the accounts added for the remaining roles.
const (
	TenantEmail        = "tenant@example.test"
	SalesAgentEmail    = "sales.agent@example.test"
	LettingsAgentEmail = "lettings.agent@example.test"
	AgencyAdminEmail   = "agency.admin@example.test"
	AgencyStaffEmail   = "agency.staff@example.test"
	LandlordEmail      = "landlord@example.test"

	PlatformAdminEmail = "admin.platform@example.test"
	MarketAdminEmail   = "admin.market@example.test"
	ModeratorEmail     = "admin.moderator@example.test"
	SupportEmail       = "admin.support@example.test"
	FinanceAdminEmail  = "admin.finance@example.test"
	ContentAdminEmail  = "admin.content@example.test"
)

// Fixed user IDs of the accounts above (the d3xx range is unused elsewhere).
var (
	TenantID        = id("d301")
	SalesAgentID    = id("d302")
	LettingsAgentID = id("d303")
	AgencyAdminID   = id("d304")
	AgencyStaffID   = id("d305")
	LandlordID      = id("d306")

	PlatformAdminID = id("d311")
	MarketAdminID   = id("d312")
	ModeratorID     = id("d313")
	SupportID       = id("d314")
	FinanceAdminID  = id("d315")
	ContentAdminID  = id("d316")
)

// roleAccounts are the accounts added for the roles no earlier sample had.
// The agency people join Sample Estates, which has listings, requests and
// appointments, so their dashboards show something.
func roleAccounts() []Account {
	return []Account{
		{ID: TenantID, Name: "Tariro Tenant", Email: TenantEmail, AccountRoles: []string{identity.AccountRoleTenant}},
		{ID: SalesAgentID, Name: "Simba Sales", Email: SalesAgentEmail, Organisation: SampleEstates, MembershipRole: auth.RoleAgent,
			AccountRoles: []string{identity.AccountRoleSalesAgent}},
		{ID: LettingsAgentID, Name: "Lindiwe Lettings", Email: LettingsAgentEmail, Organisation: SampleEstates, MembershipRole: auth.RoleAgent,
			AccountRoles: []string{identity.AccountRoleLettingsAgent}},
		{ID: AgencyAdminID, Name: "Ruvimbo Chari", Email: AgencyAdminEmail, Organisation: SampleEstates, MembershipRole: auth.RoleManager,
			AccountRoles: []string{identity.AccountRoleAgencyAdmin}},
		{ID: AgencyStaffID, Name: "Kuda Staff", Email: AgencyStaffEmail, Organisation: SampleEstates, MembershipRole: auth.RoleStaff,
			AccountRoles: []string{identity.AccountRoleAgencyStaff}},
		{ID: LandlordID, Name: "Lloyd Landlord", Email: LandlordEmail, AccountRoles: []string{identity.AccountRoleLandlord}},

		{ID: PlatformAdminID, Name: "Pat Platform", Email: PlatformAdminEmail, PlatformRole: auth.PlatformAdmin},
		{ID: MarketAdminID, Name: "Mika Market", Email: MarketAdminEmail, PlatformRole: auth.PlatformMarketAdmin, PlatformMarket: MarketCode},
		{ID: ModeratorID, Name: "Morgan Moderator", Email: ModeratorEmail, PlatformRole: auth.PlatformModerator},
		{ID: SupportID, Name: "Sam Support", Email: SupportEmail, PlatformRole: auth.PlatformSupport},
		{ID: FinanceAdminID, Name: "Fiona Finance", Email: FinanceAdminEmail, PlatformRole: auth.PlatformFinanceAdmin},
		{ID: ContentAdminID, Name: "Connor Content", Email: ContentAdminEmail, PlatformRole: auth.PlatformContentAdmin},
	}
}
