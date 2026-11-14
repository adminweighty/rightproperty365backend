package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/trust"
)

// setVerified flips the stored verification of a profile, as an
// administrator's decision would.
func (e *env) setVerified(kind, slug string, verified bool) {
	e.t.Helper()
	r, err := e.directory.Find(e.t.Context(), kind, slug)
	if err != nil {
		e.t.Fatal(err)
	}
	r.Verified = verified
	e.directory.Put(r)
}

func (e *env) trustSummary() trust.Summary {
	e.t.Helper()
	rec := e.get("/v1/trust/summary")
	wantStatus(e.t, rec, http.StatusOK)
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=60, stale-while-revalidate=300" || rec.Header().Get("ETag") == "" {
		e.t.Fatalf("trust summary Cache-Control = %q", cc)
	}
	return decode[trust.Summary](e.t, rec)
}

// fixedHits is a search engine that answers with the IDs it was given,
// whatever is asked: an index that is behind the database.
type fixedHits struct{ ids []uuid.UUID }

func (f fixedHits) Search(context.Context, listings.Query, time.Time) (listings.Hits, error) {
	return listings.Hits{IDs: f.ids, Total: len(f.ids)}, nil
}

func (f fixedHits) Suggest(context.Context, string, int, time.Time) ([]listings.Suggestion, error) {
	return nil, nil
}

// The six listings of Sample Estates, by slug.
func estatesListings(e *env) []listings.Listing {
	var out []listings.Listing
	for _, l := range e.search("?page_size=48").Items {
		if l.Advertiser.Agency != nil && l.Advertiser.Agency.Slug == "sample-estates" {
			out = append(out, l)
		}
	}
	return out
}

// Every listing the API shows comes from a verified advertiser, and says so.
func TestEveryPublicListingHasAVerifiedAdvertiser(t *testing.T) {
	e := newEnv(t)
	page := e.search("?page_size=48")
	if page.Total != devseed.ListingCount+devseed.BackendListingCount || len(page.Items) != page.Total {
		t.Fatalf("total = %d", page.Total)
	}
	for _, l := range page.Items {
		v := l.Verification
		if !v.AdvertiserVerified || v.AdvertiserVerifiedAt == nil || !l.Advertiser.Verified || !l.Advertiser.Display.Verified {
			t.Fatalf("%s: verification %+v, advertiser verified %v", l.Slug, v, l.Advertiser.Verified)
		}
		// The sample listings were marked reviewed by the seed, when they were published.
		if !v.ListingReviewed || v.ListingReviewedAt == nil || !v.ListingReviewedAt.Equal(l.PublishedAt) {
			t.Fatalf("%s: review %+v", l.Slug, v)
		}
		detail := e.listing(l.Slug)
		if detail.Verification.AdvertiserVerified != true || !detail.Verification.AdvertiserVerifiedAt.Equal(*v.AdvertiserVerifiedAt) {
			t.Fatalf("%s detail: %+v", l.Slug, detail.Verification)
		}
	}
	// A listing nobody reviewed says so: the mark is never assumed.
	r, err := e.listings.BySlug(t.Context(), "office-suite-eastlea")
	if err != nil {
		t.Fatal(err)
	}
	r.ReviewedAt, r.ReviewedBy = nil, nil
	e.listings.Put(r)
	if !strings.Contains(e.get("/v1/listings/office-suite-eastlea").Body.String(), `"listing_reviewed":false,"listing_reviewed_at":null`) {
		t.Fatal("an unreviewed listing does not say so")
	}

	// The summary: counted, not asserted.
	s := e.trustSummary()
	organisations := map[uuid.UUID]bool{}
	for _, p := range devseed.Profiles() {
		if p.AgentUserID == nil {
			organisations[p.Organisation.ID] = true
		}
	}
	if s.PublicListings != page.Total || s.ListingsFromVerifiedAdvertisers != s.PublicListings || s.ListingsReviewed != page.Total-1 ||
		s.VerifiedAdvertisers.Organisations != len(organisations) || s.VerifiedAdvertisers.DirectOwners != 1 || s.GeneratedAt.IsZero() {
		t.Fatalf("summary = %+v (want %d listings, %d organisations)", s, page.Total, len(organisations))
	}
}

