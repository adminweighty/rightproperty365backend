package dashboard

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// GET /v1/admin/overview: the counts a platform administrator starts from.
//
// Every platform role may call it, and each gets only the sections its role
// covers; a section the caller may not see is null. The rules:
//
//   - accounts (they carry names and email addresses, and have no market):
//     SUPER_ADMIN, PLATFORM_ADMIN and SUPPORT;
//   - verifications: exactly who may open the verification queue
//     (onboarding.AdminRoles), a MARKET_ADMIN for its own markets;
//   - listings: SUPER_ADMIN, PLATFORM_ADMIN, MODERATOR, and a MARKET_ADMIN
//     for its own markets;
//   - requests and routing exceptions: exactly who may open the exception
//     queue (propertyrequests.ExceptionAdminRoles), a MARKET_ADMIN for its
//     own markets.
//
// FINANCE_ADMIN and CONTENT_ADMIN get no section: nothing they are
// responsible for is built yet.

// Overview sections.
const (
	SectionAccounts      = "accounts"
	SectionVerifications = "verifications"
	SectionListings      = "listings"
	SectionRequests      = "requests"
)

// recentAccounts is how many new accounts the overview lists.
const recentAccounts = 10

type statusCount struct {
	Status string `json:"status"`
	Label  string `json:"label"`
	Count  int    `json:"count"`
}

