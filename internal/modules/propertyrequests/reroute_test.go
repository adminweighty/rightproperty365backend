package propertyrequests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// The Postgres side of rerouting (API 0.49.0): the request becomes RECEIVED
// with its new recipient and stays in the resolved list; the organisation
// with an active agent is a candidate. Rerunnable: every row is the test's own.
func TestPostgresReroute(t *testing.T) {
	pool := openFunnelDatabase(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	var market uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM markets WHERE code = 'ZW'`).Scan(&market); err != nil {
		t.Fatal(err)
	}
	org, agent, listing, id := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO organisations (id, market_id, name, kind, status) VALUES ($1, $2, $3, 'AGENCY', 'ACTIVE')`, org, market, "Reroute Test "+org.String()[:8])
	exec(`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Reroute Agent', 'ACTIVE', now())`, agent, "reroute."+agent.String()[:8]+"@example.test")
	exec(`INSERT INTO memberships (id, user_id, organisation_id, market_id, role, status) VALUES ($1, $2, $3, $4, 'AGENT', 'ACTIVE')`, uuid.New(), agent, org, market)
	exec(`INSERT INTO listings (id, market_id, title, status, price_amount, currency_code, organisation_id) VALUES ($1, $2, 'Reroute listing', 'PUBLISHED', 100, 'USD', $3)`,
		listing, market, org)
	now := time.Now().UTC().Truncate(time.Microsecond)
	store := NewPostgresStore(pool)
	if err := store.SeedRequests(ctx, []SeedRequest{{Request: NewRequest{ID: id, Reference: "RR" + id.String()[:6], MarketID: market, ListingID: listing,
		RequestType: TypeEnquiry, Status: StatusRoutingException, Message: "Who sells this?", PreferredContactMethod: ContactEmail,
		IdempotencyKey: "rr-" + id.String(), RequestHash: "fixture", GuestName: "Guest", GuestEmail: "guest@example.test",
		PrivacyVersion: "2026-01", ConsentAcceptedAt: now, CreatedAt: now}}}); err != nil {
		t.Fatal(err)
	}

	cands, err := identity.NewPostgresStore(pool).RoutableOrganisations(ctx, "ZW")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cands {
		if c.ID == org {
			found = len(c.Members) == 1 && c.Members[0].UserID == agent
		}
	}
	if !found {
		t.Fatalf("the organisation is not a candidate: %+v", cands)
	}

	admin := Actor{UserID: agent, Name: "Admin"}
	rr := Reroute{OrganisationID: org, UserID: &agent, UserName: "Reroute Agent", Actor: admin, Note: "Confirmed", At: now,
		Audit: orgaudit.New(org, agent, "Admin", orgaudit.ActionRequestAssigned, orgaudit.TargetPropertyRequest, id, "RR", nil, nil, now)}
	got, err := store.RerouteException(ctx, id, rr, func(StoredRequest) error { return nil })
	if err != nil || got.Status != StatusReceived || got.Stage != StageAssigned || got.Recipient == nil || got.Recipient.UserID == nil ||
		*got.Recipient.UserID != agent || got.ResolvedAt == nil {
		t.Fatalf("rerouted: %+v %v", got, err)
	}
	resolved := true
	items, _, err := store.ListExceptions(ctx, ExceptionFilter{Resolved: &resolved, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, r := range items {
		listed = listed || r.ID == id
	}
	if !listed {
		t.Fatal("a rerouted exception left the resolved list")
	}
}