// An organisation that stops being verified disappears from every public
// endpoint at once — and comes back when it is verified again.
func TestUnverifiedOrganisationIsNotPublic(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	mine := estatesListings(e)
	if len(mine) != 6 {
		t.Fatalf("%d listings of Sample Estates", len(mine))
	}
	before, summary := e.search("?page_size=48"), e.trustSummary()
	// A request made while the agency was verified stays in its inbox.
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(1)), http.StatusCreated)

	e.setVerified("agencies", "sample-estates", false)

	// Search: gone from the results, the total and the facets.
	after := e.search("?page_size=48")
	if after.Total != before.Total-6 || len(estatesListings(e)) != 0 {
		t.Fatalf("total %d -> %d", before.Total, after.Total)
	}
	for _, l := range after.Items {
		if !l.Advertiser.Verified || !l.Verification.AdvertiserVerified {
			t.Fatalf("%s is shown without a verified advertiser", l.Slug)
		}
	}
	count := func(p listingPage) int {
		n := 0
		for _, f := range p.Facets.Deal {
			n += f.Count
		}
		return n
	}
	if count(after) != count(before)-6 {
		t.Fatalf("deal facet %d -> %d", count(before), count(after))
	}
	for _, query := range []string{"?q=borrowdale&sort=relevance", "?advertiser_type=agency&page_size=48", "?featured=true&page_size=48", "?reference=RP-ZW-10001"} {
		for _, l := range e.search(query).Items {
			if l.Advertiser.Agency != nil && l.Advertiser.Agency.Slug == "sample-estates" {
				t.Fatalf("%s still returns %s", query, l.Slug)
			}
		}
	}
	// By slug: 404, never 410 — nothing says the advertiser lost its verification.
	for _, l := range mine {
		wantError(t, e.get("/v1/listings/"+l.Slug), http.StatusNotFound, listings.CodeListingNotFound)
	}
	// Suggestions, the sitemap, the countries' counts.
	if body := e.get("/v1/listings/suggest?q=four-bedroom").Body.String(); strings.Contains(body, mine[0].Slug) || strings.Contains(body, "four-bedroom-family-home-borrowdale") {
		t.Fatalf("suggestions name a hidden listing: %s", body)
	}
	sitemap := e.get("/v1/sitemap").Body.String()
	for _, l := range mine {
		if strings.Contains(sitemap, `"`+l.Slug+`"`) {
			t.Fatalf("the sitemap lists %s", l.Slug)
		}
	}
	if strings.Contains(sitemap, `"slug":"sample-estates"`) {
		t.Fatal("the sitemap lists the unverified agency")
	}
	// The profile and its logo are gone; its agents' pages list nothing.
	wantError(t, e.get("/v1/directory/agencies/sample-estates"), http.StatusNotFound, directory.CodeProfileNotFound)
	wantError(t, e.get("/v1/directory/agencies/sample-estates/logo"), http.StatusNotFound, directory.CodeProfileNotFound)
	if strings.Contains(e.get("/v1/directory/agencies?page_size=48").Body.String(), `"slug":"sample-estates"`) {
		t.Fatal("the directory lists the unverified agency")
	}
	agent := decode[struct {
		Profile struct {
			ListingCount int                   `json:"listing_count"`
			Listings     []listings.Listing    `json:"listings"`
			Agency       *listings.ProfileLink `json:"agency"`
		} `json:"profile"`
	}](t, e.get("/v1/directory/agents/tendai-ncube")).Profile
	if agent.ListingCount != 0 || len(agent.Listings) != 0 || agent.Agency != nil {
		t.Fatalf("the agent's page still shows the agency's listings: %+v", agent)
	}
	// Requests, appointments: refused, as for a listing that does not exist.
	wantError(t, e.post("/v1/property-requests", e.propertyRequest(1)), http.StatusNotFound, propertyrequests.CodeListingNotFound)
	wantError(t, e.post("/v1/property-requests", e.viewingRequest(2, 48*time.Hour)), http.StatusNotFound, propertyrequests.CodeListingNotFound)
	if rec := e.post("/v1/appointments", e.appointmentInput(1, 72*time.Hour), c.as(identity.SeedConsumerEmail)); rec.Code != http.StatusNotFound {
		t.Fatalf("appointment for a hidden listing = %d %s", rec.Code, rec.Body)
	}
	// Other advertisers are untouched.
	wantStatus(t, e.get("/v1/listings/five-bedroom-house-victoria-falls"), http.StatusOK)
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(8)), http.StatusCreated)

	// The agency's people keep their inbox and their own pages.
	if got := e.inboxListings(identity.SeedAgentEmail); len(got) == 0 {
		t.Fatal("the agent lost the inbox")
	}
	wantStatus(t, e.get("/v1/me/listings", c.as(devseed.EstatesOwnerEmail)), http.StatusOK)
	if profiles := e.myProfiles(c.as(devseed.EstatesOwnerEmail)); len(profiles) != 2 || profiles[0].Verified {
		t.Fatalf("my profiles = %+v", profiles)
	}

	// The summary follows.
	if s := e.trustSummary(); s.PublicListings != summary.PublicListings-6 || s.ListingsFromVerifiedAdvertisers != s.PublicListings ||
		s.VerifiedAdvertisers.Organisations != summary.VerifiedAdvertisers.Organisations-1 {
		t.Fatalf("summary = %+v, before %+v", s, summary)
	}

	// An index that is behind still proposes the hidden listings: every hit
	// is re-read from the source of truth and dropped.
	var ids []uuid.UUID
	for _, l := range before.Items {
		ids = append(ids, l.ID)
	}
	stale := listings.NewService(e.listings, fixedHits{ids: ids}, reference.NewService(e.reference, nil))
	stale.SetClock(func() time.Time { return e.now })
	found, err := stale.Find(t.Context(), listings.Query{Page: 1, PageSize: 48})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Items) != before.Total-6 || found.Total != before.Total-6 {
		t.Fatalf("a stale index returned %d items (total %d), want %d", len(found.Items), found.Total, before.Total-6)
	}
	for _, l := range found.Items {
		if !l.Advertiser.Verified || (l.Advertiser.Agency != nil && l.Advertiser.Agency.Slug == "sample-estates") {
			t.Fatalf("a stale index brought back %s", l.Slug)
		}
	}

	// Verified again: everything is back.
	e.setVerified("agencies", "sample-estates", true)
	if again := e.search("?page_size=48"); again.Total != before.Total || len(estatesListings(e)) != 6 {
		t.Fatalf("total after re-verification = %d", again.Total)
	}
	for _, l := range mine {
		wantStatus(t, e.get("/v1/listings/"+l.Slug), http.StatusOK)
	}
	wantStatus(t, e.get("/v1/directory/agencies/sample-estates"), http.StatusOK)
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(1)), http.StatusCreated)
	if s := e.trustSummary(); s.PublicListings != summary.PublicListings {
		t.Fatalf("summary after re-verification = %+v", s)
	}

	// A suspended profile hides the listings just the same.
	r, err := e.directory.Find(t.Context(), "agencies", "sample-estates")
	if err != nil {
		t.Fatal(err)
	}
	r.Status = "SUSPENDED"
	e.directory.Put(r)
	wantError(t, e.get("/v1/listings/four-bedroom-family-home-borrowdale"), http.StatusNotFound, listings.CodeListingNotFound)
}

