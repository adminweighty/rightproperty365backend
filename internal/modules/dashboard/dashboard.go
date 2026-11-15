// Package dashboard answers "what should this person see first": one
// role-aware summary for the signed-in account (GET /v1/me/dashboard) and the
// platform administrators' overview. (An organisation's members are served by
// the teams module.)
//
// It owns no data. Every number comes from the module that owns it, through
// the same scoping that module's own endpoints use, so a dashboard can never
// show more than the inboxes and lists behind it.
package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/availability"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Dashboard kinds.
const (
	// KindSeeker: a property seeker (Prospective Buyer / Prospective Tenant).
	KindSeeker = "SEEKER"
	// KindProfessionalPending: holds a professional, business or owner role
	// and nothing has been approved yet.
	KindProfessionalPending = "PROFESSIONAL_PENDING"
	// KindProfessional: a verified member of an organisation (Organisations
	// lists which, with the caller's role there). Such a member may also be
	// an approved direct owner (DirectOwner).
	KindProfessional = "PROFESSIONAL"
	// KindDirectOwner: an approved landlord or property seller who belongs to
	// no organisation.
	KindDirectOwner = "DIRECT_OWNER"
	// KindAdmin: a platform administrator's session. Their numbers are at
	// GET /v1/admin/overview.
	KindAdmin = "ADMIN"
)

// Targets: where a counter or a next action leads. A client maps each key to
// one of its own pages.
const (
	TargetSearch            = "search"
	TargetMyRequests        = "my_requests"
	TargetMyAppointments    = "my_appointments"
	TargetOnboarding        = "onboarding"
	TargetRequests          = "requests"
	TargetBusinessEnquiries = "business_enquiries"
	TargetAppointments      = "appointments"
	TargetListings          = "listings"
	TargetLeases            = "leases"
	TargetAgreements        = "agreements"
	TargetSigning           = "signing_requests"
	TargetTeam              = "team"
	TargetAdminOverview     = "admin_overview"
)

// Counter groups.
const (
	GroupMyActivity      = "my_activity"
	GroupRequests        = "requests"
	GroupRequestsByStage = "requests_by_stage"
	GroupEnquiries       = "business_enquiries"
	GroupAppointments    = "appointments"
	GroupListings        = "listings"
	GroupLeases          = "leases"
)

// recentItems is how many requests and appointments a dashboard lists.
const recentItems = 5

// Sources is where the numbers come from.
type Sources struct {
	Requests     *propertyrequests.Service
	RequestViews *propertyrequests.Handler
	Appointments *appointments.Handler
	Onboarding   *onboarding.Service
	Listings     *availability.Service
	People       identity.PeopleStore
	// RoleLabels returns the label of every account role.
	RoleLabels func(ctx context.Context) (map[string]string, error)
	// Admin sources (see admin.go).
	ListingCounts func(ctx context.Context, marketCodes []string) (map[string]int, error)
	MarketIDs     propertyrequests.MarketIDs
	Now           func() time.Time
	// Leases counts the caller's leases (nil: no lease counters);
	// MayCreateLeases is leases.MayCreateLeases.
	Leases          func(ctx context.Context, p *auth.Principal) (LeaseCounts, error)
	MayCreateLeases func(p *auth.Principal) bool
	// Signing counts the documents waiting for the caller's signature and,
	// for a sender, the ones waiting for others (nil: none).
	Signing func(ctx context.Context, p *auth.Principal) (waitingForYou, waitingForOthers int, err error)
}

// LeaseCounts are the lease numbers of a professional's dashboard.
type LeaseCounts struct {
	Drafts             int
	AwaitingSignatures int
}

// Handler serves the endpoints of this package.
type Handler struct {
	src    Sources
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(src Sources, logger *slog.Logger) *Handler {
	if src.Now == nil {
		src.Now = time.Now
	}
	return &Handler{src: src, logger: logger}
}

// ---------------------------------------------------------------------------
// GET /v1/me/dashboard
// ---------------------------------------------------------------------------

// Counter is one number on a dashboard. Key is stable; Label is the wording.
type Counter struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Value int    `json:"value"`
	Group string `json:"group"`
	// Target is where the number leads, or null.
	Target *string `json:"target"`
}

