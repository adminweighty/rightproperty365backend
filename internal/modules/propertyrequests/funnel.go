package propertyrequests

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// This file is the lead conversion funnel and the response-time KPIs of the
// request inbox (API 0.38.0, BR-005): GET
// /v1/agent/organisations/{id}/reports/lead-funnel and its direct seller's
// form GET /v1/me/reports/lead-funnel (funnel_handler.go).
//
// Both stores only hand over the facts of the cohort (FunnelFacts): each
// request with its stage moves and whether it was ever assigned. Everything
// is counted here, in one place, so the memory and Postgres stores cannot
// disagree.
//
// Counting rules (also in the README and the 0.38 contract):
//
//   - Ranks: NEW_ENQUIRY 0, ASSIGNED 1, CONTACTED 2, VIEWING_REQUESTED 3,
//     VIEWING_SCHEDULED 4, CONVERTED 5. FOLLOW_UP_REQUIRED and CLOSED have
//     none and never move a request through the funnel.
//   - A request reached step k when the highest rank among its current stage
//     and every STAGE_CHANGED to-stage is at least k: skipped stages count
//     (CONTACTED straight to VIEWING_SCHEDULED reached VIEWING_REQUESTED) and
//     moving back undoes nothing.
//   - It also reached ASSIGNED when it has an ASSIGNED timeline row or is
//     routed to a person now (recipient AGENT or SELLER).
//   - Its first response is the first STAGE_CHANGED by the receiving side to
//     CONTACTED or a later ranked stage. Assigning, FOLLOW_UP_REQUIRED,
//     CLOSED, notes and appointments are not responses.

// Funnel steps, in order. Each is a ranked lead stage; RECEIVED stands for
// NEW_ENQUIRY.
const (
	StepReceived = "RECEIVED"
)

// MaxFunnelDays is the longest report window, in days (inclusive).
const MaxFunnelDays = 366

// DefaultFunnelDays is the window when the caller gives no from.
const DefaultFunnelDays = 30

// TopFunnelListings is the number of listings in the by-listing breakdown.
const TopFunnelListings = 20

// Report scopes.
const (
	FunnelScopeOrganisation = "ORGANISATION"
	FunnelScopeMember       = "MEMBER"
	FunnelScopeDirectSeller = "DIRECT_SELLER"
)

// OrganisationInboxName names the by-agent row of unassigned requests.
const OrganisationInboxName = "Organisation inbox"

type funnelStep struct {
	Step  string
	Stage string
	Label string
}

func funnelSteps() []funnelStep {
	return []funnelStep{
		{StepReceived, StageNewEnquiry, "Received"},
		{StageAssigned, StageAssigned, "Assigned"},
		{StageContacted, StageContacted, "Contacted"},
		{StageViewingRequested, StageViewingRequested, "Viewing requested"},
		{StageViewingScheduled, StageViewingScheduled, "Viewing scheduled"},
		{StageConverted, StageConverted, "Converted"},
	}
}

// stageRank is the funnel rank of a stage, -1 for one without (FOLLOW_UP_REQUIRED, CLOSED).
func stageRank(stage string) int {
	switch stage {
	case StageNewEnquiry:
		return 0
	case StageAssigned:
		return 1
	case StageContacted:
		return 2
	case StageViewingRequested:
		return 3
	case StageViewingScheduled:
		return 4
	case StageConverted:
		return 5
	}
	return -1
}

const (
	rankAssigned  = 1
	rankContacted = 2
	rankConverted = 5
)

var requestTypeLabels = map[string]string{
	TypeEnquiry:             "Enquiry",
	TypePropertyInformation: "Property information",
	TypeViewingRequest:      "Viewing request",
	TypeConsultationRequest: "Consultation request",
	TypeStayRequest:         "Request to stay",
}

// RequestTypeLabel is the wording of a request type.
func RequestTypeLabel(t string) string {
	if l, ok := requestTypeLabels[t]; ok {
		return l
	}
	return t
}

// FunnelFilter selects the cohort of a report. Scope decides what can be
// seen at all, as in the inbox; everything else only narrows it.
type FunnelFilter struct {
	Scope InboxScope
	// OrganisationID keeps only the requests received by this organisation
	// (the organisation report). The /v1/me report leaves it nil and passes
	// a scope without organisations, which sees only the caller's
	// direct-seller requests.
	OrganisationID *uuid.UUID
	// From (inclusive) and To (exclusive) bound created_at, in UTC.
	From time.Time
	To   time.Time
	// AgentUserID keeps the requests currently routed to that person.
	AgentUserID *uuid.UUID
	ListingID   *uuid.UUID
	RequestType string
	// MarketIDs keeps the requests of these markets; nil: every market.
	MarketIDs []uuid.UUID
}

