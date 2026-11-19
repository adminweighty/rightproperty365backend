package devseed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

// LeadFunnelRequestCount is the number of sample property requests.
const LeadFunnelRequestCount = 24

// LeadFunnelRequestID returns the ID of sample request n (1..LeadFunnelRequestCount).
func LeadFunnelRequestID(n int) uuid.UUID { return id(fmt.Sprintf("e7%02x", n)) }

// funnelStep is one step of a sample request's history, after its creation.
type funnelStep struct {
	after  time.Duration
	stage  string     // a STAGE_CHANGED to this stage, or
	assign *uuid.UUID // an assignment (with the move to ASSIGNED)
}

// LeadFunnelRequests returns the sample property requests behind the lead
// funnel report (API 0.38.0), spread over the 28 days before now, with their
// stage histories. Sample Estates (Tendai Ncube: listings 1, 2, 5; Chipo
// Dube: 7, 10, 17; the rest through the organisation inbox, assigned by its
// owner Rudo Moyo) has 21 of them; Grace Mhlanga, the direct seller of
// listing 6, has 3. Among Sample Estates' requests: 4 converted (one later
// closed), 6 closed without converting, 2 at follow-up (3 ever), 7 never answered
// (one closed unanswered), and first responses from 15 minutes to 2 days.
func LeadFunnelRequests(now time.Time) []propertyrequests.SeedRequest {
	now = now.UTC().Truncate(time.Microsecond)
	routing := map[uuid.UUID]propertyrequests.Listing{}
	for _, l := range Listings() {
		routing[l.ID] = l
	}
	tendai, chipo := identity.SeedAgentUserID, EstatesAgent2ID
	names := map[uuid.UUID]string{tendai: "Tendai Ncube", chipo: "Chipo Dube", EstatesOwnerID: "Rudo Moyo", SellerID: "Grace Mhlanga"}
	const (
		inbox  = "INBOX"
		agent  = "AGENT"
		seller = "SELLER"
	)
	m, h, d := time.Minute, time.Hour, 24*time.Hour
	type sample struct {
		listing int
		typ     string
		route   string
		daysAgo int
		hour    int
		steps   []funnelStep
	}
	st := func(after time.Duration, stage string) funnelStep { return funnelStep{after: after, stage: stage} }
	to := func(after time.Duration, who uuid.UUID) funnelStep { return funnelStep{after: after, assign: &who} }
	const (
		enq  = propertyrequests.TypeEnquiry
		info = propertyrequests.TypePropertyInformation
		view = propertyrequests.TypeViewingRequest
		cons = propertyrequests.TypeConsultationRequest
		con  = propertyrequests.StageContacted
		vr   = propertyrequests.StageViewingRequested
		vs   = propertyrequests.StageViewingScheduled
		fu   = propertyrequests.StageFollowUpRequired
		won  = propertyrequests.StageConverted
		shut = propertyrequests.StageClosed
	)
	samples := []sample{
		{1, enq, agent, 28, 8, []funnelStep{st(20*m, con), st(d, vr), st(2*d, vs), st(9*d, won)}},
		{2, view, agent, 27, 9, []funnelStep{st(45*m, con), st(d, vs), st(6*d, won), st(12*d, shut)}},
		{7, enq, agent, 26, 10, []funnelStep{st(3*h, con), st(3*d, fu), st(10*d, shut)}},
		{1, info, inbox, 25, 11, []funnelStep{to(2*h, tendai), st(5*h, con), st(4*d, shut)}},
		{10, enq, agent, 24, 12, []funnelStep{st(d, shut)}},
		{5, view, agent, 22, 7, []funnelStep{st(30*m, con), st(2*h, vr), st(d, vs), st(5*d, won)}},
		{17, cons, agent, 21, 13, []funnelStep{st(26*h, con), st(7*d, shut)}},
		{2, enq, inbox, 20, 14, []funnelStep{to(d, chipo), st(2*d, con), st(4*d, vs), st(8*d, fu)}},
		{1, enq, agent, 18, 9, nil},
		{7, view, agent, 17, 10, []funnelStep{st(15*m, con), st(6*h, vs), st(4*d, won)}},
		{5, info, agent, 15, 11, []funnelStep{st(50*h, con), st(5*d, shut)}},
		{10, enq, inbox, 14, 15, nil},
		{1, view, agent, 12, 8, []funnelStep{st(40*m, con), st(h, vr), st(3*d, fu)}},
		{17, enq, agent, 10, 16, []funnelStep{st(8*h, con), st(d, vr), st(2*d, vs)}},
		{2, enq, inbox, 9, 12, []funnelStep{to(30*m, tendai), st(2*d, shut)}},
		{7, enq, agent, 7, 9, []funnelStep{st(2*h, con)}},
		{5, enq, agent, 6, 10, nil},
		{10, view, inbox, 5, 7, []funnelStep{to(4*h, chipo), st(20*h, con), st(2*d, vs)}},
		{1, enq, agent, 3, 13, []funnelStep{st(55*m, con)}},
		{17, enq, agent, 2, 11, nil},
		{2, info, inbox, 1, 6, nil},
		{6, enq, seller, 20, 10, []funnelStep{st(90*m, con), st(2*d, vs), st(6*d, won)}},
		{6, view, seller, 8, 14, []funnelStep{st(4*h, con)}},
		{6, enq, seller, 2, 9, nil},
	}
	guests := []string{"Tariro Moyo", "Kuda Banda", "Nyasha Phiri", "Rufaro Gumbo", "Tafadzwa Shumba", "Vimbai Zulu"}

	out := make([]propertyrequests.SeedRequest, 0, len(samples))
	for i, s := range samples {
		n := i + 1
		l := routing[propertyrequests.SeedListingID(s.listing)]
		day := now.AddDate(0, 0, -s.daysAgo)
		created := time.Date(day.Year(), day.Month(), day.Day(), s.hour, 0, 0, 0, time.UTC)
		var recipient propertyrequests.Recipient
		var handler uuid.UUID
		switch s.route {
		case seller:
			handler = *l.SellerUserID
			recipient = propertyrequests.Recipient{Type: propertyrequests.RecipientSeller, UserID: &handler}
		case agent:
			handler = *l.AgentUserID
			recipient = propertyrequests.Recipient{Type: propertyrequests.RecipientAgent, UserID: &handler, OrganisationID: l.OrganisationID}
		default:
			recipient = propertyrequests.Recipient{Type: propertyrequests.RecipientOrganisation, OrganisationID: l.OrganisationID}
		}
		guest := guests[i%len(guests)]
		rid := LeadFunnelRequestID(n)
		seed := propertyrequests.SeedRequest{Request: propertyrequests.NewRequest{
			ID: rid, Reference: fmt.Sprintf("LFDEMO%02d", n), MarketID: l.MarketID, ListingID: l.ID,
			RequestType: s.typ, Status: propertyrequests.StatusReceived,
			Message:                "I am interested in this property. Please get in touch.",
			PreferredContactMethod: propertyrequests.ContactEmail,
			IdempotencyKey:         "devseed-lead-funnel-" + rid.String(), RequestHash: "devseed",
			GuestName: guest, GuestEmail: fmt.Sprintf("lead%02d@example.test", n),
			PrivacyVersion: privacyVersion, ConsentAcceptedAt: created, Recipient: &recipient, CreatedAt: created,
		}}
		for _, step := range s.steps {
			at := created.Add(step.after)
			if step.assign != nil {
				owner := propertyrequests.Actor{UserID: EstatesOwnerID, Name: names[EstatesOwnerID]}
				handler = *step.assign
				seed.Steps = append(seed.Steps,
					propertyrequests.HistoryStep{AssignTo: step.assign, AssigneeName: names[*step.assign], Actor: owner, At: at},
					propertyrequests.HistoryStep{Stage: propertyrequests.StageAssigned, Actor: owner, At: at})
				continue
			}
			seed.Steps = append(seed.Steps, propertyrequests.HistoryStep{
				Stage: step.stage, Actor: propertyrequests.Actor{UserID: handler, Name: names[handler]}, At: at})
		}
		out = append(out, seed)
	}
	return out
}

// SeedMemoryLeadFunnel stores the sample property requests in memory.
func SeedMemoryLeadFunnel(ctx context.Context, store *propertyrequests.MemoryStore, now time.Time) error {
	return store.SeedRequests(ctx, LeadFunnelRequests(now))
}

// seedPostgresLeadFunnel replaces the sample property requests (their IDs
// are fixed, so a run deletes the previous copies and writes them again
// with fresh dates).
func seedPostgresLeadFunnel(ctx context.Context, tx pgx.Tx, now time.Time) error {
	return propertyrequests.SeedRequestsTx(ctx, tx, LeadFunnelRequests(now))
}
