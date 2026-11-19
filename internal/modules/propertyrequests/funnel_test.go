package propertyrequests

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/auth"
)

// funnelPeople are the IDs the funnel fixture refers to. The Postgres test
// creates them as rows; the memory store needs nothing.
type funnelPeople struct {
	org, owner, agentA, agentB, seller uuid.UUID
	listingA, listingB, sellerListing  uuid.UUID
	market                             uuid.UUID
}

func newFunnelPeople() funnelPeople {
	return funnelPeople{
		org: uuid.New(), owner: uuid.New(), agentA: uuid.New(), agentB: uuid.New(), seller: uuid.New(),
		listingA: uuid.New(), listingB: uuid.New(), sellerListing: uuid.New(), market: markets.ZimbabweID,
	}
}

// funnelDay is the first day of the fixture's window (2026-11-01 .. 2026-11-19).
var funnelDay = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// funnelFixture is the known cohort of the tests (minutes are to the first response):
//
//	R1  A ENQUIRY        agent A   CONTACTED 30m, VIEWING_SCHEDULED (skips VR), CONVERTED +5d
//	R2  A VIEWING_REQ    inbox     assigned to B +1h (ASSIGNED), CONTACTED 180m
//	R3  B ENQUIRY        inbox     CLOSED straight from NEW_ENQUIRY (no response)
//	R4  B PROPERTY_INFO  agent A   CONTACTED 600m, VIEWING_REQUESTED, back to CONTACTED, FOLLOW_UP_REQUIRED
//	R5  A ENQUIRY        agent B   untouched (no response; routed to a person: reached ASSIGNED)
//	R6  A ENQUIRY        agent A   CONTACTED 1500m, CONVERTED +3d, CLOSED
//	R7  A ENQUIRY        inbox     2026-10-31 23:59:59.999999 (before the window)
//	R8  A ENQUIRY        inbox     2026-11-19 23:59:59.999999 (last instant inside)
//	R9  A ENQUIRY        inbox     2026-11-20 00:00:00 (after the window)
//	R10 A ENQUIRY        inbox     2026-11-01 00:00:00 (first instant inside)
//	R11 S ENQUIRY        seller    CONTACTED 5m (the direct seller's)
func funnelFixture(p funnelPeople) []SeedRequest {
	var out []SeedRequest
	base := funnelDay.AddDate(0, 0, 9) // 2026-11-10
	owner, a, b := Actor{UserID: p.owner, Name: "Owner"}, Actor{UserID: p.agentA, Name: "Agent A"}, Actor{UserID: p.agentB, Name: "Agent B"}
	add := func(listing uuid.UUID, typ string, recipient Recipient, created time.Time, steps ...HistoryStep) {
		n := len(out) + 1
		id := uuid.NewSHA1(p.org, fmt.Appendf(nil, "funnel-%d", n))
		rec := recipient
		out = append(out, SeedRequest{Request: NewRequest{
			ID: id, Reference: fmt.Sprintf("FN%s%02d", strings.ToUpper(id.String()[:4]), n), MarketID: p.market, ListingID: listing,
			RequestType: typ, Status: StatusReceived, Message: "Funnel fixture request.", PreferredContactMethod: ContactEmail,
			IdempotencyKey: "funnel-" + id.String(), RequestHash: "fixture", GuestName: "Guest", GuestEmail: "guest@example.test",
			PrivacyVersion: "2026-01", ConsentAcceptedAt: created, Recipient: &rec, CreatedAt: created,
		}, Steps: steps})
	}
	org := p.org
	inbox := Recipient{Type: RecipientOrganisation, OrganisationID: &org}
	agent := func(u uuid.UUID) Recipient {
		return Recipient{Type: RecipientAgent, OrganisationID: &org, UserID: &u}
	}
	move := func(actor Actor, stage string, at time.Time) HistoryStep {
		return HistoryStep{Stage: stage, Actor: actor, At: at}
	}
	agentB := p.agentB
	day := 24 * time.Hour

	add(p.listingA, TypeEnquiry, agent(p.agentA), base,
		move(a, StageContacted, base.Add(30*time.Minute)), move(a, StageViewingScheduled, base.Add(2*day)), move(a, StageConverted, base.Add(5*day)))
	add(p.listingA, TypeViewingRequest, inbox, base,
		HistoryStep{AssignTo: &agentB, AssigneeName: "Agent B", Actor: owner, At: base.Add(time.Hour)},
		move(owner, StageAssigned, base.Add(time.Hour)), move(b, StageContacted, base.Add(180*time.Minute)))
	add(p.listingB, TypeEnquiry, inbox, base, move(owner, StageClosed, base.Add(day)))
	add(p.listingB, TypePropertyInformation, agent(p.agentA), base,
		move(a, StageContacted, base.Add(600*time.Minute)), move(a, StageViewingRequested, base.Add(day)),
		move(a, StageContacted, base.Add(2*day)), move(a, StageFollowUpRequired, base.Add(3*day)))
	add(p.listingA, TypeEnquiry, agent(p.agentB), base)
	add(p.listingA, TypeEnquiry, agent(p.agentA), base,
		move(a, StageContacted, base.Add(1500*time.Minute)), move(a, StageConverted, base.Add(3*day)), move(a, StageClosed, base.Add(4*day)))
	add(p.listingA, TypeEnquiry, inbox, funnelDay.Add(-time.Microsecond))
	add(p.listingA, TypeEnquiry, inbox, funnelDay.AddDate(0, 0, 19).Add(-time.Microsecond))
	add(p.listingA, TypeEnquiry, inbox, funnelDay.AddDate(0, 0, 19))
	add(p.listingA, TypeEnquiry, inbox, funnelDay)
	seller := p.seller
	add(p.sellerListing, TypeEnquiry, Recipient{Type: RecipientSeller, UserID: &seller}, base,
		move(Actor{UserID: p.seller, Name: "Seller"}, StageContacted, base.Add(5*time.Minute)))
	return out
}

