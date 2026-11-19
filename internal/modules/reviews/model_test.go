package reviews

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/tenancies"
)

func TestPublicName(t *testing.T) {
	for in, want := range map[string]string{
		"Tariro Tenant":            "Tariro T.",
		"  casey   van der merwe ": "casey M.",
		"Rudo":                     "Rudo",
		"":                         "Customer",
		"Émile ñandú":              "Émile Ñ.",
	} {
		if got := PublicName(in); got != want {
			t.Errorf("PublicName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAverage(t *testing.T) {
	if Average(Rating{}) != nil {
		t.Fatal("no reviews must give a null average")
	}
	for _, tc := range []struct {
		r    Rating
		want string
	}{
		{Rating{Count: 2, Sum: 9}, "4.5"},
		{Rating{Count: 3, Sum: 13}, "4.3"},
		{Rating{Count: 3, Sum: 11}, "3.7"},
		{Rating{Count: 1, Sum: 4}, "4.0"},
		{Rating{Count: 4, Sum: 17}, "4.3"}, // 4.25 rounds half up
		{Rating{Count: 1, Sum: 5}, "5.0"},
	} {
		if got := Average(tc.r); got == nil || *got != tc.want {
			t.Errorf("Average(%+v) = %v, want %s", tc.r, got, tc.want)
		}
	}
}

func TestEligibilityRules(t *testing.T) {
	org, other := uuid.New(), uuid.New()
	book := tenancies.OrganisationBook(org)
	profile, otherProfile := uuid.New(), uuid.New()
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	job := func(b tenancies.Book, provider *uuid.UUID, status string) MaintenanceJob {
		return MaintenanceJob{ID: uuid.New(), Book: b, ProviderProfileID: provider, Status: status}
	}
	for _, tc := range []struct {
		name string
		job  MaintenanceJob
		want string
	}{
		{"closed", job(book, &profile, maintenance.StatusClosed), ""},
		{"resolved", job(book, &profile, maintenance.StatusResolved), ""},
		{"in progress", job(book, &profile, maintenance.StatusInProgress), ReasonNotCompleted},
		{"cancelled", job(book, &profile, maintenance.StatusCancelled), ReasonNotCompleted},
		{"another provider", job(book, &otherProfile, maintenance.StatusClosed), ReasonWrongProvider},
		{"never assigned", job(book, nil, maintenance.StatusClosed), ReasonWrongProvider},
		{"another org's", job(tenancies.OrganisationBook(other), &profile, maintenance.StatusClosed), ReasonBasisNotFound},
		{"a direct owner's", job(tenancies.PersonalBook(org), &profile, maintenance.StatusClosed), ReasonBasisNotFound},
	} {
		if got := maintenanceReason(tc.job, book, profile); got != tc.want {
			t.Errorf("maintenance %s: %q, want %q", tc.name, got, tc.want)
		}
	}

	user, someone := uuid.New(), uuid.New()
	replied := now.Add(-20 * 24 * time.Hour)
	enq := func(customer *uuid.UUID, prof uuid.UUID, age time.Duration, repliedAt *time.Time, version string) businessenquiries.CustomerEnquiry {
		return businessenquiries.CustomerEnquiry{ID: uuid.New(), ProfileID: prof, CustomerUserID: customer, CreatedAt: now.Add(-age),
			RepliedAt: repliedAt, PrivacyVersion: version}
	}
	day := 24 * time.Hour
	for _, tc := range []struct {
		name string
		e    businessenquiries.CustomerEnquiry
		want string
	}{
		{"14 days, replied", enq(&user, profile, 14*day, &replied, "2026-01"), ""},
		{"13 days, replied", enq(&user, profile, 14*day-time.Second, &replied, "2026-01"), ReasonTooRecent},
		{"not replied", enq(&user, profile, 30*day, nil, "2026-01"), ReasonNotReplied},
		{"another provider", enq(&user, otherProfile, 30*day, &replied, "2026-01"), ReasonWrongProvider},
		{"someone else's", enq(&someone, profile, 30*day, &replied, "2026-01"), ReasonBasisNotFound},
		{"a guest's", enq(nil, profile, 30*day, &replied, "2026-01"), ReasonBasisNotFound},
		{"a maintenance assignment", enq(&user, profile, 30*day, &replied, maintenance.DefaultEnquiryPrivacyVer), ReasonBasisNotFound},
	} {
		got, from := enquiryReason(tc.e, user, profile, now)
		if got != tc.want {
			t.Errorf("enquiry %s: %q, want %q", tc.name, got, tc.want)
		}
		if got == ReasonTooRecent && (from == nil || !from.Equal(tc.e.CreatedAt.Add(EnquiryMinAge))) {
			t.Errorf("enquiry %s: eligible from %v", tc.name, from)
		}
	}
}

// The moderation state machine.
func TestAllowed(t *testing.T) {
	reply := &Reply{Text: "Thanks", At: time.Now()}
	for _, tc := range []struct {
		r    Review
		want []string
	}{
		{Review{Status: StatusPending}, []string{ActionPublish, ActionReject}},
		{Review{Status: StatusPublished}, []string{ActionHide}},
		{Review{Status: StatusPublished, OpenReports: 2, Reply: reply}, []string{ActionPublish, ActionHide, ActionRemoveReply}},
		{Review{Status: StatusHidden}, []string{ActionPublish}},
		{Review{Status: StatusRejected}, nil},
	} {
		if got := Allowed(tc.r); !slices.Equal(got, tc.want) {
			t.Errorf("%s (%d reports): %v, want %v", tc.r.Status, tc.r.OpenReports, got, tc.want)
		}
	}
}

// The memory store's guards mirror the Postgres constraints.
func TestMemoryStoreGuards(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	now := time.Now().UTC()
	user, reporter := uuid.New(), uuid.New()
	r := Review{ID: newID(), ProfileID: uuid.New(), ProviderOrganisationID: uuid.New(), ProviderName: "P", ReviewerKind: ReviewerCustomer,
		ReviewerUserID: &user, ReviewerName: "A B.", BasisKind: BasisEnquiry, BasisID: uuid.New(), Rating: 4, Text: "twenty characters long",
		Status: StatusPending, Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.Apply(ctx, Change{Create: &r}); err != nil {
		t.Fatal(err)
	}
	dup := r
	dup.ID = newID()
	if err := s.Apply(ctx, Change{Create: &dup}); !errors.Is(err, ErrBasisReviewed) {
		t.Fatalf("second review of a basis: %v", err)
	}
	report := Report{ID: newID(), ReviewID: r.ID, ReporterUserID: &reporter, Reason: "x", Status: ReportOpen, CreatedAt: now}
	if err := s.Apply(ctx, Change{Reports: []Report{report}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("report of a pending review: %v", err)
	}
	next := r
	next.Status, next.PublishedAt = StatusPublished, &now
	if err := s.Apply(ctx, Change{Update: &next, ExpectVersion: 7}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if err := s.Apply(ctx, Change{Update: &next, ExpectVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, Change{Reports: []Report{report}}); err != nil {
		t.Fatal(err)
	}
	again := report
	again.ID = newID()
	if err := s.Apply(ctx, Change{Reports: []Report{again}}); !errors.Is(err, ErrAlreadyReported) {
		t.Fatalf("second open report: %v", err)
	}
	if got, _ := s.Review(ctx, r.ID); got.Version != 2 || got.OpenReports != 1 {
		t.Fatalf("stored: %+v", got)
	}
	ratings, _ := s.Ratings(ctx, []uuid.UUID{r.ProfileID})
	if ratings[r.ProfileID] != (Rating{Count: 1, Sum: 4}) {
		t.Fatalf("ratings: %+v", ratings)
	}
	s.ForgetUser(user, "Deleted account")
	s.ForgetUser(reporter, "Deleted account")
	got, _ := s.Review(ctx, r.ID)
	reports, _ := s.Reports(ctx, r.ID)
	if got.ReviewerUserID != nil || got.ReviewerName != FormerCustomer || reports[0].ReporterUserID != nil {
		t.Fatalf("after deletion: %+v %+v", got, reports)
	}
}