// inbox is the inbox filter with the same scope, type, listing and window.
func (f FunnelFilter) inbox() InboxFilter {
	from, to := f.From, f.To
	return InboxFilter{Scope: f.Scope, Type: f.RequestType, ListingID: f.ListingID, From: &from, To: &to}
}

// matches applies the whole filter (memory store; Postgres has the same in SQL).
func (f FunnelFilter) matches(r StoredRequest) bool {
	if !f.inbox().matches(r) {
		return false
	}
	if f.OrganisationID != nil && (r.Recipient.OrganisationID == nil || *r.Recipient.OrganisationID != *f.OrganisationID) {
		return false
	}
	if f.AgentUserID != nil && (r.Recipient.UserID == nil || *r.Recipient.UserID != *f.AgentUserID) {
		return false
	}
	return f.MarketIDs == nil || slices.Contains(f.MarketIDs, r.MarketID)
}

// FunnelMove is one STAGE_CHANGED row of a request's history.
type FunnelMove struct {
	To        string
	ActorType string
	At        time.Time
}

// FunnelFact is what a report needs to know about one request.
type FunnelFact struct {
	RequestID   uuid.UUID
	ListingID   uuid.UUID
	RequestType string
	Stage       string
	CreatedAt   time.Time
	// RecipientType and HandlerUserID are the current routing: the person it
	// is assigned to (or the direct seller), nil for the organisation inbox.
	RecipientType string
	HandlerUserID *uuid.UUID
	// EverAssigned: the history has an ASSIGNED row.
	EverAssigned bool
	// Moves are the STAGE_CHANGED rows, oldest first.
	Moves []FunnelMove
}

// FunnelStore is the part of Store behind this file.
type FunnelStore interface {
	// FunnelFacts returns the facts of every request under the filter.
	FunnelFacts(ctx context.Context, f FunnelFilter) ([]FunnelFact, error)
}

// factOf is the derived state of one request.
type factOf struct {
	furthest        int
	firstResponse   *time.Duration
	toConversion    *time.Duration
	everFollowUp    bool
	everConverted   bool
	reachedAssigned bool
}

func derive(f FunnelFact) factOf {
	d := factOf{furthest: max(stageRank(f.Stage), 0)}
	for _, m := range f.Moves {
		rank := stageRank(m.To)
		d.furthest = max(d.furthest, rank)
		if m.To == StageFollowUpRequired {
			d.everFollowUp = true
		}
		if rank >= rankContacted && m.ActorType == ActorProfessional && d.firstResponse == nil {
			v := m.At.Sub(f.CreatedAt)
			d.firstResponse = &v
		}
		if m.To == StageConverted && d.toConversion == nil {
			v := m.At.Sub(f.CreatedAt)
			d.toConversion = &v
		}
	}
	d.everConverted = d.furthest >= rankConverted
	d.reachedAssigned = d.furthest >= rankAssigned || f.EverAssigned ||
		f.RecipientType == RecipientAgent || f.RecipientType == RecipientSeller
	return d
}

// reached reports whether a request reached the step of rank k.
func (d factOf) reached(k int) bool {
	if k == rankAssigned {
		return d.reachedAssigned
	}
	return d.furthest >= k
}

// Rate is a ratio as a decimal string with four places, rounded half up;
// nil when the denominator is 0.
func Rate(n, d int) *string {
	if d <= 0 {
		return nil
	}
	scaled := (int64(n)*20000 + int64(d)) / (2 * int64(d))
	s := fmt.Sprintf("%d.%04d", scaled/10000, scaled%10000)
	return &s
}