type roleCount struct {
	Role   string `json:"role"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Count  int    `json:"count"`
}

type recentAccount struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	EmailVerified bool      `json:"email_verified"`
	Roles         []string  `json:"roles"`
	CreatedAt     time.Time `json:"created_at"`
}

type accountsSection struct {
	Total          int             `json:"total"`
	Administrators int             `json:"administrators"`
	Unverified     int             `json:"unverified"`
	Last7Days      int             `json:"last_7_days"`
	Last30Days     int             `json:"last_30_days"`
	ByStatus       []statusCount   `json:"by_status"`
	ByRole         []roleCount     `json:"by_role"`
	Recent         []recentAccount `json:"recent"`
}

type verificationsSection struct {
	// Waiting is the number of applications submitted and not yet decided.
	Waiting  int           `json:"waiting"`
	ByStatus []statusCount `json:"by_status"`
}

type listingsSection struct {
	// MarketCodes is null for every country, else the countries counted.
	MarketCodes []string      `json:"market_codes"`
	Total       int           `json:"total"`
	ByStatus    []statusCount `json:"by_status"`
}

type exceptionCounts struct {
	Open     int `json:"open"`
	Resolved int `json:"resolved"`
}

type requestsSection struct {
	MarketCodes []string                      `json:"market_codes"`
	Total       int                           `json:"total"`
	Last7Days   int                           `json:"last_7_days"`
	Last30Days  int                           `json:"last_30_days"`
	ByStage     []propertyrequests.StageCount `json:"by_stage"`
	// RoutingExceptions are requests nobody could receive. They are not in by_stage.
	RoutingExceptions exceptionCounts `json:"routing_exceptions"`
}

type platformRoleView struct {
	Role       string  `json:"role"`
	MarketCode *string `json:"market_code"`
}

// Overview is the body of GET /v1/admin/overview.
type Overview struct {
	Roles []platformRoleView `json:"roles"`
	// Sections lists the sections that are not null, in display order.
	Sections      []string              `json:"sections"`
	Accounts      *accountsSection      `json:"accounts"`
	Verifications *verificationsSection `json:"verifications"`
	Listings      *listingsSection      `json:"listings"`
	Requests      *requestsSection      `json:"requests"`
}

// listingMarkets returns the countries whose listings an administrator may
// count: all for the global roles, its own for a MARKET_ADMIN.
func listingMarkets(p *auth.Principal) (codes []string, all, ok bool) {
	for _, r := range p.PlatformRoles {
		switch r.Role {
		case auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformModerator:
			return nil, true, true
		case auth.PlatformMarketAdmin:
			if r.MarketCode != "" && !slices.Contains(codes, r.MarketCode) {
				codes = append(codes, r.MarketCode)
			}
		}
	}
	return codes, false, len(codes) > 0
}

func holds(p *auth.Principal, roles ...string) bool {
	for _, r := range p.PlatformRoles {
		if slices.Contains(roles, r.Role) {
			return true
		}
	}
	return false
}

func accountStatusLabel(status string) string {
	switch status {
	case "ACTIVE":
		return "Active"
	case "SUSPENDED":
		return "Suspended"
	case "DELETED":
		return "Deleted"
	}
	return status
}

func listingStatusName(status string) string {
	label := listingStatusLabel(status)
	return label[:len(label)-len(" listings")]
}

// Overview serves GET /v1/admin/overview. The route requires an ADMIN-scope
// session holding a platform role.
func (h *Handler) Overview(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	if p.SessionScope != auth.ScopeAdmin || len(p.PlatformRoles) == 0 {
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
		return
	}
	ctx, now := r.Context(), h.src.Now().UTC()
	out := Overview{Roles: []platformRoleView{}, Sections: []string{}}
	for _, role := range p.PlatformRoles {
		v := platformRoleView{Role: role.Role}
		if role.MarketCode != "" {
			code := role.MarketCode
			v.MarketCode = &code
		}
		out.Roles = append(out.Roles, v)
	}
	fail := func(err error) { httpx.WriteInternalError(w, r, h.logger, err) }

	if holds(p, auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformSupport) {
		stats, err := h.src.People.AccountStats(ctx, now, recentAccounts)
		if err != nil {
			fail(err)
			return
		}
		labels := map[string]string{}
		if h.src.RoleLabels != nil {
			if labels, err = h.src.RoleLabels(ctx); err != nil {
				fail(err)
				return
			}
		}
		s := &accountsSection{
			Total: stats.Total, Administrators: stats.Administrators, Unverified: stats.Unverified,
			Last7Days: stats.Last7Days, Last30Days: stats.Last30Days,
			ByStatus: []statusCount{}, ByRole: []roleCount{}, Recent: []recentAccount{},
		}
		for _, status := range []string{"ACTIVE", "SUSPENDED", "DELETED"} {
			s.ByStatus = append(s.ByStatus, statusCount{Status: status, Label: accountStatusLabel(status), Count: stats.ByStatus[status]})
		}
		for _, c := range stats.ByRole {
			label := labels[c.Role]
			if label == "" {
				label = c.Role
			}
			s.ByRole = append(s.ByRole, roleCount{Role: c.Role, Label: label, Status: c.Status, Count: c.Count})
		}
		for _, a := range stats.Recent {
			roles := a.Roles
			if roles == nil {
				roles = []string{}
			}
			s.Recent = append(s.Recent, recentAccount{ID: a.ID, Name: a.Name, Email: a.Email, EmailVerified: a.EmailVerified, Roles: roles, CreatedAt: a.CreatedAt.UTC()})
		}
		out.Accounts = s
		out.Sections = append(out.Sections, SectionAccounts)
	}

	// The verification queue's own counts, scoped by its own rule.
	switch page, err := h.src.Onboarding.List(ctx, p, "", "", "", 1, 1); {
	case errors.Is(err, onboarding.ErrForbidden):
	case err != nil:
		fail(err)
		return
	default:
		s := &verificationsSection{Waiting: page.Counts[onboarding.StatusSubmitted], ByStatus: []statusCount{}}
		for _, status := range onboarding.Statuses() {
			s.ByStatus = append(s.ByStatus, statusCount{Status: status, Label: onboarding.StatusLabel(status), Count: page.Counts[status]})
		}
		out.Verifications = s
		out.Sections = append(out.Sections, SectionVerifications)
	}

	if codes, all, ok := listingMarkets(p); ok && h.src.ListingCounts != nil {
		if all {
			codes = nil
		}
		counts, err := h.src.ListingCounts(ctx, codes)
		if err != nil {
			fail(err)
			return
		}
		s := &listingsSection{MarketCodes: codes, ByStatus: []statusCount{}}
		statuses := []string{"DRAFT", "PENDING_REVIEW", "PUBLISHED", "PAUSED", "EXPIRED", "SOLD", "LET", "CLOSED"}
		for status := range counts {
			if !slices.Contains(statuses, status) {
				statuses = append(statuses, status)
			}
		}
		for _, status := range statuses {
			s.Total += counts[status]
			s.ByStatus = append(s.ByStatus, statusCount{Status: status, Label: listingStatusName(status), Count: counts[status]})
		}
		out.Listings = s
		out.Sections = append(out.Sections, SectionListings)
	}

	ids, ok, err := propertyrequests.AdminMarketIDs(ctx, p, h.src.MarketIDs)
	if err != nil {
		fail(err)
		return
	}
	if ok {
		counts, err := h.src.Requests.AdminCounts(ctx, ids)
		if err != nil {
			fail(err)
			return
		}
		codes, all, _ := propertyrequests.AdminMarkets(p)
		if all {
			codes = nil
		}
		out.Requests = &requestsSection{
			MarketCodes: codes, Total: counts.Total, Last7Days: counts.Last7Days, Last30Days: counts.Last30Days,
			ByStage:           propertyrequests.StageCountsOf(counts.ByStage),
			RoutingExceptions: exceptionCounts{Open: counts.ExceptionsOpen, Resolved: counts.ExceptionsResolved},
		}
		out.Sections = append(out.Sections, SectionRequests)
	}

	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}
