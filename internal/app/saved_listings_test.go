package app

import (
	"net/http"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/saved"
)

type savedItemBody struct {
	ListingID         string    `json:"listing_id"`
	SavedAt           time.Time `json:"saved_at"`
	Available         bool      `json:"available"`
	UnavailableReason *string   `json:"unavailable_reason"`
	UnavailableLabel  *string   `json:"unavailable_label"`
	Listing           struct {
		Slug      *string `json:"slug"`
		Reference string  `json:"reference"`
		Title     string  `json:"title"`
		Category  struct {
			Code  string `json:"code"`
			Label string `json:"label"`
		} `json:"category"`
		Price struct {
			Amount   string `json:"amount"`
			Currency string `json:"currency"`
		} `json:"price"`
		PricePeriod *string `json:"price_period"`
		City        struct {
			Code  *string `json:"code"`
			Label string  `json:"label"`
		} `json:"city"`
		Country struct {
			Code string `json:"code"`
			Name string `json:"name"`
		} `json:"country"`
		Photo *struct {
			URL string `json:"url"`
		} `json:"photo"`
	} `json:"listing"`
}

type savedPageBody struct {
	Items      []savedItemBody `json:"items"`
	Page       int             `json:"page"`
	PageSize   int             `json:"page_size"`
	Total      int             `json:"total"`
	TotalPages int             `json:"total_pages"`
}

type saveBody struct {
	Saved   bool      `json:"saved"`
	SavedAt time.Time `json:"saved_at"`
	Created bool      `json:"created"`
}

type savedIDsBody struct {
	IDs []string `json:"ids"`
}

// publicSample returns a public sample listing other than the tenant's saved one.
func publicSample(t *testing.T, now time.Time) listings.Record {
	t.Helper()
	taken := map[string]bool{}
	for _, x := range devseed.SavedListingSamples(now) {
		taken[x.ListingID.String()] = true
	}
	for _, r := range devseed.ListingRecords(now) {
		if r.Public(now) && !taken[r.ID.String()] && r.Category != listings.CategoryShortStay {
			return r
		}
	}
	t.Fatal("no public sample listing")
	return listings.Record{}
}