// Action is one thing the person should do next. Code is stable.
type Action struct {
	Code   string `json:"code"`
	Text   string `json:"text"`
	Target string `json:"target"`
	// Count is how many items wait, or null when the action is not about a number.
	Count *int `json:"count"`
	// Role is the account role an onboarding action is about, or null.
	Role *string `json:"role"`
}

// RoleView is one account role of the caller.
type RoleView struct {
	Role   string `json:"role"`
	Label  string `json:"label"`
	Status string `json:"status"`
}

// OrganisationView is one organisation the caller belongs to, with what the
// caller may do there.
type OrganisationView struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	MarketCode string    `json:"market_code"`
	Role       string    `json:"role"`
	RoleLabel  string    `json:"role_label"`
	// CanSeeMembers: GET /v1/agent/organisations/{id}/members is open to the caller.
	CanSeeMembers bool `json:"can_see_members"`
	// CanAssignWork: the caller sees the whole organisation's requests and appointments.
	CanAssignWork     bool `json:"can_assign_work"`
	CanManageListings bool `json:"can_manage_listings"`
	// CanManageTeam: the caller may invite people, decide join requests and
	// change, suspend or remove members (OWNER and MANAGER; what each may do
	// to whom is in GET /v1/agent/organisations/{id}/members).
	CanManageTeam bool `json:"can_manage_team"`
	// CanCreateLeases: the caller may draft leases for the organisation
	// (POST /v1/me/leases): exactly can_manage_listings.
	CanCreateLeases bool `json:"can_create_leases"`
}

// OnboardingView is the caller's application for one role.
type OnboardingView struct {
	Role             string     `json:"role"`
	RoleLabel        string     `json:"role_label"`
	Status           string     `json:"status"`
	StatusLabel      string     `json:"status_label"`
	CanSubmit        bool       `json:"can_submit"`
	CanEdit          bool       `json:"can_edit"`
	MissingFields    int        `json:"missing_fields"`
	MissingDocuments int        `json:"missing_documents"`
	DecisionReason   *string    `json:"decision_reason"`
	SubmittedAt      *time.Time `json:"submitted_at"`
}

// Capabilities says which parts of the product are open to the caller, so a
// client can build its navigation without knowing the role rules.
type Capabilities struct {
	ReceivesRequests          bool `json:"receives_requests"`
	ReceivesBusinessEnquiries bool `json:"receives_business_enquiries"`
	ManagesAppointments       bool `json:"manages_appointments"`
	ManagesListings           bool `json:"manages_listings"`
	SeesTeam                  bool `json:"sees_team"`
	// CreatesLeases: /v1/me/leases is open to the caller.
	CreatesLeases bool `json:"creates_leases"`
}

// Recent holds the short lists of a dashboard. Every array is always present.
type Recent struct {
	// ReceivedRequests: the newest requests in the caller's inbox.
	ReceivedRequests []propertyrequests.InboxItem `json:"received_requests"`
	// MyRequests: the newest requests the caller sent.
	MyRequests []propertyrequests.CustomerView `json:"my_requests"`
	// Appointments: the next upcoming ones, soonest first.
	Appointments []appointments.Brief `json:"appointments"`
}

// Dashboard is the body of GET /v1/me/dashboard.
type Dashboard struct {
	Kind          string             `json:"kind"`
	Name          string             `json:"name"`
	Roles         []RoleView         `json:"roles"`
	Organisations []OrganisationView `json:"organisations"`
	// DirectOwner: the caller is an approved landlord or property seller.
	DirectOwner  bool             `json:"direct_owner"`
	Capabilities Capabilities     `json:"capabilities"`
	Onboarding   []OnboardingView `json:"onboarding"`
	Counters     []Counter        `json:"counters"`
	// NeedsAttention is the number of items waiting on the caller: the sum
	// of the counts of NextActions (a badge for the dashboard link).
	NeedsAttention int      `json:"needs_attention"`
	NextActions    []Action `json:"next_actions"`
	Recent         Recent   `json:"recent"`
}

func target(t string) *string { return &t }

func count(n int) *int { return &n }

