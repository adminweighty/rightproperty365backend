package propertyrequests

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// CodeOrganisationNotFound: the organisation does not exist or the caller is
// not one of its members (the same answer, so nothing leaks).
const CodeOrganisationNotFound = "ORGANISATION_NOT_FOUND"

const dateLayout = "2006-01-02"

var marketCodePattern = regexp.MustCompile(`^[A-Z]{2}$`)

type funnelWindow struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Days     int    `json:"days"`
	Timezone string `json:"timezone"`
}

type funnelFilters struct {
	AgentUserID *uuid.UUID `json:"agent_user_id"`
	ListingID   *uuid.UUID `json:"listing_id"`
	RequestType *string    `json:"request_type"`
	MarketCode  *string    `json:"market_code"`
}

// funnelResponse is the body of both lead-funnel routes.
type funnelResponse struct {
	OrganisationID *uuid.UUID    `json:"organisation_id"`
	Scope          string        `json:"scope"`
	Window         funnelWindow  `json:"window"`
	Filters        funnelFilters `json:"filters"`
	GeneratedAt    time.Time     `json:"generated_at"`
	Funnel
}

// LeadFunnel serves GET /v1/agent/organisations/{id}/reports/lead-funnel:
// OWNER and MANAGER see the whole organisation, AGENT and STAFF what their
// inbox shows (their own requests and the unassigned organisation inbox),
// anybody else 404.
func (h *Handler) LeadFunnel(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		unauthenticated(w, r)
		return
	}
	org, err := uuid.Parse(chi.URLParam(r, "id"))
	role := ""
	if err == nil {
		role = p.MembershipRole(org)
	}
	scope, label := InboxScope{UserID: p.UserID}, ""
	switch role {
	case auth.RoleOwner, auth.RoleManager:
		scope.ManagedOrganisationIDs, label = []uuid.UUID{org}, FunnelScopeOrganisation
	case auth.RoleAgent, auth.RoleStaff:
		scope.MemberOrganisationIDs, label = []uuid.UUID{org}, FunnelScopeMember
	default:
		httpx.WriteError(w, r, http.StatusNotFound, CodeOrganisationNotFound, "This organisation does not exist.", nil)
		return
	}
	h.leadFunnel(w, r, scope, &org, label)
}

// MyLeadFunnel serves GET /v1/me/reports/lead-funnel: the requests routed to
// the caller as a direct seller. Who may call it is who may open the inbox.
func (h *Handler) MyLeadFunnel(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.receiver(w, r)
	if !ok {
		return
	}
	h.leadFunnel(w, r, InboxScope{UserID: p.UserID}, nil, FunnelScopeDirectSeller)
}

