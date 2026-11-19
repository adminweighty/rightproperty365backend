package packages

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

func TestUsageAndGrace(t *testing.T) {
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	market := MarketView{Code: "ZW", Name: "Zimbabwe", Currency: "USD", MinorUnit: 2}
	starter := Package{ID: uuid.New(), Code: "STARTER", Name: "Starter", ListingLimit: 2, Active: true, Default: true}
	r := Resolved{Basis: BasisDefault, Package: &starter}
	day := func(n int) time.Time { return now.AddDate(0, 0, n) }

	// Within the limit.
	u := usageOf(market, r, []time.Time{day(5)})
	if *u.Limit != 2 || u.Used != 1 || *u.Remaining != 1 || u.AtLimit || u.OverLimit || u.GraceUntil != nil ||
		Warning(&u, false, false) != nil {
		t.Fatalf("within: %+v", u)
	}
	// At the limit: a warning for a listing that is not live, none for a live one.
	u = usageOf(market, r, []time.Time{day(5), day(9)})
	if !u.AtLimit || u.OverLimit || *u.Remaining != 0 || Warning(&u, true, false) != nil {
		t.Fatalf("at: %+v", u)
	}
	if w := Warning(&u, false, false); w == nil || !strings.Contains(*w, "Starter package allows 2 active listings in Zimbabwe") {
		t.Fatalf("advertiser warning: %v", w)
	}
	if w := Warning(&u, false, true); w == nil || !strings.Contains(*w, "Approving this listing will be refused") {
		t.Fatalf("reviewer warning: %v", w)
	}
	// Over it (a downgrade): back within the limit when the over_by-th
	// earliest listing expires.
	u = usageOf(market, r, []time.Time{day(3), day(5), day(9), day(20)})
	if !u.OverLimit || u.OverBy != 2 || u.GraceUntil == nil || !u.GraceUntil.Equal(day(5)) || *u.Remaining != 0 ||
		!strings.Contains(u.Message, "2026-11-24") {
		t.Fatalf("over: %+v", u)
	}
	// No package: no limit.
	u = usageOf(market, Resolved{Basis: BasisNone}, []time.Time{day(1)})
	if u.Limit != nil || u.Remaining != nil || u.AtLimit || u.Basis != BasisNone || Warning(&u, false, false) != nil {
		t.Fatalf("none: %+v", u)
	}
	// A limit of 0 is always at the limit.
	zero := starter
	zero.ListingLimit = 0
	if u = usageOf(market, Resolved{Basis: BasisAssigned, Package: &zero}, nil); !u.AtLimit || u.OverLimit {
		t.Fatalf("zero: %+v", u)
	}
}

func TestAssignmentStatus(t *testing.T) {
	today := Day(time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC))
	end := today.AddDate(0, 0, -1)
	cancelled := today
	for _, c := range []struct {
		a    Assignment
		want string
	}{
		{Assignment{StartsOn: today}, AssignmentCurrent},
		{Assignment{StartsOn: today.AddDate(0, 0, -5), EndsOn: &today}, AssignmentCurrent},
		{Assignment{StartsOn: today.AddDate(0, 0, 1)}, AssignmentScheduled},
		{Assignment{StartsOn: today.AddDate(0, 0, -5), EndsOn: &end}, AssignmentEnded},
		{Assignment{StartsOn: today, CancelledAt: &cancelled}, AssignmentCancelled},
	} {
		if got := c.a.Status(today); got != c.want {
			t.Fatalf("%+v: %s, want %s", c.a, got, c.want)
		}
		if c.a.InForce(today) != (c.want == AssignmentCurrent) {
			t.Fatalf("%+v: in force %v", c.a, c.a.InForce(today))
		}
	}
	if acct, ok := AccountOf(nil, nil); ok || acct.Kind != "" {
		t.Fatal("a listing without an owner has no account")
	}
	org, user := uuid.New(), uuid.New()
	if acct, _ := AccountOf(&org, &user); acct.Kind != KindOrganisation || acct.ID != org {
		t.Fatalf("organisation first: %+v", acct)
	}
	if acct, _ := AccountOf(nil, &user); acct.Kind != KindDirectOwner || acct.ID != user {
		t.Fatalf("direct owner: %+v", acct)
	}
}

// TestMemoryStoreReplacement checks the memory twin of the Postgres
// replacement rule (the Postgres one is TestPostgresStoreContract).
func TestMemoryStoreReplacement(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(nil)
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	today := Day(now)
	a, b := Package{ID: uuid.New(), MarketCode: "ZW", Code: "A", Name: "A", ListingLimit: 1, Active: true, Default: true},
		Package{ID: uuid.New(), MarketCode: "ZW", Code: "B", Name: "B", ListingLimit: 2, Active: true}
	s.PutPackage(a)
	if err := s.CreatePackage(ctx, b, adminaudit.Event{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePackage(ctx, Package{ID: uuid.New(), MarketCode: "ZW", Code: "b"}, adminaudit.Event{}); !errors.Is(err, ErrCodeTaken) {
		t.Fatalf("duplicate code: %v", err)
	}
	acct := Account{Kind: KindOrganisation, ID: uuid.New()}
	noAudit := func([]Assignment) adminaudit.Event { return adminaudit.Event{} }
	started := Assignment{ID: uuid.New(), Account: acct, MarketCode: "ZW", PackageID: b.ID, StartsOn: today.AddDate(0, 0, -3), Source: SourceAdmin}
	s.PutAssignment(started)
	res, err := s.Assign(ctx, Assignment{ID: uuid.New(), Account: acct, MarketCode: "ZW", PackageID: a.ID, StartsOn: today.AddDate(0, 0, 2),
		Source: SourceAdmin, CreatedAt: now}, noAudit)
	if err != nil || len(res.Replaced) != 1 {
		t.Fatalf("assign: %+v %v", res, err)
	}
	r := s.ResolveNow(acct, "ZW", now)
	if r.Basis != BasisAssigned || r.Package.ID != b.ID || r.Assignment.EndsOn == nil || !r.Assignment.EndsOn.Equal(today.AddDate(0, 0, 1)) ||
		r.Scheduled == nil || r.ScheduledPackage.ID != a.ID {
		t.Fatalf("resolve: %+v", r)
	}
	if err := s.EndAssignments(ctx, acct, "ZW", now, nil, now, noAudit); err != nil {
		t.Fatal(err)
	}
	if r := s.ResolveNow(acct, "ZW", now); r.Basis != BasisDefault || r.Package.ID != a.ID || r.Scheduled != nil {
		t.Fatalf("after end: %+v", r)
	}
	if err := s.EndAssignments(ctx, acct, "ZW", now, nil, now, noAudit); !errors.Is(err, ErrAssignmentNotFound) {
		t.Fatalf("end again: %v", err)
	}
}