// kindOf decides which dashboard applies. Access is never derived from it:
// every number below is scoped by the module that owns it.
func kindOf(p *auth.Principal) string {
	switch {
	case p.SessionScope == auth.ScopeAdmin:
		return KindAdmin
	case p.HasMembership():
		return KindProfessional
	case p.IsDirectOwner():
		return KindDirectOwner
	}
	for _, r := range p.AccountRoles {
		if !identity.IsSeekerRole(r.Role) {
			return KindProfessionalPending
		}
	}
	return KindSeeker
}

// build assembles the dashboard of a caller.
func (h *Handler) build(ctx context.Context, p *auth.Principal) (Dashboard, error) {
	d := Dashboard{
		Kind: kindOf(p), Name: p.Name, Roles: []RoleView{}, Organisations: []OrganisationView{},
		DirectOwner: p.IsDirectOwner(), Onboarding: []OnboardingView{}, Counters: []Counter{}, NextActions: []Action{},
		Recent: Recent{
			ReceivedRequests: []propertyrequests.InboxItem{}, MyRequests: []propertyrequests.CustomerView{},
			Appointments: []appointments.Brief{},
		},
	}
	if d.Kind == KindAdmin {
		// An administrator works from the overview; nothing of the customer
		// side is shown on an administrator's session.
		d.NextActions = append(d.NextActions, Action{Code: "OPEN_ADMIN_OVERVIEW", Text: "Open the administration overview", Target: TargetAdminOverview})
		return d, nil
	}

	labels := map[string]string{}
	if h.src.RoleLabels != nil {
		var err error
		if labels, err = h.src.RoleLabels(ctx); err != nil {
			return Dashboard{}, err
		}
	}
	roleLabel := func(role string) string {
		if l := labels[role]; l != "" {
			return l
		}
		return role
	}
	for _, r := range p.AccountRoles {
		d.Roles = append(d.Roles, RoleView{Role: r.Role, Label: roleLabel(r.Role), Status: r.Status})
	}
	for _, m := range p.Memberships {
		d.Organisations = append(d.Organisations, OrganisationView{
			ID: m.OrganisationID, Name: m.OrganisationName, Kind: m.OrganisationKind, MarketCode: m.MarketCode,
			Role: m.Role, RoleLabel: auth.MembershipRoleLabel(m.OrganisationKind, m.Role),
			CanSeeMembers: p.MayAssignWork(m.OrganisationID), CanAssignWork: p.MayAssignWork(m.OrganisationID),
			CanManageListings: p.MayManageListings(m.OrganisationID), CanManageTeam: p.MayManageTeam(m.OrganisationID),
			CanCreateLeases: p.MayManageListings(m.OrganisationID),
		})
		d.Capabilities.SeesTeam = d.Capabilities.SeesTeam || p.MayAssignWork(m.OrganisationID)
	}

	// Onboarding: one entry per role that has an application.
	apps, err := h.src.Onboarding.Mine(ctx, p)
	if err != nil {
		return Dashboard{}, err
	}
	for _, a := range apps {
		d.Onboarding = append(d.Onboarding, OnboardingView{
			Role: a.Role, RoleLabel: a.RoleLabel, Status: a.Status, StatusLabel: a.StatusLabel,
			CanSubmit: a.CanSubmit, CanEdit: a.CanEdit,
			MissingFields: len(a.Missing.Fields), MissingDocuments: len(a.Missing.Documents),
			DecisionReason: a.Decision.Reason, SubmittedAt: a.SubmittedAt,
		})
	}

	if err := h.professional(ctx, p, &d); err != nil {
		return Dashboard{}, err
	}
	if err := h.customer(ctx, p, &d); err != nil {
		return Dashboard{}, err
	}
	if h.src.Signing != nil {
		you, others, err := h.src.Signing(ctx, p)
		if err != nil {
			return Dashboard{}, err
		}
		if you > 0 || d.Kind == KindSeeker {
			d.Counters = append(d.Counters, Counter{Key: "agreements.waiting_for_you", Label: "Documents waiting for your signature", Value: you, Group: GroupMyActivity, Target: target(TargetSigning)})
		}
		if you > 0 {
			d.NextActions = append(d.NextActions, Action{Code: "SIGN_DOCUMENTS", Text: plural(you, "document is waiting for your signature", "documents are waiting for your signature"), Target: TargetSigning, Count: count(you)})
		}
		if d.Capabilities.CreatesLeases {
			d.Counters = append(d.Counters, Counter{Key: "agreements.waiting_for_others", Label: "Documents waiting for others", Value: others, Group: GroupLeases, Target: target(TargetAgreements)})
		}
	}
	h.onboardingActions(&d)
	if d.Kind == KindSeeker {
		d.NextActions = append(d.NextActions, Action{Code: "FIND_A_PROPERTY", Text: "Search for a property", Target: TargetSearch})
	}
	for _, a := range d.NextActions {
		if a.Count != nil {
			d.NeedsAttention += *a.Count
		}
	}
	return d, nil
}