func (h *Handler) leadFunnel(w http.ResponseWriter, r *http.Request, scope InboxScope, org *uuid.UUID, label string) {
	q, fields := r.URL.Query(), map[string]string{}
	f := FunnelFilter{Scope: scope, OrganisationID: org}
	resp := funnelResponse{OrganisationID: org, Scope: label, GeneratedAt: h.svc.clock()}

	today := resp.GeneratedAt.Truncate(24 * time.Hour)
	date := func(name string) *time.Time {
		raw := strings.TrimSpace(q.Get(name))
		if raw == "" {
			return nil
		}
		t, err := time.Parse(dateLayout, raw)
		if err != nil {
			fields[name] = "must be a date YYYY-MM-DD"
			return nil
		}
		return &t
	}
	from, to := date("from"), date("to")
	if to == nil {
		to = &today
	}
	if from == nil {
		d := to.AddDate(0, 0, -(DefaultFunnelDays - 1))
		from = &d
	}
	days := int(to.Sub(*from).Hours()/24) + 1
	switch {
	case fields["from"] != "" || fields["to"] != "":
	case days < 1:
		fields["to"] = "must not be before from"
	case days > MaxFunnelDays:
		fields["to"] = "the window can be at most " + strconv.Itoa(MaxFunnelDays) + " days"
	}
	f.From, f.To = *from, to.AddDate(0, 0, 1)
	resp.Window = funnelWindow{From: from.Format(dateLayout), To: to.Format(dateLayout), Days: days, Timezone: "UTC"}

	for name, dst := range map[string]**uuid.UUID{"agent_user_id": &f.AgentUserID, "listing_id": &f.ListingID} {
		if raw := strings.TrimSpace(q.Get(name)); raw != "" {
			id, err := uuid.Parse(raw)
			if err != nil {
				fields[name] = "must be a UUID"
				continue
			}
			*dst = &id
		}
	}
	resp.Filters.AgentUserID, resp.Filters.ListingID = f.AgentUserID, f.ListingID
	if raw := strings.TrimSpace(q.Get("request_type")); raw != "" {
		if !slices.Contains(requestTypes(), raw) {
			fields["request_type"] = "must be one of " + strings.Join(requestTypes(), ", ")
		} else {
			f.RequestType, resp.Filters.RequestType = raw, &raw
		}
	}
	if raw := strings.ToUpper(strings.TrimSpace(q.Get("market_code"))); raw != "" {
		var ids []uuid.UUID
		if marketCodePattern.MatchString(raw) && h.markets != nil {
			var err error
			if ids, err = h.markets(r.Context(), []string{raw}); err != nil {
				httpx.WriteInternalError(w, r, h.logger, err)
				return
			}
		}
		if len(ids) == 0 {
			fields["market_code"] = "is not a known market"
		} else {
			f.MarketIDs, resp.Filters.MarketCode = ids, &raw
		}
	}
	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format != "" && format != "json" && format != "csv" {
		fields["format"] = "must be json or csv"
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}

	report, err := h.svc.FunnelReport(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ids := make([]uuid.UUID, 0, len(report.ByListing))
	for _, row := range report.ByListing {
		ids = append(ids, row.ListingID)
	}
	names, err := h.listingNames(r.Context(), ids)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	for i := range report.ByListing {
		report.ByListing[i].Listing = listingOf(names, report.ByListing[i].ListingID)
	}
	resp.Funnel = report
	w.Header().Set("Cache-Control", "private, no-store")
	if format == "csv" {
		h.writeFunnelCSV(w, resp)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// FunnelCSVHeader is the header row of the CSV form of a report.
func FunnelCSVHeader() []string {
	return []string{"table", "key", "label", "received", "contacted", "converted", "conversion_rate", "median_first_response_minutes"}
}

// csvCell neutralises spreadsheet formulas, as the CRM export does.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func csvRow(table, key, label string, b BreakdownCounts) []string {
	rate, median := "", ""
	if b.ConversionRate != nil {
		rate = *b.ConversionRate
	}
	if b.MedianFirstResponseMinutes != nil {
		median = strconv.FormatFloat(*b.MedianFirstResponseMinutes, 'f', -1, 64)
	}
	return []string{table, csvCell(key), csvCell(label), strconv.Itoa(b.Received), strconv.Itoa(b.Contacted), strconv.Itoa(b.Converted), rate, median}
}

// FunnelCSVRows are the breakdown tables of a report as CSV rows (without
// the header): agents, then request types, then listings.
func FunnelCSVRows(report Funnel) [][]string {
	var out [][]string
	for _, a := range report.ByAgent {
		key := ""
		if a.UserID != nil {
			key = a.UserID.String()
		}
		out = append(out, csvRow("agent", key, a.Name, a.BreakdownCounts))
	}
	for _, t := range report.ByRequestType {
		out = append(out, csvRow("request_type", t.RequestType, t.Label, t.BreakdownCounts))
	}
	for _, l := range report.ByListing {
		title := ""
		if l.Listing != nil {
			title = l.Listing.Title
		}
		out = append(out, csvRow("listing", l.ListingID.String(), title, l.BreakdownCounts))
	}
	return out
}

func (h *Handler) writeFunnelCSV(w http.ResponseWriter, resp funnelResponse) {
	hd := w.Header()
	hd.Set("Content-Type", "text/csv; charset=utf-8")
	hd.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="lead-funnel-%s-to-%s.csv"`, resp.Window.From, resp.Window.To))
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write(FunnelCSVHeader())
	_ = cw.WriteAll(FunnelCSVRows(resp.Funnel))
}