func TestSavedListings(t *testing.T) {
	e := newEnv(t)
	base := "/v1/me/saved-listings"
	tenant, consumer := e.as(devseed.TenantEmail), e.as("consumer@example.test")
	samples := devseed.SavedListingSamples(e.now)
	if len(samples) != 2 {
		t.Fatalf("%d saved samples, want 2", len(samples))
	}

	// The seeded tenant: one available item, one expired, newest first.
	page := decode[savedPageBody](t, e.get(base, tenant))
	if page.Total != 2 || page.Page != 1 || page.PageSize != 24 || page.TotalPages != 1 || len(page.Items) != 2 {
		t.Fatalf("tenant's page: %+v", page)
	}
	avail, gone := page.Items[0], page.Items[1]
	if !avail.Available || avail.Listing.Slug == nil || *avail.Listing.Slug == "" || avail.UnavailableReason != nil ||
		avail.Listing.Category.Label == "" || avail.Listing.Country.Name == "" || avail.Listing.Photo == nil {
		t.Fatalf("available item: %+v", avail)
	}
	if gone.ListingID != devseed.ExpiredListingID.String() || gone.Available || gone.Listing.Slug != nil ||
		gone.UnavailableReason == nil || *gone.UnavailableReason != saved.ReasonExpired || *gone.UnavailableLabel != "Listing expired" ||
		gone.Listing.Title == "" || gone.Listing.Reference != "RP-ZW-10099" {
		t.Fatalf("expired item: %+v", gone)
	}
	if rec := e.get(base, tenant); rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("Cache-Control %q", rec.Header().Get("Cache-Control"))
	}
	ids := decode[savedIDsBody](t, e.get(base+"/ids", tenant))
	if len(ids.IDs) != 1 || ids.IDs[0] != avail.ListingID {
		t.Fatalf("tenant's ids: %+v", ids)
	}

	// Who may: 401 signed out, 403 on an admin session.
	pick := publicSample(t, e.now)
	path := base + "/" + pick.ID.String()
	wantError(t, e.get(base), http.StatusUnauthorized, "UNAUTHENTICATED")
	wantError(t, e.get(base+"/ids"), http.StatusUnauthorized, "UNAUTHENTICATED")
	wantError(t, e.put(path, nil), http.StatusUnauthorized, "UNAUTHENTICATED")
	wantError(t, e.deleteReq(path), http.StatusUnauthorized, "UNAUTHENTICATED")
	admin := e.asAdmin("admin@example.test")
	wantError(t, e.get(base, admin), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.get(base+"/ids", admin), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.put(path, nil, admin), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.deleteReq(path, admin), http.StatusForbidden, "FORBIDDEN")

	// Saving: idempotent, created only the first time.
	first := decode[saveBody](t, e.put(path, nil, consumer))
	if !first.Saved || !first.Created || !first.SavedAt.Equal(e.now) {
		t.Fatalf("first save: %+v", first)
	}
	e.now = e.now.Add(time.Minute)
	again := decode[saveBody](t, e.put(path, nil, consumer))
	if !again.Saved || again.Created || !again.SavedAt.Equal(first.SavedAt) {
		t.Fatalf("second save: %+v", again)
	}
	// Not public, unknown, or not an ID: 404.
	wantError(t, e.put(base+"/"+devseed.ExpiredListingID.String(), nil, consumer), http.StatusNotFound, saved.CodeListingNotFound)
	wantError(t, e.put(base+"/"+devseed.DraftListingID.String(), nil, consumer), http.StatusNotFound, saved.CodeListingNotFound)
	wantError(t, e.put(base+"/not-an-id", nil, consumer), http.StatusNotFound, saved.CodeListingNotFound)

	// The consumer sees their own save only; the tenant's are unchanged.
	mine := decode[savedPageBody](t, e.get(base, consumer))
	if mine.Total != 1 || mine.Items[0].ListingID != pick.ID.String() || !mine.Items[0].Available || *mine.Items[0].Listing.Slug != pick.Slug {
		t.Fatalf("consumer's page: %+v", mine)
	}
	if page := decode[savedPageBody](t, e.get(base, tenant)); page.Total != 2 {
		t.Fatalf("tenant's page after the consumer saved: %+v", page)
	}
	// Removing someone else's save (the tenant's listing, never saved by the
	// consumer) is a no-op for both.
	wantStatus(t, e.deleteReq(base+"/"+samples[0].ListingID.String(), consumer), http.StatusNoContent)
	if page := decode[savedPageBody](t, e.get(base, tenant)); page.Total != 2 {
		t.Fatalf("the consumer removed the tenant's save: %+v", page)
	}

	// Validation of paging.
	wantError(t, e.get(base+"?page=0", consumer), http.StatusBadRequest, "VALIDATION_FAILED")
	wantError(t, e.get(base+"?page_size=101", consumer), http.StatusBadRequest, "VALIDATION_FAILED")

	// The listing expires: it stays in the list, from its snapshot, without
	// a slug, and leaves the ids.
	e.now = e.now.Add(100 * 24 * time.Hour)
	consumer = e.as("consumer@example.test")
	mine = decode[savedPageBody](t, e.get(base, consumer))
	if mine.Total != 1 || mine.Items[0].Available || mine.Items[0].Listing.Slug != nil || *mine.Items[0].UnavailableReason != saved.ReasonExpired ||
		mine.Items[0].Listing.Title != pick.Title {
		t.Fatalf("after expiry: %+v", mine)
	}
	if ids := decode[savedIDsBody](t, e.get(base+"/ids", consumer)); len(ids.IDs) != 0 {
		t.Fatalf("ids after expiry: %+v", ids)
	}
	// ...and can still be removed, twice.
	wantStatus(t, e.deleteReq(path, consumer), http.StatusNoContent)
	wantStatus(t, e.deleteReq(path, consumer), http.StatusNoContent)
	if mine := decode[savedPageBody](t, e.get(base, consumer)); mine.Total != 0 || len(mine.Items) != 0 {
		t.Fatalf("after removal: %+v", mine)
	}

	// Events only on a change: one save, one removal (the seed writes none).
	var savedEvents, unsaved int
	for _, ev := range e.saved.Outbox() {
		switch ev.Type {
		case saved.EventListingSaved:
			savedEvents++
		case saved.EventListingUnsaved:
			unsaved++
		}
		if ev.AggregateID != pick.ID {
			t.Fatalf("event about another listing: %+v", ev)
		}
	}
	if savedEvents != 1 || unsaved != 1 {
		t.Fatalf("%d LISTING_SAVED and %d LISTING_UNSAVED events, want 1 and 1", savedEvents, unsaved)
	}
}

// Deleting one's account removes one's saved listings.
func TestSavedListingsGoWithTheAccount(t *testing.T) {
	e := newEnv(t)
	cookie := e.loginCookie(devseed.TenantEmail, seedPassword)
	if ids, _ := e.saved.ListingIDs(t.Context(), devseed.TenantID); len(ids) != 2 {
		t.Fatalf("seeded saves: %v", ids)
	}
	wantStatus(t, e.del("/v1/me", obj{"password": seedPassword}, browser(cookie)), http.StatusOK)
	if ids, _ := e.saved.ListingIDs(t.Context(), devseed.TenantID); len(ids) != 0 {
		t.Fatalf("saves left after the account was deleted: %v", ids)
	}
}