// professional adds what the caller RECEIVES: requests, business enquiries,
// appointments and listings. Each part appears only when its own endpoint is
// open to the caller, and holds exactly what that endpoint would return.
func (h *Handler) professional(ctx context.Context, p *auth.Principal, d *Dashboard) error {
	// The badge numbers of GET /v1/inbox/summary.
	summary, err := h.src.Appointments.SummaryFor(ctx, p)
	if err != nil {
		return err
	}

	// Requests: GET /v1/agent/property-requests.
	if d.Capabilities.ReceivesRequests, err = h.src.Requests.MayOpenInbox(ctx, p); err != nil {
		return err
	}
	if d.Capabilities.ReceivesRequests {
		scope := propertyrequests.ScopeOf(p)
		page, err := h.src.Requests.InboxPage(ctx, propertyrequests.InboxFilter{Scope: scope, Limit: recentItems})
		if err != nil {
			return err
		}
		if d.Recent.ReceivedRequests, err = h.src.RequestViews.Items(ctx, page.Items); err != nil {
			return err
		}
		open := 0
		for stage, n := range page.StageCounts {
			if stage != propertyrequests.StageConverted && stage != propertyrequests.StageClosed {
				open += n
			}
		}
		d.Counters = append(d.Counters,
			Counter{Key: "requests.new", Label: "New requests", Value: summary.PropertyRequests.New, Group: GroupRequests, Target: target(TargetRequests)},
			Counter{Key: "requests.open", Label: "Open requests", Value: open, Group: GroupRequests, Target: target(TargetRequests)},
		)
		for _, c := range propertyrequests.StageCountsOf(page.StageCounts) {
			d.Counters = append(d.Counters, Counter{
				Key: "requests.stage." + c.Stage, Label: c.Label, Value: c.Count, Group: GroupRequestsByStage, Target: target(TargetRequests),
			})
		}
		if n := summary.PropertyRequests.New; n > 0 {
			d.NextActions = append(d.NextActions, Action{Code: "RESPOND_TO_NEW_REQUESTS", Text: plural(n, "new request is waiting for a reply", "new requests are waiting for a reply"), Target: TargetRequests, Count: count(n)})
		}
		if n := page.StageCounts[propertyrequests.StageFollowUpRequired]; n > 0 {
			d.NextActions = append(d.NextActions, Action{Code: "FOLLOW_UP_REQUESTS", Text: plural(n, "request needs a follow-up", "requests need a follow-up"), Target: TargetRequests, Count: count(n)})
		}
	}

	// Business enquiries: GET /v1/inbox/business-enquiries (any membership).
	if d.Capabilities.ReceivesBusinessEnquiries = p.HasMembership(); d.Capabilities.ReceivesBusinessEnquiries {
		n := summary.BusinessEnquiries.New
		d.Counters = append(d.Counters, Counter{Key: "business_enquiries.new", Label: "New business enquiries", Value: n, Group: GroupEnquiries, Target: target(TargetBusinessEnquiries)})
		if n > 0 {
			d.NextActions = append(d.NextActions, Action{Code: "RESPOND_TO_BUSINESS_ENQUIRIES", Text: plural(n, "new business enquiry is waiting", "new business enquiries are waiting"), Target: TargetBusinessEnquiries, Count: count(n)})
		}
	}

	// Appointments: GET /v1/inbox/appointments.
	if d.Capabilities.ManagesAppointments = !appointments.ScopeOf(p).Empty(); d.Capabilities.ManagesAppointments {
		a := summary.Appointments
		d.Counters = append(d.Counters,
			Counter{Key: "appointments.requested", Label: "Appointments to confirm", Value: a.Requested, Group: GroupAppointments, Target: target(TargetAppointments)},
			Counter{Key: "appointments.reschedule_requested", Label: "New times to agree", Value: a.RescheduleRequested, Group: GroupAppointments, Target: target(TargetAppointments)},
			Counter{Key: "appointments.upcoming_confirmed", Label: "Upcoming confirmed appointments", Value: a.UpcomingConfirmed, Group: GroupAppointments, Target: target(TargetAppointments)},
		)
		if a.Requested > 0 {
			d.NextActions = append(d.NextActions, Action{Code: "CONFIRM_APPOINTMENTS", Text: plural(a.Requested, "appointment is waiting to be confirmed", "appointments are waiting to be confirmed"), Target: TargetAppointments, Count: count(a.Requested)})
		}
		if a.RescheduleRequested > 0 {
			d.NextActions = append(d.NextActions, Action{Code: "REVIEW_RESCHEDULES", Text: plural(a.RescheduleRequested, "appointment has a new time to agree", "appointments have a new time to agree"), Target: TargetAppointments, Count: count(a.RescheduleRequested)})
		}
		next, err := h.src.Appointments.NextReceived(ctx, p, recentItems)
		if err != nil {
			return err
		}
		d.Recent.Appointments = append(d.Recent.Appointments, next...)
	}

	// Leases: GET /v1/me/leases.
	if h.src.MayCreateLeases != nil && h.src.Leases != nil {
		if d.Capabilities.CreatesLeases = h.src.MayCreateLeases(p); d.Capabilities.CreatesLeases {
			c, err := h.src.Leases(ctx, p)
			if err != nil {
				return err
			}
			d.Counters = append(d.Counters,
				Counter{Key: "leases.awaiting_signatures", Label: "Leases waiting for signatures", Value: c.AwaitingSignatures, Group: GroupLeases, Target: target(TargetLeases)},
				Counter{Key: "leases.drafts", Label: "Draft leases", Value: c.Drafts, Group: GroupLeases, Target: target(TargetLeases)},
			)
		}
	}

	// Listings: GET /v1/me/listings.
	if d.Capabilities.ManagesListings = p.ManagesListings(); d.Capabilities.ManagesListings {
		counts, err := h.src.Listings.ListingCounts(ctx, p)
		if err != nil {
			return err
		}
		d.Counters = append(d.Counters, Counter{Key: "listings.total", Label: "Listings", Value: counts.Total, Group: GroupListings, Target: target(TargetListings)})
		// PUBLISHED and the review statuses always, then any other status
		// the caller has listings in.
		fixed := []string{"PUBLISHED", "DRAFT", "PENDING_REVIEW", "CHANGES_REQUESTED"}
		statuses := append([]string{}, fixed...)
		for status := range counts.ByStatus {
			if !slices.Contains(statuses, status) {
				statuses = append(statuses, status)
			}
		}
		slices.Sort(statuses[len(fixed):])
		for _, status := range statuses {
			d.Counters = append(d.Counters, Counter{
				Key: "listings.status." + status, Label: listingStatusLabel(status), Value: counts.ByStatus[status], Group: GroupListings, Target: target(TargetListings),
			})
		}
		if n := counts.ByStatus["CHANGES_REQUESTED"]; n > 0 {
			d.NextActions = append(d.NextActions, Action{Code: "UPDATE_LISTINGS_FOR_REVIEW", Text: plural(n, "listing needs changes before it can be published", "listings need changes before they can be published"), Target: TargetListings, Count: count(n)})
		}
		if counts.ShortStays > 0 {
			d.Counters = append(d.Counters, Counter{Key: "short_stays.calendar_stale", Label: "Short stays needing a calendar update", Value: counts.StaleCalendars, Group: GroupListings, Target: target(TargetListings)})
		}
		if n := counts.StaleCalendars; n > 0 {
			d.NextActions = append(d.NextActions, Action{Code: "UPDATE_SHORT_STAY_CALENDARS", Text: plural(n, "short stay needs its calendar updated", "short stays need their calendars updated"), Target: TargetListings, Count: count(n)})
		}
	}
	return nil
}