// A host that is not verified: its short stays, their calendars and requests
// to stay are closed too.
func TestUnverifiedHostShortStays(t *testing.T) {
	e := newEnv(t)
	stay := e.propertyRequest(22)
	stay["request_type"], stay["check_in"], stay["check_out"], stay["guests"] = "STAY_REQUEST", "2026-11-20", "2026-11-23", 2
	wantStatus(t, e.get("/v1/listings/safari-lodge-victoria-falls/availability"), http.StatusOK)

	e.setVerified("hosts", "sample-falls-stays", false)
	wantError(t, e.get("/v1/listings/safari-lodge-victoria-falls"), http.StatusNotFound, listings.CodeListingNotFound)
	if rec := e.get("/v1/listings/safari-lodge-victoria-falls/availability"); rec.Code != http.StatusNotFound {
		t.Fatalf("availability of a hidden short stay = %d", rec.Code)
	}
	wantError(t, e.post("/v1/property-requests", stay), http.StatusNotFound, propertyrequests.CodeListingNotFound)
	for _, l := range e.search("?check_in=2026-11-20&check_out=2026-11-23&page_size=48").Items {
		if l.Advertiser.Agency != nil && l.Advertiser.Agency.Slug == "sample-falls-stays" {
			t.Fatalf("a search by stay returns %s", l.Slug)
		}
	}
	// The host still manages the calendar.
	wantStatus(t, e.get("/v1/me/listings", newCast(e).as("sample-falls-stays@example.test")), http.StatusOK)

	e.setVerified("hosts", "sample-falls-stays", true)
	e.seq++
	stay["idempotency_key"] = fmt.Sprintf("test-key-%04d", e.seq)
	wantStatus(t, e.post("/v1/property-requests", stay), http.StatusCreated)
}