// funnelWindowFilter is the fixture's window (2026-11-01 .. 2026-11-19).
func funnelWindowFilter(scope InboxScope, org *uuid.UUID) FunnelFilter {
	return FunnelFilter{Scope: scope, OrganisationID: org, From: funnelDay, To: funnelDay.AddDate(0, 0, 19)}
}

func seededMemory(t *testing.T, p funnelPeople) *MemoryStore {
	t.Helper()
	s := NewMemoryStore()
	if err := s.SeedRequests(context.Background(), funnelFixture(p)); err != nil {
		t.Fatal(err)
	}
	return s
}

func reportOf(t *testing.T, s Store, f FunnelFilter) Funnel {
	t.Helper()
	facts, err := s.FunnelFacts(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return ComputeFunnel(facts)
}

func str(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

func num(v *float64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(*v)
}

// checkManagerReport asserts the whole organisation's figures of the fixture.
func checkManagerReport(t *testing.T, got Funnel) {
	t.Helper()
	if got.Received != 8 {
		t.Fatalf("received %d, want 8 (R1-R6, R8, R10)", got.Received)
	}
	want := []struct {
		step               string
		reached, current   int
		fromPrev, fromRecv string
	}{
		{StepReceived, 8, 3, "null", "1.0000"},
		{StageAssigned, 5, 0, "0.6250", "0.6250"},
		{StageContacted, 4, 1, "0.8000", "0.5000"},
		// R1 skipped VIEWING_REQUESTED; R6 went CONTACTED -> CONVERTED.
		{StageViewingRequested, 3, 0, "0.7500", "0.3750"},
		{StageViewingScheduled, 2, 0, "0.6667", "0.2500"},
		{StageConverted, 2, 1, "1.0000", "0.2500"},
	}
	for i, w := range want {
		s := got.Steps[i]
		if s.Step != w.step || s.Reached != w.reached || s.Current != w.current || str(s.ConversionFromPrevious) != w.fromPrev || str(s.ConversionFromReceived) != w.fromRecv {
			t.Fatalf("step %d = %+v (%s, %s), want %+v", i, s, str(s.ConversionFromPrevious), str(s.ConversionFromReceived), w)
		}
	}
	if want := (SideStates{ClosedWithoutConverting: 1, ConvertedThenClosed: 1, FollowUpRequired: FollowUpCounts{1, 1}, Open: 5, Closed: 3}); got.SideStates != want {
		t.Fatalf("side states %+v", got.SideStates)
	}
	rt := got.ResponseTime
	// Responses 30, 180, 600 and 1500 minutes: median (180+600)/2, p90 the 4th of 4.
	if rt.Responded != 4 || rt.NeverResponded != 4 || num(rt.MedianMinutes) != "390" || num(rt.P90Minutes) != "1500" ||
		rt.Within1h != 1 || str(rt.Within1hRate) != "0.1250" || rt.Within24h != 3 || str(rt.Within24hRate) != "0.3750" {
		t.Fatalf("response time %+v median %s p90 %s", rt, num(rt.MedianMinutes), num(rt.P90Minutes))
	}
	if got.TimeToConversion.Converted != 2 || num(got.TimeToConversion.MedianDays) != "4" {
		t.Fatalf("time to conversion %+v %s", got.TimeToConversion, num(got.TimeToConversion.MedianDays))
	}
	if len(got.ByAgent) != 3 || got.ByAgent[0].Received != 3 || got.ByAgent[0].Converted != 2 || str(got.ByAgent[0].ConversionRate) != "0.6667" ||
		num(got.ByAgent[0].MedianFirstResponseMinutes) != "600" {
		t.Fatalf("by agent %+v", got.ByAgent)
	}
	// Agent B (R2, R5) and the inbox (R3, R8, R10).
	if last := got.ByAgent[2]; last.UserID != nil && got.ByAgent[1].UserID != nil {
		t.Fatalf("no inbox row: %+v", got.ByAgent)
	}
	if len(got.ByRequestType) != 5 || got.ByRequestType[0].RequestType != TypeEnquiry || got.ByRequestType[0].Received != 6 ||
		got.ByRequestType[4].Received != 0 || got.ByRequestType[4].ConversionRate != nil {
		t.Fatalf("by type %+v", got.ByRequestType)
	}
	if len(got.ByListing) != 2 || got.ByListing[0].Received != 6 || got.ByListing[1].Received != 2 || got.ByListing[1].Contacted != 1 {
		t.Fatalf("by listing %+v", got.ByListing)
	}
}

func TestFunnelCountsWithSkippedStages(t *testing.T) {
	p := newFunnelPeople()
	s := seededMemory(t, p)
	manager := InboxScope{UserID: p.owner, ManagedOrganisationIDs: []uuid.UUID{p.org}}
	checkManagerReport(t, reportOf(t, s, funnelWindowFilter(manager, &p.org)))

	// One request, CONTACTED straight to VIEWING_SCHEDULED: it reached every step up to it.
	created := funnelDay
	got := ComputeFunnel([]FunnelFact{{RequestID: uuid.New(), Stage: StageViewingScheduled, CreatedAt: created, Moves: []FunnelMove{
		{To: StageContacted, ActorType: ActorProfessional, At: created.Add(time.Minute)},
		{To: StageViewingScheduled, ActorType: ActorProfessional, At: created.Add(time.Hour)},
	}}})
	for i, want := range []int{1, 1, 1, 1, 1, 0} {
		if got.Steps[i].Reached != want {
			t.Fatalf("step %s reached %d, want %d", got.Steps[i].Step, got.Steps[i].Reached, want)
		}
	}
	// Closed from NEW_ENQUIRY, inbox: only received.
	got = ComputeFunnel([]FunnelFact{{RequestID: uuid.New(), Stage: StageClosed, CreatedAt: created, RecipientType: RecipientOrganisation,
		Moves: []FunnelMove{{To: StageClosed, ActorType: ActorProfessional, At: created.Add(time.Minute)}}}})
	if got.Steps[0].Reached != 1 || got.Steps[1].Reached != 0 || got.ResponseTime.NeverResponded != 1 || got.SideStates.ClosedWithoutConverting != 1 {
		t.Fatalf("closed straight away: %+v", got)
	}
}

func TestFunnelRatesWithZeroDenominators(t *testing.T) {
	for _, tt := range []struct {
		n, d int
		want string
	}{{0, 0, "null"}, {3, 0, "null"}, {0, 4, "0.0000"}, {1, 3, "0.3333"}, {2, 3, "0.6667"}, {1, 8, "0.1250"}, {5, 5, "1.0000"}, {1, 20000, "0.0001"}, {1, 30000, "0.0000"}} {
		if got := str(Rate(tt.n, tt.d)); got != tt.want {
			t.Fatalf("Rate(%d, %d) = %s, want %s", tt.n, tt.d, got, tt.want)
		}
	}
	// An empty cohort: every rate and duration is null.
	empty := ComputeFunnel(nil)
	for _, s := range empty.Steps {
		if s.Reached != 0 || s.ConversionFromPrevious != nil || s.ConversionFromReceived != nil {
			t.Fatalf("empty step %+v", s)
		}
	}
	if rt := empty.ResponseTime; rt.MedianMinutes != nil || rt.P90Minutes != nil || rt.Within1hRate != nil || rt.Within24hRate != nil ||
		empty.TimeToConversion.MedianDays != nil || len(empty.ByAgent) != 0 || len(empty.ByListing) != 0 || len(empty.ByRequestType) != 5 {
		t.Fatalf("empty report %+v", empty)
	}
	// R3 alone (listing B, ENQUIRY): assigned 0 of 1 is "0.0000", contacted 0 of 0 is null.
	p := newFunnelPeople()
	f := funnelWindowFilter(InboxScope{UserID: p.owner, ManagedOrganisationIDs: []uuid.UUID{p.org}}, &p.org)
	f.ListingID, f.RequestType = &p.listingB, TypeEnquiry
	got := reportOf(t, seededMemory(t, p), f)
	if got.Received != 1 || str(got.Steps[1].ConversionFromPrevious) != "0.0000" || got.Steps[2].ConversionFromPrevious != nil ||
		str(got.Steps[2].ConversionFromReceived) != "0.0000" {
		t.Fatalf("R3 alone: %+v", got.Steps)
	}
}

func TestFunnelMedianAndP90(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if num(Median(values)) != "5.5" || num(Percentile(values, 90)) != "9" {
		t.Fatalf("1..10: median %s p90 %s", num(Median(values)), num(Percentile(values, 90)))
	}
	if num(Median([]float64{7})) != "7" || num(Percentile([]float64{7}, 90)) != "7" || Median(nil) != nil || Percentile(nil, 90) != nil {
		t.Fatal("single and empty")
	}
	// Eleven values: p90 is the 10th (ceil 9.9).
	eleven := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	if num(Median(eleven)) != "6" || num(Percentile(eleven, 90)) != "10" {
		t.Fatal("1..11")
	}
}

func TestFunnelWindowIsUTCDays(t *testing.T) {
	p := newFunnelPeople()
	s := seededMemory(t, p)
	manager := InboxScope{UserID: p.owner, ManagedOrganisationIDs: []uuid.UUID{p.org}}
	// The fixture's own window keeps R10 (00:00:00 on the first day) and R8
	// (the last microsecond of the last day) and drops R7 and R9.
	if got := reportOf(t, s, funnelWindowFilter(manager, &p.org)); got.Received != 8 {
		t.Fatalf("window received %d", got.Received)
	}
	// One day: 2026-11-20 holds only R9.
	day := funnelDay.AddDate(0, 0, 19)
	if got := reportOf(t, s, FunnelFilter{Scope: manager, OrganisationID: &p.org, From: day, To: day.AddDate(0, 0, 1)}); got.Received != 1 {
		t.Fatalf("2026-11-20 received %d", got.Received)
	}
	// Through the handler: from/to are UTC dates, inclusive.
	h, _ := funnelHandler(t, s, p, day.Add(10*time.Hour))
	body := getFunnel[funnelResponse](t, h, managerPrincipal(p), "/v1/agent/organisations/"+p.org.String()+"/reports/lead-funnel?from=2026-11-01&to=2026-11-19", http.StatusOK)
	if body.Received != 8 || body.Window.Days != 19 || body.Window.From != "2026-11-01" || body.Window.To != "2026-11-19" {
		t.Fatalf("handler window %+v received %d", body.Window, body.Received)
	}
	// The default is the last 30 days to today (2026-11-20): R9 to R10 and the rest.
	body = getFunnel[funnelResponse](t, h, managerPrincipal(p), "/v1/agent/organisations/"+p.org.String()+"/reports/lead-funnel", http.StatusOK)
	if body.Window.From != "2026-10-22" || body.Window.To != "2026-11-20" || body.Window.Days != 30 || body.Received != 10 {
		t.Fatalf("default window %+v received %d", body.Window, body.Received)
	}
	for _, q := range []string{"from=2026-11-02&to=2026-11-01", "from=2025-01-01&to=2026-01-02", "from=11/01/2026", "request_type=NOPE",
		"agent_user_id=x", "market_code=QQ", "format=xml"} {
		getFunnel[map[string]any](t, h, managerPrincipal(p), "/v1/agent/organisations/"+p.org.String()+"/reports/lead-funnel?"+q, http.StatusBadRequest)
	}
	// 366 days is allowed.
	getFunnel[map[string]any](t, h, managerPrincipal(p), "/v1/agent/organisations/"+p.org.String()+"/reports/lead-funnel?from=2025-11-20&to=2026-11-20", http.StatusOK)
}

type fakeAssignees map[uuid.UUID]string

func (f fakeAssignees) Assignable(context.Context, uuid.UUID, uuid.UUID) (string, bool, error) {
	return "", false, nil
}
func (f fakeAssignees) Name(_ context.Context, id uuid.UUID) (string, error) { return f[id], nil }

func funnelHandler(t *testing.T, s *MemoryStore, p funnelPeople, now time.Time) (http.Handler, *Handler) {
	t.Helper()
	svc := NewService(s, WithClock(func() time.Time { return now }),
		WithAssignees(fakeAssignees{p.agentA: "Agent A", p.agentB: "Agent B", p.seller: "Seller"}))
	h := NewHandler(svc, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	h.SetMarkets(func(_ context.Context, codes []string) ([]uuid.UUID, error) {
		if len(codes) == 1 && codes[0] == "ZW" {
			return []uuid.UUID{markets.ZimbabweID}, nil
		}
		return []uuid.UUID{}, nil
	})
	r := chi.NewRouter()
	r.Get("/v1/agent/organisations/{id}/reports/lead-funnel", h.LeadFunnel)
	r.Get("/v1/me/reports/lead-funnel", h.MyLeadFunnel)
	return r, h
}

func managerPrincipal(p funnelPeople) *auth.Principal {
	return &auth.Principal{UserID: p.owner, Memberships: []auth.Membership{{OrganisationID: p.org, Role: auth.RoleManager}}}
}

func getRaw(t *testing.T, h http.Handler, who *auth.Principal, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if who != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), who))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getFunnel[T any](t *testing.T, h http.Handler, who *auth.Principal, path string, status int) T {
	t.Helper()
	rec := getRaw(t, h, who, path)
	if rec.Code != status {
		t.Fatalf("GET %s: %d %s, want %d", path, rec.Code, rec.Body.String(), status)
	}
	return decode[T](t, rec)
}

func TestFunnelScopes(t *testing.T) {
	p := newFunnelPeople()
	s := seededMemory(t, p)
	h, _ := funnelHandler(t, s, p, funnelDay.AddDate(0, 0, 19).Add(time.Hour))
	path := "/v1/agent/organisations/" + p.org.String() + "/reports/lead-funnel?from=2026-11-01&to=2026-11-19"

	manager := getFunnel[funnelResponse](t, h, managerPrincipal(p), path, http.StatusOK)
	checkManagerReport(t, manager.Funnel)
	if manager.Scope != FunnelScopeOrganisation || manager.OrganisationID == nil || *manager.OrganisationID != p.org ||
		manager.ByAgent[0].Name != "Agent A" {
		t.Fatalf("manager %+v", manager)
	}

	// Agent A sees what their inbox shows: R1, R4, R6 and the inbox R3, R8, R10.
	agentA := &auth.Principal{UserID: p.agentA, Memberships: []auth.Membership{{OrganisationID: p.org, Role: auth.RoleAgent}}}
	agent := getFunnel[funnelResponse](t, h, agentA, path, http.StatusOK)
	if agent.Scope != FunnelScopeMember || agent.Received != 6 || agent.Steps[5].Reached != 2 || len(agent.ByAgent) != 2 {
		t.Fatalf("agent %+v", agent)
	}
	// Narrowing to someone else gives nothing more than the scope.
	if other := getFunnel[funnelResponse](t, h, agentA, path+"&agent_user_id="+p.agentB.String(), http.StatusOK); other.Received != 0 {
		t.Fatalf("agent A filtered to agent B: %d", other.Received)
	}
	own := getFunnel[funnelResponse](t, h, agentA, path+"&agent_user_id="+p.agentA.String(), http.StatusOK)
	if own.Received != 3 || own.Filters.AgentUserID == nil {
		t.Fatalf("agent A own: %d", own.Received)
	}
	// STAFF: the inbox's scoping too.
	staff := &auth.Principal{UserID: uuid.New(), Memberships: []auth.Membership{{OrganisationID: p.org, Role: auth.RoleStaff}}}
	if got := getFunnel[funnelResponse](t, h, staff, path, http.StatusOK); got.Received != 3 {
		t.Fatalf("staff %d (the inbox only)", got.Received)
	}
	// Filters.
	if got := getFunnel[funnelResponse](t, h, managerPrincipal(p), path+"&market_code=zw&request_type=PROPERTY_INFORMATION", http.StatusOK); got.Received != 1 ||
		got.Filters.MarketCode == nil || *got.Filters.MarketCode != "ZW" {
		t.Fatalf("filters %+v", got)
	}

	// The direct seller's own report: R11 only.
	sellerP := &auth.Principal{UserID: p.seller}
	s.PutListing(Listing{ID: p.sellerListing, MarketID: p.market, Status: ListingStatusPublished, SellerUserID: &p.seller, SellerEligible: true})
	mine := getFunnel[funnelResponse](t, h, sellerP, "/v1/me/reports/lead-funnel?from=2026-11-01&to=2026-11-19", http.StatusOK)
	if mine.Scope != FunnelScopeDirectSeller || mine.OrganisationID != nil || mine.Received != 1 || mine.ResponseTime.Within1h != 1 ||
		len(mine.ByAgent) != 1 || mine.ByAgent[0].Name != "Seller" {
		t.Fatalf("seller %+v", mine)
	}
	// The manager's own /v1/me report has none of the organisation's requests.
	if got := getFunnel[funnelResponse](t, h, managerPrincipal(p), "/v1/me/reports/lead-funnel?from=2026-11-01&to=2026-11-19", http.StatusOK); got.Received != 0 {
		t.Fatalf("manager /me %d", got.Received)
	}
	// Somebody with no inbox at all: 403; anonymous: 401.
	getFunnel[map[string]any](t, h, &auth.Principal{UserID: uuid.New()}, "/v1/me/reports/lead-funnel", http.StatusForbidden)
	getFunnel[map[string]any](t, h, nil, "/v1/me/reports/lead-funnel", http.StatusUnauthorized)
}

func TestFunnelOtherOrganisationIs404(t *testing.T) {
	p := newFunnelPeople()
	h, _ := funnelHandler(t, seededMemory(t, p), p, funnelDay.AddDate(0, 0, 19))
	other := uuid.New()
	outsider := &auth.Principal{UserID: uuid.New(), Memberships: []auth.Membership{{OrganisationID: other, Role: auth.RoleOwner}}}
	for _, path := range []string{
		"/v1/agent/organisations/" + p.org.String() + "/reports/lead-funnel",
		"/v1/agent/organisations/" + uuid.NewString() + "/reports/lead-funnel",
		"/v1/agent/organisations/not-a-uuid/reports/lead-funnel",
	} {
		getFunnel[map[string]any](t, h, outsider, path, http.StatusNotFound)
		if rec := getRaw(t, h, outsider, path); !strings.Contains(rec.Body.String(), CodeOrganisationNotFound) {
			t.Fatalf("404 body %s", rec.Body.String())
		}
	}
	// Its own organisation, with nothing in it: 200 and zeros.
	got := getFunnel[funnelResponse](t, h, outsider, "/v1/agent/organisations/"+other.String()+"/reports/lead-funnel", http.StatusOK)
	if got.Received != 0 {
		t.Fatalf("other organisation's report %d", got.Received)
	}
	getFunnel[map[string]any](t, h, nil, "/v1/agent/organisations/"+p.org.String()+"/reports/lead-funnel", http.StatusUnauthorized)
}

func TestFunnelCSV(t *testing.T) {
	p := newFunnelPeople()
	s := seededMemory(t, p)
	h, handler := funnelHandler(t, s, p, funnelDay.AddDate(0, 0, 19))
	handler.SetListingNames(namesFunc(func(ids []uuid.UUID) map[uuid.UUID]InboxListing {
		return map[uuid.UUID]InboxListing{p.listingA: {Slug: "a", Title: "=HYPERLINK(1)", Reference: "RA"}}
	}))
	rec := getRaw(t, h, managerPrincipal(p), "/v1/agent/organisations/"+p.org.String()+"/reports/lead-funnel?from=2026-11-01&to=2026-11-19&format=csv")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="lead-funnel-2026-11-01-to-2026-11-19.csv"` ||
		rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("csv %d %v", rec.Code, rec.Header())
	}
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows[0], FunnelCSVHeader()) || len(rows) != 1+3+5+2 {
		t.Fatalf("csv rows %v", rows)
	}
	if want := []string{"agent", p.agentA.String(), "Agent A", "3", "3", "2", "0.6667", "600"}; !reflect.DeepEqual(rows[1], want) {
		t.Fatalf("agent row %v, want %v", rows[1], want)
	}
	// The inbox ties with agent A on received and comes after the named person.
	if want := []string{"agent", "", OrganisationInboxName, "3", "0", "0", "0.0000", ""}; !reflect.DeepEqual(rows[2], want) {
		t.Fatalf("inbox row %v", rows[2])
	}
	if want := []string{"agent", p.agentB.String(), "Agent B", "2", "1", "0", "0.0000", "180"}; !reflect.DeepEqual(rows[3], want) {
		t.Fatalf("agent B row %v", rows[3])
	}
	if want := []string{"request_type", TypeStayRequest, "Request to stay", "0", "0", "0", "", ""}; !reflect.DeepEqual(rows[8], want) {
		t.Fatalf("stay row %v", rows[8])
	}
	if rows[9][0] != "listing" || rows[9][1] != p.listingA.String() || rows[9][2] != "'=HYPERLINK(1)" || rows[10][2] != "" {
		t.Fatalf("listing rows %v", rows[9:])
	}
}

type namesFunc func([]uuid.UUID) map[uuid.UUID]InboxListing

func (f namesFunc) ListingNames(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]InboxListing, error) {
	return f(ids), nil
}

func TestSeedRequestsIsRerunnable(t *testing.T) {
	p := newFunnelPeople()
	s := seededMemory(t, p)
	if err := s.SeedRequests(context.Background(), funnelFixture(p)); err != nil {
		t.Fatal(err)
	}
	if n := len(s.Requests()); n != 11 {
		t.Fatalf("%d requests after a second run", n)
	}
	manager := InboxScope{UserID: p.owner, ManagedOrganisationIDs: []uuid.UUID{p.org}}
	checkManagerReport(t, reportOf(t, s, funnelWindowFilter(manager, &p.org)))
}