// customer adds what the caller SENT: their own requests and appointments.
// A seeker always gets these; anyone else only once they have some.
func (h *Handler) customer(ctx context.Context, p *auth.Principal, d *Dashboard) error {
	mine, err := h.src.Requests.Mine(ctx, p.UserID, nil, recentItems, 0)
	if err != nil {
		return err
	}
	appts, err := h.src.Appointments.MineSummaryFor(ctx, p, recentItems)
	if err != nil {
		return err
	}
	seeker := d.Kind == KindSeeker
	if seeker || mine.Total > 0 {
		if d.Recent.MyRequests, err = h.src.RequestViews.CustomerViews(ctx, mine.Items); err != nil {
			return err
		}
		total := 0
		for _, n := range mine.StatusCounts {
			total += n
		}
		d.Counters = append(d.Counters,
			Counter{Key: "my_requests.total", Label: "Requests sent", Value: total, Group: GroupMyActivity, Target: target(TargetMyRequests)},
			Counter{Key: "my_requests.open", Label: "Requests in progress", Value: total - mine.StatusCounts[propertyrequests.CustomerClosed], Group: GroupMyActivity, Target: target(TargetMyRequests)},
		)
		for _, c := range propertyrequests.StatusCountsOf(mine.StatusCounts) {
			d.Counters = append(d.Counters, Counter{Key: "my_requests.status." + c.Status, Label: c.Label, Value: c.Count, Group: GroupMyActivity, Target: target(TargetMyRequests)})
		}
	}
	if seeker || appts.Upcoming > 0 {
		d.Counters = append(d.Counters,
			Counter{Key: "my_appointments.upcoming", Label: "Upcoming appointments", Value: appts.Upcoming, Group: GroupMyActivity, Target: target(TargetMyAppointments)},
			Counter{Key: "my_appointments.needs_reply", Label: "Appointments waiting for your reply", Value: appts.NeedsReply, Group: GroupMyActivity, Target: target(TargetMyAppointments)},
		)
		d.Recent.Appointments = append(d.Recent.Appointments, appts.Next...)
		slices.SortStableFunc(d.Recent.Appointments, func(a, b appointments.Brief) int { return a.StartUTC.Compare(b.StartUTC) })
		if len(d.Recent.Appointments) > recentItems {
			d.Recent.Appointments = d.Recent.Appointments[:recentItems]
		}
	}
	if n := appts.NeedsReply; n > 0 {
		d.NextActions = append(d.NextActions, Action{Code: "REPLY_TO_APPOINTMENT", Text: plural(n, "appointment has a new time waiting for your reply", "appointments have a new time waiting for your reply"), Target: TargetMyAppointments, Count: count(n)})
	}
	return nil
}