// A direct owner's listing is public only while their PRIVATE_SELLER or
// LANDLORD role is approved.
func TestDirectOwnerMustBeApproved(t *testing.T) {
	e := newEnv(t)
	const slug = "five-bedroom-house-victoria-falls"
	if l := e.listing(slug); l.Advertiser.Type != listings.AdvertiserPrivateSeller || !l.Advertiser.Verified || l.Advertiser.Name != "Grace Mhlanga" {
		t.Fatalf("advertiser = %+v", l.Advertiser)
	}
	owners := e.trustSummary().VerifiedAdvertisers.DirectOwners

	for _, status := range []string{identity.AccountRolePendingVerification, identity.AccountRoleRejected} {
		if err := e.identity.SetAccountRoleStatus(devseed.SellerID, identity.AccountRolePrivateSeller, status); err != nil {
			t.Fatal(err)
		}
		wantError(t, e.get("/v1/listings/"+slug), http.StatusNotFound, listings.CodeListingNotFound)
		if page := e.search("?advertiser_type=private"); page.Total != 0 || len(page.Items) != 0 {
			t.Fatalf("%s: %d private listings are public", status, page.Total)
		}
		if strings.Contains(e.get("/v1/sitemap").Body.String(), slug) {
			t.Fatalf("%s: the sitemap lists the listing", status)
		}
		wantError(t, e.post("/v1/property-requests", e.propertyRequest(6)), http.StatusNotFound, propertyrequests.CodeListingNotFound)
		if got := e.trustSummary().VerifiedAdvertisers.DirectOwners; got != owners-1 {
			t.Fatalf("%s: %d direct owners", status, got)
		}
	}

	// Approved (ACTIVE): public again, and shown as verified.
	if err := e.identity.SetAccountRoleStatus(devseed.SellerID, identity.AccountRolePrivateSeller, identity.AccountRoleActive); err != nil {
		t.Fatal(err)
	}
	if l := e.listing(slug); !l.Verification.AdvertiserVerified || !l.Advertiser.Display.Verified {
		t.Fatalf("after approval: %+v", l.Verification)
	}
	if page := e.search("?advertiser_type=private"); page.Total != 1 {
		t.Fatalf("%d private listings", page.Total)
	}
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(6)), http.StatusCreated)
}
