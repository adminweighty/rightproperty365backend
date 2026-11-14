package listings

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

type noLabels struct{}

func (noLabels) Labels(context.Context, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (noLabels) ActiveCodes(context.Context, string) ([]string, error) { return nil, nil }

// staleIndex is a search engine that is behind the source of truth: it still
// returns listings that have since expired, been unpublished or deleted.
type staleIndex struct{ ids []uuid.UUID }

func (s staleIndex) Search(context.Context, Query, time.Time) (Hits, error) {
	return Hits{IDs: s.ids, Total: len(s.ids)}, nil
}
func (s staleIndex) Suggest(context.Context, string, int, time.Time) ([]Suggestion, error) {
	return nil, nil
}

// Whatever a search adapter returns is re-read from the Store, which checks
// status and expiry: a stale index cannot bring back a listing that is no
// longer public.
func TestSearchResultsAreRecheckedAgainstTheSourceOfTruth(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	store := NewMemoryStore(noLabels{})
	mk := func(slug, status string, expires time.Time) uuid.UUID {
		id := uuid.New()
		store.Put(Record{ID: id, Slug: slug, Title: slug, Status: status, PublishedAt: now.AddDate(0, 0, -10), ExpiresAt: expires,
			Price: Money{Amount: "1.00", Currency: "USD"}})
		return id
	}
	live := mk("live", StatusPublished, now.AddDate(0, 0, 80))
	expired := mk("expired", StatusPublished, now.Add(-time.Second))
	atTheInstant := mk("expires-now", StatusPublished, now)
	paused := mk("paused", "PAUSED", now.AddDate(0, 0, 80))
	draft := mk("draft", "DRAFT", now.AddDate(0, 0, 80))
	deleted := uuid.New()

	svc := NewService(store, staleIndex{ids: []uuid.UUID{expired, live, paused, atTheInstant, draft, deleted}}, noLabels{})
	svc.SetClock(func() time.Time { return now })
	page, err := svc.Find(context.Background(), Query{Page: 1, PageSize: 12})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != live {
		t.Fatalf("items = %+v, want only the live listing", page.Items)
	}

	// The same rule by slug, with the two different answers.
	for slug, want := range map[string]error{"live": nil, "expired": ErrUnavailable, "expires-now": ErrUnavailable, "paused": ErrUnavailable, "draft": ErrNotFound, "nope": ErrNotFound} {
		if _, err := svc.BySlug(context.Background(), slug); err != want {
			t.Fatalf("BySlug(%s) = %v, want %v", slug, err, want)
		}
	}
	// And for the listings of an owner and the sitemap.
	if slugs, _ := svc.Slugs(context.Background()); len(slugs) != 1 || slugs[0].Slug != "live" {
		t.Fatalf("slugs = %+v", slugs)
	}
}

func TestParseAmount(t *testing.T) {
	for in, want := range map[string]string{"385000": "385000.00", "950.5": "950.50", " 0 ": "0.00", "1234.5678": "1234.57"} {
		got, err := ParseAmount(in)
		if err != nil || FormatAmount(got) != want {
			t.Fatalf("ParseAmount(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1", "1e9", "1/2", "+5", "1 000"} {
		if _, err := ParseAmount(bad); err == nil {
			t.Fatalf("ParseAmount(%q) should fail", bad)
		}
	}
}