// Median is the middle value of the sorted values, or the mean of the two
// middle ones; nil for none.
func Median(sorted []float64) *float64 {
	n := len(sorted)
	if n == 0 {
		return nil
	}
	v := sorted[n/2]
	if n%2 == 0 {
		v = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return &v
}

// Percentile is the nearest-rank percentile p (0 < p <= 100) of the sorted
// values: the value at position ceil(p/100 * n); nil for none.
func Percentile(sorted []float64, p float64) *float64 {
	n := len(sorted)
	if n == 0 {
		return nil
	}
	pos := int(math.Ceil(p / 100 * float64(n)))
	pos = min(max(pos, 1), n)
	v := sorted[pos-1]
	return &v
}

func round(v *float64, places int) *float64 {
	if v == nil {
		return nil
	}
	f := math.Pow10(places)
	r := math.Round(*v*f) / f
	return &r
}

// FunnelStepView is one step of the funnel.
type FunnelStepView struct {
	Step                   string  `json:"step"`
	Label                  string  `json:"label"`
	Reached                int     `json:"reached"`
	ConversionFromPrevious *string `json:"conversion_from_previous"`
	ConversionFromReceived *string `json:"conversion_from_received"`
	Current                int     `json:"current"`
}

// FollowUpCounts are the requests at FOLLOW_UP_REQUIRED now and ever.
type FollowUpCounts struct {
	Current int `json:"current"`
	Ever    int `json:"ever"`
}

// SideStates are the counts outside the funnel's straight line.
type SideStates struct {
	ClosedWithoutConverting int            `json:"closed_without_converting"`
	ConvertedThenClosed     int            `json:"converted_then_closed"`
	FollowUpRequired        FollowUpCounts `json:"follow_up_required"`
	Open                    int            `json:"open"`
	Closed                  int            `json:"closed"`
}

// ResponseTime are the first-response KPIs.
type ResponseTime struct {
	Responded      int      `json:"responded"`
	NeverResponded int      `json:"never_responded"`
	MedianMinutes  *float64 `json:"median_minutes"`
	P90Minutes     *float64 `json:"p90_minutes"`
	Within1h       int      `json:"within_1h"`
	Within1hRate   *string  `json:"within_1h_rate"`
	Within24h      int      `json:"within_24h"`
	Within24hRate  *string  `json:"within_24h_rate"`
}

// TimeToConversion is the median time from receipt to conversion.
type TimeToConversion struct {
	Converted  int      `json:"converted"`
	MedianDays *float64 `json:"median_days"`
}

// BreakdownCounts are the core numbers of a breakdown row.
type BreakdownCounts struct {
	Received                   int      `json:"received"`
	Contacted                  int      `json:"contacted"`
	Converted                  int      `json:"converted"`
	ConversionRate             *string  `json:"conversion_rate"`
	MedianFirstResponseMinutes *float64 `json:"median_first_response_minutes"`
	firstResponses             []float64
}

func (b *BreakdownCounts) add(d factOf) {
	b.Received++
	if d.reached(rankContacted) {
		b.Contacted++
	}
	if d.everConverted {
		b.Converted++
	}
	if d.firstResponse != nil {
		b.firstResponses = append(b.firstResponses, d.firstResponse.Minutes())
	}
}

func (b *BreakdownCounts) finish() {
	b.ConversionRate = Rate(b.Converted, b.Received)
	sort.Float64s(b.firstResponses)
	b.MedianFirstResponseMinutes = round(Median(b.firstResponses), 1)
}

// AgentRow is one row of the by-agent breakdown.
type AgentRow struct {
	UserID *uuid.UUID `json:"user_id"`
	Name   string     `json:"name"`
	BreakdownCounts
}

// TypeRow is one row of the by-request-type breakdown.
type TypeRow struct {
	RequestType string `json:"request_type"`
	Label       string `json:"label"`
	BreakdownCounts
}

// ListingRow is one row of the by-listing breakdown.
type ListingRow struct {
	ListingID uuid.UUID     `json:"listing_id"`
	Listing   *InboxListing `json:"listing"`
	BreakdownCounts
}

// Funnel is the computed report (without its window and filters).
type Funnel struct {
	Received         int              `json:"received"`
	Steps            []FunnelStepView `json:"steps"`
	SideStates       SideStates       `json:"side_states"`
	ResponseTime     ResponseTime     `json:"response_time"`
	TimeToConversion TimeToConversion `json:"time_to_conversion"`
	ByAgent          []AgentRow       `json:"by_agent"`
	ByRequestType    []TypeRow        `json:"by_request_type"`
	ByListing        []ListingRow     `json:"by_listing"`
}

// ComputeFunnel counts a report from the facts of its cohort. Agent names
// and listing summaries are filled in by the caller.
func ComputeFunnel(facts []FunnelFact) Funnel {
	out := Funnel{Received: len(facts)}
	steps := funnelSteps()
	reached := make([]int, len(steps))
	current := map[string]int{}
	var responses, conversions []float64

	agents := map[uuid.UUID]*AgentRow{}
	var inbox *AgentRow
	types := map[string]*TypeRow{}
	for _, t := range requestTypes() {
		types[t] = &TypeRow{RequestType: t, Label: RequestTypeLabel(t)}
	}
	listings := map[uuid.UUID]*ListingRow{}

	for _, f := range facts {
		d := derive(f)
		for k := range steps {
			if k == 0 || d.reached(k) {
				reached[k]++
			}
		}
		current[f.Stage]++
		switch {
		case f.Stage == StageClosed && d.everConverted:
			out.SideStates.ConvertedThenClosed++
		case f.Stage == StageClosed:
			out.SideStates.ClosedWithoutConverting++
		}
		if f.Stage == StageFollowUpRequired {
			out.SideStates.FollowUpRequired.Current++
		}
		if d.everFollowUp || f.Stage == StageFollowUpRequired {
			out.SideStates.FollowUpRequired.Ever++
		}
		if f.Stage == StageConverted || f.Stage == StageClosed {
			out.SideStates.Closed++
		} else {
			out.SideStates.Open++
		}
		if d.firstResponse != nil {
			minutes := d.firstResponse.Minutes()
			responses = append(responses, minutes)
			if minutes <= 60 {
				out.ResponseTime.Within1h++
			}
			if minutes <= 24*60 {
				out.ResponseTime.Within24h++
			}
		}
		if d.toConversion != nil {
			conversions = append(conversions, d.toConversion.Hours()/24)
		}

		var row *AgentRow
		if f.HandlerUserID == nil {
			if inbox == nil {
				inbox = &AgentRow{Name: OrganisationInboxName}
			}
			row = inbox
		} else {
			if agents[*f.HandlerUserID] == nil {
				id := *f.HandlerUserID
				agents[id] = &AgentRow{UserID: &id}
			}
			row = agents[*f.HandlerUserID]
		}
		row.add(d)
		if t := types[f.RequestType]; t != nil {
			t.add(d)
		}
		if listings[f.ListingID] == nil {
			listings[f.ListingID] = &ListingRow{ListingID: f.ListingID}
		}
		listings[f.ListingID].add(d)
	}

	for k, s := range steps {
		v := FunnelStepView{Step: s.Step, Label: s.Label, Reached: reached[k], Current: current[s.Stage],
			ConversionFromReceived: Rate(reached[k], out.Received)}
		if k > 0 {
			v.ConversionFromPrevious = Rate(reached[k], reached[k-1])
		}
		out.Steps = append(out.Steps, v)
	}

	sort.Float64s(responses)
	sort.Float64s(conversions)
	rt := &out.ResponseTime
	rt.Responded, rt.NeverResponded = len(responses), out.Received-len(responses)
	rt.MedianMinutes, rt.P90Minutes = round(Median(responses), 1), round(Percentile(responses, 90), 1)
	rt.Within1hRate, rt.Within24hRate = Rate(rt.Within1h, out.Received), Rate(rt.Within24h, out.Received)
	out.TimeToConversion = TimeToConversion{Converted: reached[len(steps)-1], MedianDays: round(Median(conversions), 2)}

	out.ByAgent = []AgentRow{}
	for _, row := range agents {
		row.finish()
		out.ByAgent = append(out.ByAgent, *row)
	}
	if inbox != nil {
		inbox.finish()
		out.ByAgent = append(out.ByAgent, *inbox)
	}
	sortAgents(out.ByAgent)
	out.ByRequestType = []TypeRow{}
	for _, t := range requestTypes() {
		types[t].finish()
		out.ByRequestType = append(out.ByRequestType, *types[t])
	}
	out.ByListing = []ListingRow{}
	for _, row := range listings {
		row.finish()
		out.ByListing = append(out.ByListing, *row)
	}
	sort.Slice(out.ByListing, func(i, j int) bool {
		a, b := out.ByListing[i], out.ByListing[j]
		if a.Received != b.Received {
			return a.Received > b.Received
		}
		return a.ListingID.String() < b.ListingID.String()
	})
	if len(out.ByListing) > TopFunnelListings {
		out.ByListing = out.ByListing[:TopFunnelListings]
	}
	return out
}

// sortAgents orders by received (descending), then name, then user ID (the
// organisation inbox, without one, last among equals).
func sortAgents(rows []AgentRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Received != b.Received {
			return a.Received > b.Received
		}
		if (a.UserID == nil) != (b.UserID == nil) {
			return b.UserID == nil
		}
		if a.Name != b.Name {
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
		return a.UserID != nil && b.UserID != nil && a.UserID.String() < b.UserID.String()
	})
}

// FunnelReport counts the report of the cohort under the filter, with the
// names of the agents (when assignees are configured).
func (s *Service) FunnelReport(ctx context.Context, f FunnelFilter) (Funnel, error) {
	facts, err := s.store.FunnelFacts(ctx, f)
	if err != nil {
		return Funnel{}, fmt.Errorf("load funnel facts: %w", err)
	}
	out := ComputeFunnel(facts)
	if s.assignees != nil {
		for i := range out.ByAgent {
			if id := out.ByAgent[i].UserID; id != nil {
				name, err := s.assignees.Name(ctx, *id)
				if err != nil {
					return Funnel{}, err
				}
				out.ByAgent[i].Name = name
			}
		}
		sortAgents(out.ByAgent)
	}
	return out, nil
}