// onboardingActions says what each application needs from the person.
func (h *Handler) onboardingActions(d *Dashboard) {
	for _, a := range d.Onboarding {
		role := a.Role
		action := Action{Target: TargetOnboarding, Role: &role}
		switch a.Status {
		case onboarding.StatusDraft:
			action.Code, action.Text = "COMPLETE_ONBOARDING", "Complete and submit your "+a.RoleLabel+" verification"
		case onboarding.StatusChangesRequested:
			action.Code, action.Text = "UPDATE_ONBOARDING", "Changes were requested on your "+a.RoleLabel+" verification"
		case onboarding.StatusSubmitted:
			action.Code, action.Text = "AWAIT_VERIFICATION", "Your "+a.RoleLabel+" verification is being reviewed"
		case onboarding.StatusRejected:
			action.Code, action.Text = "REAPPLY_ONBOARDING", "Your "+a.RoleLabel+" verification was not approved; you can apply again"
		default:
			continue
		}
		d.NextActions = append(d.NextActions, action)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// listingStatusLabel words a listing status: "PENDING_REVIEW" -> "Pending review".
func listingStatusLabel(status string) string {
	if status == "" {
		return status
	}
	words := strings.ToLower(strings.ReplaceAll(status, "_", " "))
	return strings.ToUpper(words[:1]) + words[1:] + " listings"
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

// Me serves GET /v1/me/dashboard.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	d, err := h.build(r.Context(), p)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, d)
}
