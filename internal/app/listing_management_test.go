package app

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/storage"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type managedPhotoBody struct {
	ID       string `json:"id"`
	Position int    `json:"position"`
	Alt      string `json:"alt"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Status   string `json:"status"`
	URL      string `json:"url"`
	Variants []struct {
		Size   string `json:"size"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
		URL    string `json:"url"`
	} `json:"variants"`
}

type listingView struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Reference   string `json:"reference"`
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	Public      bool   `json:"public"`
	Review      struct {
		SubmittedAt *time.Time `json:"submitted_at"`
		ReviewedAt  *time.Time `json:"reviewed_at"`
		Reason      *string    `json:"reason"`
	} `json:"review"`
	PendingChanges *struct {
		Status    string  `json:"status"`
		Reason    *string `json:"reason"`
		NewPhotos int     `json:"new_photos"`
	} `json:"pending_changes"`
	Content struct {
		Title       string  `json:"title"`
		Deal        *string `json:"deal"`
		Category    *string `json:"category"`
		PricePeriod *string `json:"price_period"`
		Country     *string `json:"country"`
		City        *string `json:"city"`
		Suburb      *string `json:"suburb"`
		MaxGuests   *int    `json:"max_guests"`
		Price       struct {
			Amount   *string `json:"amount"`
			Currency *string `json:"currency"`
		} `json:"price"`
	} `json:"content"`
	Photos       []managedPhotoBody `json:"photos"`
	PhotoLimit   int                `json:"photo_limit"`
	Completeness struct {
		Ready   bool `json:"ready"`
		Missing []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"missing"`
	} `json:"completeness"`
	Owner struct {
		Type           string  `json:"type"`
		OrganisationID *string `json:"organisation_id"`
		AgentUserID    *string `json:"agent_user_id"`
	} `json:"owner"`
	CanEdit     bool       `json:"can_edit"`
	Actions     []string   `json:"actions"`
	PublishedAt *time.Time `json:"published_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Timeline    []struct {
		Action      string  `json:"action"`
		ToStatus    *string `json:"to_status"`
		Actor       string  `json:"actor"`
		ActorUserID *string `json:"actor_user_id"`
	} `json:"timeline"`
	// Reviewer view only.
	ReviewKind *string `json:"review_kind"`
	Changes    []struct {
		Field  string `json:"field"`
		Before any    `json:"before"`
		After  any    `json:"after"`
	} `json:"changes"`
	Decisions []string `json:"decisions"`
}

func (b listingView) missing() string {
	var out []string
	for _, m := range b.Completeness.Missing {
		out = append(out, m.Field)
	}
	return strings.Join(out, ",")
}

type queueBody struct {
	Items []struct {
		ID          string                `json:"id"`
		Reference   string                `json:"reference"`
		Title       string                `json:"title"`
		Status      string                `json:"status"`
		ReviewKind  *string               `json:"review_kind"`
		PhotoCount  int                   `json:"photo_count"`
		Advertiser  struct{ Name string } `json:"advertiser"`
		Country     struct{ Code string } `json:"country"`
		SubmittedAt *time.Time            `json:"submitted_at"`
	} `json:"items"`
	Total int `json:"total"`
}

func (q queueBody) has(id string) bool {
	for _, it := range q.Items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func managedOf(t *testing.T, rec *httptest.ResponseRecorder, status int) listingView {
	t.Helper()
	wantStatus(t, rec, status)
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want private, no-store", cc)
	}
	return decode[struct {
		Listing listingView `json:"listing"`
	}](t, rec).Listing
}

func (e *env) deleteReq(path string, opts ...reqOpt) *httptest.ResponseRecorder {
	return e.do(http.MethodDelete, path, nil, opts...)
}

// testPhoto draws a JPEG of w x h pixels.
func testPhoto(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y += 4 {
		for x := 0; x < w; x += 4 {
			c := color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 120, A: 255}
			for dy := 0; dy < 4 && y+dy < h; dy++ {
				for dx := 0; dx < 4 && x+dx < w; dx++ {
					img.SetRGBA(x+dx, y+dy, c)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withOrientation inserts an EXIF block with the orientation tag (and a
// camera comment) right after the JPEG's start-of-image marker.
func withOrientation(content []byte, orientation uint16) []byte {
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08")
	tiff = binary.BigEndian.AppendUint16(tiff, 1)                       // one entry
	tiff = append(tiff, 0x01, 0x12, 0x00, 0x03, 0x00, 0x00, 0x00, 0x01) // Orientation, SHORT, count 1
	tiff = binary.BigEndian.AppendUint16(tiff, orientation)             // value
	tiff = append(tiff, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)             // padding, no next IFD
	payload := append(append([]byte("Exif\x00\x00"), tiff...), []byte("SecretCamera GPS 17.8S")...)
	segment := append([]byte{0xff, 0xe1}, binary.BigEndian.AppendUint16(nil, uint16(len(payload)+2))...)
	out := append([]byte{}, content[:2]...)
	out = append(out, append(segment, payload...)...)
	return append(out, content[2:]...)
}

// uploadPhoto sends the multipart POST an editor sends, each from its own
// address so that the tests do not run into the upload rate limit.
func (e *env) uploadPhoto(listingID string, content []byte, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "photo.jpg")
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = part.Write(content)
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/me/listings/"+listingID+"/photos", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	e.seq++
	req.RemoteAddr = fmt.Sprintf("198.51.%d.%d:40000", 100+e.seq/250, e.seq%250+1)
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// completeListing creates a listing and fills in everything a submission
// needs except the photos.
func (e *env) completeListing(as reqOpt, title string) listingView {
	e.t.Helper()
	created := managedOf(e.t, e.post("/v1/me/listings", obj{"title": title, "country": "ZW"}, as), http.StatusCreated)
	return managedOf(e.t, e.patch("/v1/me/listings/"+created.ID, obj{
		"deal": "sale", "category": "residential", "property_type": "house",
		"price": obj{"amount": "250000", "currency": "USD"}, "city": "harare", "suburb": "borrowdale", "bedrooms": 3, "bathrooms": 2,
		"description": "A bright family house with a garden, a double garage and a borehole, close to schools.",
	}, as), http.StatusOK)
}

// readyListing is a complete listing with one described photo.
func (e *env) readyListing(as reqOpt, title string) listingView {
	e.t.Helper()
	l := e.completeListing(as, title)
	withPhoto := managedOf(e.t, e.uploadPhoto(l.ID, testPhoto(e.t, 1200, 900), as), http.StatusCreated)
	return managedOf(e.t, e.patch("/v1/me/listings/"+l.ID+"/photos/"+withPhoto.Photos[0].ID, obj{"alt": "The front of the house"}, as), http.StatusOK)
}

func (e *env) outboxTypes(id string) []string {
	var out []string
	for _, ev := range e.listings.Outbox() {
		if ev.AggregateID.String() == id {
			out = append(out, ev.Type)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The whole way: create, edit, photos, submit, approve, public
// ---------------------------------------------------------------------------

func TestListingFromDraftToPublic(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	tendai := c.as(identity.SeedAgentEmail)
	admin := e.admin()

	// Create: a DRAFT of Tendai's agency, with Tendai as its agent; slug and
	// reference come from the server.
	created := managedOf(t, e.post("/v1/me/listings", obj{"title": "Sunny cottage in Borrowdale", "country": "ZW"}, tendai), http.StatusCreated)
	if created.Status != "DRAFT" || created.Owner.Type != "organisation" || created.Owner.OrganisationID == nil ||
		*created.Owner.OrganisationID != devseed.SampleEstates.ID.String() || created.Owner.AgentUserID == nil ||
		*created.Owner.AgentUserID != identity.SeedAgentUserID.String() || !created.CanEdit || created.PhotoLimit != 20 {
		t.Fatalf("created: %+v", created)
	}
	if !strings.HasPrefix(created.Reference, "RP-ZW-") || !strings.HasPrefix(created.Slug, "sunny-cottage-in-borrowdale-zw-") ||
		created.Public || created.Completeness.Ready || *created.Content.Price.Currency != "USD" {
		t.Fatalf("created: %+v", created)
	}
	id := created.ID
	path := "/v1/me/listings/" + id

	// Validation against the reference data.
	env := wantError(t, e.patch(path, obj{"deal": "swap", "city": "london", "price": obj{"amount": "12.345"}, "max_guests": 4, "colour": "red"}, tendai),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["colour"] == "" {
		t.Fatalf("an unknown field must be refused: %v", env.Error.Fields)
	}
	env = wantError(t, e.patch(path, obj{"deal": "swap", "city": "london", "price": obj{"amount": "12.345"}, "max_guests": 4}, tendai),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	for _, f := range []string{"deal", "city", "price", "max_guests"} {
		if env.Error.Fields[f] == "" {
			t.Fatalf("field %s not refused: %v", f, env.Error.Fields)
		}
	}
	// A country without reference cities takes a free-text city; ZW does not.
	env = wantError(t, e.patch(path, obj{"city_name": "Somewhere"}, tendai), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["city_name"] == "" {
		t.Fatalf("city_name in ZW: %v", env.Error.Fields)
	}
	// Choosing a short stay makes it a rental per night.
	short := managedOf(t, e.patch(path, obj{"category": "short-stay"}, tendai), http.StatusOK)
	if *short.Content.Deal != "rent" || *short.Content.PricePeriod != "night" {
		t.Fatalf("short stay: %+v", short.Content)
	}
	l := managedOf(t, e.patch(path, obj{
		"deal": "sale", "category": "residential", "property_type": "cottage", "price": obj{"amount": "185000", "currency": "USD"},
		"city": "harare", "suburb": "borrowdale", "bedrooms": 2, "bathrooms": 1, "features": []string{},
		"description": "A garden cottage with two bedrooms, a covered patio and fibre internet, in a quiet road.",
	}, tendai), http.StatusOK)
	if l.Content.PricePeriod != nil || l.missing() != "photos" {
		t.Fatalf("after editing: period %v, missing %s", l.Content.PricePeriod, l.missing())
	}

	// Without a photo it cannot be submitted.
	env = wantError(t, e.post(path+"/submit", nil, tendai), http.StatusUnprocessableEntity, listings.CodeListingIncomplete)
	if env.Error.Fields["photos"] == "" {
		t.Fatalf("incomplete: %v", env.Error.Fields)
	}

	// One photo: three sizes, waiting for review.
	l = managedOf(t, e.uploadPhoto(id, testPhoto(t, 1600, 1200), tendai), http.StatusCreated)
	if len(l.Photos) != 1 {
		t.Fatalf("photos: %+v", l.Photos)
	}
	ph := l.Photos[0]
	if ph.Status != "PENDING_REVIEW" || ph.Width != 1600 || ph.Height != 1200 || len(ph.Variants) != 3 ||
		ph.Variants[1].Width != 1280 || ph.Variants[2].Width != 640 || ph.Variants[2].Height != 480 ||
		!strings.HasPrefix(ph.URL, "/v1/listing-photos/"+ph.ID+"/large?v=") {
		t.Fatalf("photo: %+v", ph)
	}
	// Not public: nobody else gets it; its manager does, never cached.
	wantError(t, e.get(ph.URL), http.StatusNotFound, listings.CodePhotoNotFound)
	wantError(t, e.get(ph.URL, c.as(devseed.CommercialAgentEmail)), http.StatusNotFound, listings.CodePhotoNotFound)
	rec := e.get(ph.Variants[2].URL, tendai)
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Content-Type") != "image/jpeg" || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("private photo headers: %v", rec.Header())
	}
	if cfg, _, err := image.DecodeConfig(rec.Body); err != nil || cfg.Width != 640 {
		t.Fatalf("small size: %v %v", cfg, err)
	}

	// Every photo needs a description.
	env = wantError(t, e.post(path+"/submit", nil, tendai), http.StatusUnprocessableEntity, listings.CodeListingIncomplete)
	if env.Error.Fields["photos"] == "" {
		t.Fatalf("alt missing: %v", env.Error.Fields)
	}
	wantError(t, e.patch(path+"/photos/"+ph.ID, obj{"alt": "<b>bold</b>"}, tendai), http.StatusBadRequest, httpx.CodeValidationFailed)
	managedOf(t, e.patch(path+"/photos/"+ph.ID, obj{"alt": "The cottage from the garden"}, tendai), http.StatusOK)

	// Submit: waiting for review, and locked until then.
	l = managedOf(t, e.post(path+"/submit", nil, tendai), http.StatusOK)
	if l.Status != "PENDING_REVIEW" || l.Review.SubmittedAt == nil || contains(l.Actions, "edit") {
		t.Fatalf("submitted: %+v", l)
	}
	wantError(t, e.patch(path, obj{"title": "Changed"}, tendai), http.StatusConflict, listings.CodeInReview)
	wantError(t, e.uploadPhoto(id, testPhoto(t, 1200, 900), tendai), http.StatusConflict, listings.CodeInReview)
	wantError(t, e.get("/v1/listings/"+l.Slug), http.StatusNotFound, listings.CodeListingNotFound)

	// The reviewer's queue and detail.
	queue := decode[queueBody](t, e.get("/v1/admin/listings", withCookie(admin)))
	if !queue.has(id) || !queue.has(devseed.PendingListingID.String()) {
		t.Fatalf("queue: %+v", queue)
	}
	detail := managedOf(t, e.get("/v1/admin/listings/"+id, withCookie(admin)), http.StatusOK)
	if detail.ReviewKind == nil || *detail.ReviewKind != "NEW" || len(detail.Decisions) != 3 || len(detail.Photos) != 1 ||
		len(detail.Timeline) != 2 || detail.Timeline[1].ActorUserID == nil {
		t.Fatalf("admin detail: %+v", detail)
	}
	if rec := e.get(ph.URL, withCookie(admin)); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("reviewer photo: %d %v", rec.Code, rec.Header())
	}

	// Approve: public, for 90 days, reviewed, its photo too.
	approved := managedOf(t, e.post("/v1/admin/listings/"+id+"/approve", nil, browser(admin)), http.StatusOK)
	if approved.Status != "PUBLISHED" || !approved.Public || approved.PublishedAt == nil || approved.ExpiresAt == nil ||
		!approved.ExpiresAt.Equal(e.now.Add(listings.PublicationPeriod)) || approved.Photos[0].Status != "APPROVED" || len(approved.Decisions) != 0 {
		t.Fatalf("approved: %+v", approved)
	}
	if events := e.outboxTypes(id); !contains(events, listings.EventListingPublished) || !contains(events, listings.EventListingApproved) {
		t.Fatalf("events: %v", events)
	}
	wantError(t, e.post("/v1/admin/listings/"+id+"/approve", nil, browser(admin)), http.StatusConflict, listings.CodeStateConflict)

	public := decode[struct {
		Listing listings.Listing `json:"listing"`
	}](t, e.get("/v1/listings/"+l.Slug)).Listing
	if public.Title != "Sunny cottage in Borrowdale" || len(public.Photos) != 1 || !public.Verification.ListingReviewed ||
		public.Photos[0].URL != ph.URL || public.Photos[0].Alt != "The cottage from the garden" || public.Photos[0].Width != 1600 ||
		len(public.Photos[0].Variants) != 3 || public.Photos[0].Variants[2].Width != 640 {
		t.Fatalf("public listing: %+v", public)
	}
	if found := e.get("/v1/listings?q=sunny+cottage").Body.String(); !strings.Contains(found, `"slug":"`+l.Slug+`"`) {
		t.Fatalf("not found by search: %s", found)
	}
	// The public photo: anyone, cached for a year with its version, briefly without.
	rec = e.get(ph.URL)
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("public photo headers: %v", rec.Header())
	}
	etag := rec.Header().Get("ETag")
	if rec := e.get(ph.URL, withHeader("If-None-Match", etag)); rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", rec.Code)
	}
	if rec := e.get(strings.Split(ph.URL, "?")[0]); rec.Header().Get("Cache-Control") != "public, max-age=300, stale-while-revalidate=86400" {
		t.Fatalf("unversioned: %v", rec.Header())
	}
	wantError(t, e.get("/v1/listing-photos/"+ph.ID+"/huge"), http.StatusNotFound, listings.CodePhotoNotFound)

	// Editing the published listing: the public keeps the approved version
	// until the changes are approved.
	l = managedOf(t, e.patch(path, obj{"title": "Sunny garden cottage in Borrowdale"}, tendai), http.StatusOK)
	if l.Status != "PUBLISHED" || l.PendingChanges == nil || l.PendingChanges.Status != "EDITING" || l.Content.Title != "Sunny garden cottage in Borrowdale" {
		t.Fatalf("edited: %+v", l)
	}
	l = managedOf(t, e.uploadPhoto(id, testPhoto(t, 1200, 900), tendai), http.StatusCreated)
	newPhoto := l.Photos[1]
	managedOf(t, e.patch(path+"/photos/"+newPhoto.ID, obj{"alt": "The kitchen"}, tendai), http.StatusOK)
	wantError(t, e.get(newPhoto.URL), http.StatusNotFound, listings.CodePhotoNotFound)
	// The country of a published listing stays.
	wantError(t, e.patch(path, obj{"country": "GB"}, tendai), http.StatusBadRequest, httpx.CodeValidationFailed)
	if got := decode[struct{ Listing listings.Listing }](t, e.get("/v1/listings/"+l.Slug)).Listing; got.Title != "Sunny cottage in Borrowdale" || len(got.Photos) != 1 {
		t.Fatalf("public during the edit: %q, %d photos", got.Title, len(got.Photos))
	}
	l = managedOf(t, e.post(path+"/submit", nil, tendai), http.StatusOK)
	if l.PendingChanges.Status != "PENDING_REVIEW" || l.PendingChanges.NewPhotos != 1 {
		t.Fatalf("edit submitted: %+v", l.PendingChanges)
	}
	edits := decode[queueBody](t, e.get("/v1/admin/listings?kind=EDIT", withCookie(admin)))
	if !edits.has(id) || edits.has(devseed.PendingListingID.String()) || *edits.Items[0].ReviewKind != "EDIT" {
		t.Fatalf("edit queue: %+v", edits)
	}
	detail = managedOf(t, e.get("/v1/admin/listings/"+id, withCookie(admin)), http.StatusOK)
	if *detail.ReviewKind != "EDIT" || len(detail.Changes) != 1 || detail.Changes[0].Field != "title" ||
		detail.Changes[0].Before != "Sunny cottage in Borrowdale" || detail.Changes[0].After != "Sunny garden cottage in Borrowdale" {
		t.Fatalf("what changed: %+v", detail.Changes)
	}
	managedOf(t, e.post("/v1/admin/listings/"+id+"/approve", nil, browser(admin)), http.StatusOK)
	got := decode[struct{ Listing listings.Listing }](t, e.get("/v1/listings/"+l.Slug)).Listing
	if got.Title != "Sunny garden cottage in Borrowdale" || len(got.Photos) != 2 || got.Photos[1].Alt != "The kitchen" ||
		!got.ExpiresAt.Equal(e.now.Add(listings.PublicationPeriod)) {
		t.Fatalf("after the edit was approved: %q, %d photos, expires %v", got.Title, len(got.Photos), got.ExpiresAt)
	}
	if rec := e.get(newPhoto.URL); rec.Code != http.StatusOK {
		t.Fatalf("approved new photo: %d", rec.Code)
	}

	// The lister's dashboard counts the review statuses.
	d := decode[obj](t, e.get("/v1/me/dashboard", tendai))
	keys := map[string]float64{}
	for _, counter := range d["counters"].([]any) {
		keys[counter.(obj)["key"].(string)] = counter.(obj)["value"].(float64)
	}
	if keys["listings.status.DRAFT"] != 1 || keys["listings.status.PENDING_REVIEW"] != 1 || keys["listings.status.CHANGES_REQUESTED"] != 0 {
		t.Fatalf("dashboard counters: %v", keys)
	}
}

// ---------------------------------------------------------------------------
// Photos
// ---------------------------------------------------------------------------

func TestTwentyPhotosPerListing(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	l := e.completeListing(tendai, "House with twenty photos")
	photo := testPhoto(t, 900, 800)
	for i := 0; i < listings.MaxPhotosPerListing; i++ {
		wantStatus(t, e.uploadPhoto(l.ID, photo, tendai), http.StatusCreated)
	}
	env := wantError(t, e.uploadPhoto(l.ID, photo, tendai), http.StatusConflict, listings.CodePhotoLimitReached)
	if env.Error.Message != "A property can have up to 20 photos." {
		t.Fatalf("message = %q", env.Error.Message)
	}
	if got := managedOf(t, e.get("/v1/me/listings/"+l.ID, tendai), http.StatusOK); len(got.Photos) != 20 {
		t.Fatalf("%d photos", len(got.Photos))
	}
	// Room again after a deletion.
	got := managedOf(t, e.get("/v1/me/listings/"+l.ID, tendai), http.StatusOK)
	managedOf(t, e.deleteReq("/v1/me/listings/"+l.ID+"/photos/"+got.Photos[0].ID, tendai), http.StatusOK)
	wantStatus(t, e.uploadPhoto(l.ID, photo, tendai), http.StatusCreated)
}

func TestPhotoUploadRules(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	l := e.completeListing(tendai, "House for the photo rules")

	// Too large: refused before it is decoded.
	big := append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, listings.MaxPhotoBytes)...)
	wantError(t, e.uploadPhoto(l.ID, big, tendai), http.StatusRequestEntityTooLarge, listings.CodeFileTooLarge)
	// Not an image, or not one that decodes.
	wantError(t, e.uploadPhoto(l.ID, []byte("just some text, not a photo"), tendai), http.StatusUnsupportedMediaType, listings.CodeUnsupportedFile)
	broken := testPhoto(t, 1200, 900)
	wantError(t, e.uploadPhoto(l.ID, broken[:len(broken)/3], tendai), http.StatusUnsupportedMediaType, listings.CodeUnsupportedFile)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="900"><script>alert(1)</script></svg>`)
	wantError(t, e.uploadPhoto(l.ID, svg, tendai), http.StatusUnsupportedMediaType, listings.CodeUnsupportedFile)
	// Too small.
	env := wantError(t, e.uploadPhoto(l.ID, testPhoto(t, 700, 500), tendai), http.StatusBadRequest, httpx.CodeValidationFailed)
	if !strings.Contains(env.Error.Fields["file"], "800 pixels") {
		t.Fatalf("too small: %v", env.Error.Fields)
	}
	if got := managedOf(t, e.get("/v1/me/listings/"+l.ID, tendai), http.StatusOK); len(got.Photos) != 0 {
		t.Fatalf("a refused upload left %d photos", len(got.Photos))
	}

	// A PNG is stored as JPEG; a sideways phone photo is turned upright and
	// loses its metadata.
	pngImg := image.NewRGBA(image.Rect(0, 0, 1000, 800))
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, pngImg); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.uploadPhoto(l.ID, pngBuf.Bytes(), tendai), http.StatusCreated)
	sideways := withOrientation(testPhoto(t, 1600, 900), 6)
	got := managedOf(t, e.uploadPhoto(l.ID, sideways, tendai), http.StatusCreated)
	upright := got.Photos[1]
	if upright.Width != 900 || upright.Height != 1600 {
		t.Fatalf("EXIF orientation not applied: %dx%d", upright.Width, upright.Height)
	}
	rec := e.get(upright.URL, tendai)
	wantStatus(t, rec, http.StatusOK)
	stored := rec.Body.Bytes()
	if bytes.Contains(stored, []byte("Exif")) || bytes.Contains(stored, []byte("SecretCamera")) {
		t.Fatal("the stored photo kept the upload's metadata")
	}
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(stored)); err != nil || format != "jpeg" || cfg.Width != 900 || cfg.Height != 1600 {
		t.Fatalf("stored: %v %s %v", cfg, format, err)
	}
	// A photo larger than 2048 pixels is stored at 2048 on its longer side.
	got = managedOf(t, e.uploadPhoto(l.ID, testPhoto(t, 3000, 2000), tendai), http.StatusCreated)
	if p := got.Photos[2]; p.Width != 2048 || p.Height != 1365 {
		t.Fatalf("large: %dx%d", p.Width, p.Height)
	}
}

func TestPhotoOrderAndDeletion(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	l := e.completeListing(tendai, "House for the photo order")
	for i := 0; i < 3; i++ {
		l = managedOf(t, e.uploadPhoto(l.ID, testPhoto(t, 1000+i*10, 800), tendai), http.StatusCreated)
	}
	a, b, c := l.Photos[0].ID, l.Photos[1].ID, l.Photos[2].ID
	path := "/v1/me/listings/" + l.ID + "/photos"

	ordered := managedOf(t, e.put(path+"/order", obj{"photo_ids": []string{c, a, b}}, tendai), http.StatusOK)
	if ordered.Photos[0].ID != c || ordered.Photos[1].ID != a || ordered.Photos[2].ID != b || ordered.Photos[0].Position != 0 {
		t.Fatalf("order: %v", ordered.Photos)
	}
	for _, bad := range [][]string{{c, a}, {c, a, a}, {c, a, uuid.NewString()}} {
		wantError(t, e.put(path+"/order", obj{"photo_ids": bad}, tendai), http.StatusBadRequest, httpx.CodeValidationFailed)
	}

	// Deleting a photo removes its stored files.
	_, stored, err := e.listings.PhotoByID(t.Context(), uuid.MustParse(a))
	if err != nil || len(stored.Keys()) != 3 {
		t.Fatalf("stored photo: %+v %v", stored, err)
	}
	for _, key := range stored.Keys() {
		rc, err := e.files.Open(t.Context(), key)
		if err != nil {
			t.Fatalf("object %s: %v", key, err)
		}
		_ = rc.Close()
	}
	after := managedOf(t, e.deleteReq(path+"/"+a, tendai), http.StatusOK)
	if len(after.Photos) != 2 || after.Photos[0].ID != c || after.Photos[1].ID != b {
		t.Fatalf("after deletion: %v", after.Photos)
	}
	for _, key := range stored.Keys() {
		if _, err := e.files.Open(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("object %s still there: %v", key, err)
		}
	}
	wantError(t, e.deleteReq(path+"/"+a, tendai), http.StatusNotFound, listings.CodePhotoNotFound)

	// Deleting a never-published listing removes its photos' files too.
	_, kept, _ := e.listings.PhotoByID(t.Context(), uuid.MustParse(b))
	wantStatus(t, e.deleteReq("/v1/me/listings/"+l.ID, tendai), http.StatusNoContent)
	wantError(t, e.get("/v1/me/listings/"+l.ID, tendai), http.StatusNotFound, listings.CodeListingNotFound)
	for _, key := range kept.Keys() {
		if _, err := e.files.Open(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("object %s of a deleted listing still there: %v", key, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Review decisions
// ---------------------------------------------------------------------------

func TestChangesRequestedAndResubmitted(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	chipo := c.as(devseed.EstatesAgent2Email)
	admin := e.admin()
	id := devseed.PendingListingID.String()

	// The seeded listing waits for review with its bundled photo.
	seeded := managedOf(t, e.get("/v1/me/listings/"+id, chipo), http.StatusOK)
	if seeded.Status != "PENDING_REVIEW" || len(seeded.Photos) != 1 || seeded.Photos[0].Status != "PENDING_REVIEW" ||
		!strings.HasPrefix(seeded.Photos[0].URL, "/assets/images/") {
		t.Fatalf("seeded pending listing: %+v", seeded)
	}
	// A reason is required.
	wantError(t, e.post("/v1/admin/listings/"+id+"/request-changes", obj{"reason": "  "}, browser(admin)), http.StatusBadRequest, httpx.CodeValidationFailed)
	l := managedOf(t, e.post("/v1/admin/listings/"+id+"/request-changes", obj{"reason": "Please add the floor area."}, browser(admin)), http.StatusOK)
	if l.Status != "CHANGES_REQUESTED" || l.Review.Reason == nil || *l.Review.Reason != "Please add the floor area." {
		t.Fatalf("changes requested: %+v", l)
	}
	if events := e.outboxTypes(id); !contains(events, listings.EventListingChangesRequested) {
		t.Fatalf("events: %v", events)
	}
	// The advertiser sees the reason, edits and submits again.
	mine := managedOf(t, e.get("/v1/me/listings/"+id, chipo), http.StatusOK)
	if mine.Review.Reason == nil || !contains(mine.Actions, "edit") || !contains(mine.Actions, "submit") || mine.Timeline[len(mine.Timeline)-1].ActorUserID != nil {
		t.Fatalf("advertiser's view: %+v", mine)
	}
	managedOf(t, e.patch("/v1/me/listings/"+id, obj{"floor_area": 140}, chipo), http.StatusOK)
	l = managedOf(t, e.post("/v1/me/listings/"+id+"/submit", nil, chipo), http.StatusOK)
	if l.Status != "PENDING_REVIEW" {
		t.Fatalf("resubmitted: %s", l.Status)
	}
	l = managedOf(t, e.post("/v1/admin/listings/"+id+"/approve", nil, browser(admin)), http.StatusOK)
	if l.Status != "PUBLISHED" || l.Review.Reason != nil {
		t.Fatalf("approved: %+v", l)
	}
	public := decode[struct{ Listing listings.Listing }](t, e.get("/v1/listings/"+l.Slug)).Listing
	if public.FloorArea == nil || *public.FloorArea != 140 || len(public.Photos) != 1 || public.Photos[0].URL != "/assets/images/listing-townhouse-greendale.jpg" {
		t.Fatalf("public: %+v", public)
	}
}

func TestRejectedListing(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	admin := e.admin()
	l := e.readyListing(tendai, "House that will be rejected")
	managedOf(t, e.post("/v1/me/listings/"+l.ID+"/submit", nil, tendai), http.StatusOK)
	wantError(t, e.post("/v1/admin/listings/"+l.ID+"/reject", obj{}, browser(admin)), http.StatusBadRequest, httpx.CodeValidationFailed)
	r := managedOf(t, e.post("/v1/admin/listings/"+l.ID+"/reject", obj{"reason": "This property is listed twice."}, browser(admin)), http.StatusOK)
	if r.Status != "REJECTED" || *r.Review.Reason != "This property is listed twice." {
		t.Fatalf("rejected: %+v", r)
	}
	if events := e.outboxTypes(l.ID); !contains(events, listings.EventListingRejected) {
		t.Fatalf("events: %v", events)
	}
	// Final: no edit, no submit, no archive (which would let it come back by a relist); delete only.
	mine := managedOf(t, e.get("/v1/me/listings/"+l.ID, tendai), http.StatusOK)
	if strings.Join(mine.Actions, ",") != "delete" {
		t.Fatalf("actions of a rejected listing: %v", mine.Actions)
	}
	wantError(t, e.patch("/v1/me/listings/"+l.ID, obj{"title": "Another try"}, tendai), http.StatusConflict, listings.CodeStateConflict)
	wantError(t, e.post("/v1/me/listings/"+l.ID+"/submit", nil, tendai), http.StatusConflict, listings.CodeStateConflict)
	wantError(t, e.post("/v1/me/listings/"+l.ID+"/archive", nil, tendai), http.StatusConflict, listings.CodeStateConflict)
	wantStatus(t, e.deleteReq("/v1/me/listings/"+l.ID, tendai), http.StatusNoContent)
}

func TestRejectedEditKeepsThePublishedVersion(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	admin := e.admin()
	id := propertyrequests.SeedListingID(1).String()
	managedOf(t, e.patch("/v1/me/listings/"+id, obj{"title": "An unwanted new title"}, tendai), http.StatusOK)
	l := managedOf(t, e.uploadPhoto(id, testPhoto(t, 1200, 900), tendai), http.StatusCreated)
	added := l.Photos[len(l.Photos)-1].ID
	_, stored, _ := e.listings.PhotoByID(t.Context(), uuid.MustParse(added))
	managedOf(t, e.patch("/v1/me/listings/"+id+"/photos/"+added, obj{"alt": "A new photo"}, tendai), http.StatusOK)
	managedOf(t, e.post("/v1/me/listings/"+id+"/submit", nil, tendai), http.StatusOK)
	r := managedOf(t, e.post("/v1/admin/listings/"+id+"/reject", obj{"reason": "The new title is misleading."}, browser(admin)), http.StatusOK)
	if r.Status != "PUBLISHED" || r.PendingChanges == nil || r.PendingChanges.Status != "REJECTED" || r.Content.Title == "An unwanted new title" || len(r.Photos) != 1 {
		t.Fatalf("rejected edit: %+v", r)
	}
	for _, key := range stored.Keys() {
		if _, err := e.files.Open(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("the rejected photo's file %s is still there: %v", key, err)
		}
	}
	if public := decode[struct{ Listing listings.Listing }](t, e.get("/v1/listings/"+r.Slug)).Listing; public.Title == "An unwanted new title" {
		t.Fatal("a rejected edit became public")
	}
	// The last public photo of a published listing stays.
	wantError(t, e.deleteReq("/v1/me/listings/"+id+"/photos/"+r.Photos[0].ID, tendai), http.StatusConflict, listings.CodePhotoRequired)
	// A published listing cannot be deleted.
	wantError(t, e.deleteReq("/v1/me/listings/"+id, tendai), http.StatusConflict, listings.CodeStateConflict)
}

func TestArchiveAndRelist(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	admin := e.admin()
	id := propertyrequests.SeedListingID(1).String()
	l := managedOf(t, e.get("/v1/me/listings/"+id, tendai), http.StatusOK)
	if l.Status != "PUBLISHED" || !l.Public || !contains(l.Actions, "archive") || contains(l.Actions, "delete") {
		t.Fatalf("sample listing: %+v", l)
	}
	a := managedOf(t, e.post("/v1/me/listings/"+id+"/archive", nil, tendai), http.StatusOK)
	if a.Status != "ARCHIVED" || a.Public || !contains(a.Actions, "relist") {
		t.Fatalf("archived: %+v", a)
	}
	if events := e.outboxTypes(id); !contains(events, listings.EventListingUnpublished) {
		t.Fatalf("events: %v", events)
	}
	// Withdrawn: "no longer available", and not found by search.
	wantError(t, e.get("/v1/listings/"+l.Slug), http.StatusGone, listings.CodeListingUnavailable)
	if body := e.get("/v1/listings?page_size=48").Body.String(); strings.Contains(body, `"slug":"`+l.Slug+`"`) {
		t.Fatal("an archived listing is still in the search")
	}
	wantError(t, e.post("/v1/me/listings/"+id+"/archive", nil, tendai), http.StatusConflict, listings.CodeStateConflict)

	// Relist: it was published, so it goes back to review, then a new period.
	r := managedOf(t, e.post("/v1/me/listings/"+id+"/relist", nil, tendai), http.StatusOK)
	if r.Status != "PENDING_REVIEW" {
		t.Fatalf("relisted: %s", r.Status)
	}
	e.now = e.now.Add(48 * time.Hour)
	admin = e.admin() // administrator sessions are short
	p := managedOf(t, e.post("/v1/admin/listings/"+id+"/approve", nil, browser(admin)), http.StatusOK)
	if p.Status != "PUBLISHED" || !p.ExpiresAt.Equal(e.now.Add(listings.PublicationPeriod)) {
		t.Fatalf("approved relist: %+v", p)
	}
	wantStatus(t, e.get("/v1/listings/"+l.Slug), http.StatusOK)

	// An expired listing is renewed the same way.
	owner := newCast(e).as(devseed.EstatesOwnerEmail)
	expired := managedOf(t, e.get("/v1/me/listings/"+devseed.ExpiredListingID.String(), owner), http.StatusOK)
	if expired.Status != "EXPIRED" || !contains(expired.Actions, "relist") {
		t.Fatalf("expired: %+v", expired)
	}

	// A draft that is archived and relisted is a draft again.
	d := e.completeListing(owner, "Draft to archive")
	managedOf(t, e.post("/v1/me/listings/"+d.ID+"/archive", nil, owner), http.StatusOK)
	if back := managedOf(t, e.post("/v1/me/listings/"+d.ID+"/relist", nil, owner), http.StatusOK); back.Status != "DRAFT" {
		t.Fatalf("relisted draft: %s", back.Status)
	}
}

// ---------------------------------------------------------------------------
// Who may
// ---------------------------------------------------------------------------

func TestListingManagementPermissions(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	draft := "/v1/me/listings/" + devseed.DraftListingID.String()
	body := obj{"title": "A new listing", "country": "ZW"}

	// Unauthenticated.
	wantError(t, e.post("/v1/me/listings", body), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.get(draft), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	// A seeker and a professional whose verification is pending manage nothing.
	for _, email := range []string{identity.SeedConsumerEmail, devseed.PendingAgentEmail} {
		wantError(t, e.post("/v1/me/listings", body, c.as(email)), http.StatusForbidden, httpx.CodeForbidden)
		wantError(t, e.get(draft, c.as(email)), http.StatusNotFound, listings.CodeListingNotFound)
	}
	// Another organisation: the same 404 as a listing that does not exist,
	// for reading, changing and the photos.
	other := c.as(devseed.CommercialAgentEmail)
	unknown := wantError(t, e.get("/v1/me/listings/"+uuid.NewString(), other), http.StatusNotFound, listings.CodeListingNotFound)
	foreign := wantError(t, e.get(draft, other), http.StatusNotFound, listings.CodeListingNotFound)
	if unknown.Error.Message != foreign.Error.Message {
		t.Fatal("another organisation's listing must look like an unknown one")
	}
	wantError(t, e.patch(draft, obj{"title": "Mine now"}, other), http.StatusNotFound, listings.CodeListingNotFound)
	wantError(t, e.uploadPhoto(devseed.DraftListingID.String(), testPhoto(t, 1000, 800), other), http.StatusNotFound, listings.CodeListingNotFound)
	wantError(t, e.post("/v1/me/listings", obj{"title": "x", "country": "ZW", "organisation_id": devseed.SampleEstates.ID.String()}, other),
		http.StatusForbidden, httpx.CodeForbidden)

	// STAFF of the agency do not manage listings.
	staff := c.member("staff.member@example.test", devseed.SampleEstates.ID, auth.RoleStaff)
	wantError(t, e.post("/v1/me/listings", body, c.as(staff)), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get(draft, c.as(staff)), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.patch(draft, obj{"title": "Staff edit"}, c.as(staff)), http.StatusForbidden, httpx.CodeForbidden)

	// Another agent of the agency sees Tendai's draft, read-only.
	chipo := c.as(devseed.EstatesAgent2Email)
	seen := managedOf(t, e.get(draft, chipo), http.StatusOK)
	if seen.CanEdit || len(seen.Actions) != 0 {
		t.Fatalf("another agent: can_edit %v, actions %v", seen.CanEdit, seen.Actions)
	}
	wantError(t, e.patch(draft, obj{"title": "Chipo's edit"}, chipo), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.post(draft+"/submit", nil, chipo), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.deleteReq(draft, chipo), http.StatusForbidden, httpx.CodeForbidden)
	// The agency's owner manages every listing of the agency.
	owner := c.as(devseed.EstatesOwnerEmail)
	if l := managedOf(t, e.patch(draft, obj{"title": "Four-bedroom house with a pool and a view"}, owner), http.StatusOK); !l.CanEdit {
		t.Fatal("the owner must be able to edit")
	}
	// An owner's own listing has no agent.
	if mine := managedOf(t, e.post("/v1/me/listings", body, owner), http.StatusCreated); mine.Owner.AgentUserID != nil {
		t.Fatalf("owner's listing has an agent: %v", *mine.Owner.AgentUserID)
	}
	// The approved direct owner lists for themselves.
	seller := managedOf(t, e.post("/v1/me/listings", body, c.as(devseed.SellerEmail)), http.StatusCreated)
	if seller.Owner.Type != "direct_owner" || seller.Owner.OrganisationID != nil {
		t.Fatalf("seller's listing: %+v", seller.Owner)
	}

	// The seeded draft is incomplete in one way: the second photo has no description.
	mine := managedOf(t, e.get(draft, c.as(identity.SeedAgentEmail)), http.StatusOK)
	if mine.Status != "DRAFT" || len(mine.Photos) != 2 || mine.missing() != "photos" || !mine.CanEdit {
		t.Fatalf("seeded draft: %+v", mine)
	}
}

func TestListingReviewAccess(t *testing.T) {
	e := newEnv(t)
	pending := devseed.PendingListingID.String()

	// Not for customers, nor for administrators without a review role.
	wantError(t, e.get("/v1/admin/listings"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.get("/v1/admin/listings", newCast(e).as(identity.SeedAgentEmail)), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get("/v1/admin/listings", withCookie(e.customerScopeAdmin())), http.StatusForbidden, httpx.CodeForbidden)
	finance := e.platformAdmin("finance.listings@example.test", auth.PlatformFinanceAdmin, "")
	wantError(t, e.get("/v1/admin/listings", withCookie(finance)), http.StatusForbidden, httpx.CodeForbidden)

	// A moderator sees every country.
	moderator := e.platformAdmin("moderator.listings@example.test", auth.PlatformModerator, "")
	if q := decode[queueBody](t, e.get("/v1/admin/listings", withCookie(moderator))); !q.has(pending) || q.Total != 1 {
		t.Fatalf("moderator queue: %+v", q)
	}
	// A market administrator reviews their own country only.
	gb := e.platformAdmin("gb.listings@example.test", auth.PlatformMarketAdmin, "GB")
	if q := decode[queueBody](t, e.get("/v1/admin/listings", withCookie(gb))); q.has(pending) || q.Total != 0 {
		t.Fatalf("GB queue: %+v", q)
	}
	wantError(t, e.get("/v1/admin/listings/"+pending, withCookie(gb)), http.StatusNotFound, listings.CodeListingNotFound)
	wantError(t, e.post("/v1/admin/listings/"+pending+"/approve", nil, browser(gb)), http.StatusNotFound, listings.CodeListingNotFound)
	zw := e.platformAdmin("zw.listings@example.test", auth.PlatformMarketAdmin, "ZW")
	if q := decode[queueBody](t, e.get("/v1/admin/listings?country=ZW", withCookie(zw))); !q.has(pending) || q.Items[0].Advertiser.Name != "Sample Estates" {
		t.Fatalf("ZW queue: %+v", q)
	}
	wantError(t, e.get("/v1/admin/listings?status=NOPE", withCookie(zw)), http.StatusBadRequest, httpx.CodeValidationFailed)
	approved := managedOf(t, e.post("/v1/admin/listings/"+pending+"/approve", nil, browser(zw)), http.StatusOK)
	if approved.Status != "PUBLISHED" {
		t.Fatalf("approved by the ZW administrator: %s", approved.Status)
	}
	if q := decode[queueBody](t, e.get("/v1/admin/listings?status=PUBLISHED&q=greendale+shops", withCookie(zw))); !q.has(pending) {
		t.Fatalf("published filter: %+v", q)
	}
}

// The public JSON of a listing keeps the photo fields it always had, for the
// bundled sample photos as for the uploaded ones.
func TestPublicPhotoFieldsAreUnchanged(t *testing.T) {
	e := newEnv(t)
	raw := e.get("/v1/listings/" + e.seeded()[0].Slug).Body.String()
	if !strings.Contains(raw, `"photos":[{"url":"/assets/images/`) || !strings.Contains(raw, `"variants":[]}]`) {
		t.Fatalf("photos: %s", raw)
	}
	for _, key := range []string{`"alt":`, `"width":`, `"height":`} {
		if !strings.Contains(raw, key) {
			t.Fatalf("photo lacks %s: %s", key, raw)
		}
	}
	// Nothing about the storage ever reaches a client.
	rec := e.get("/v1/me/listings/"+propertyrequests.SeedListingID(1).String(), newCast(e).as(identity.SeedAgentEmail))
	if body, _ := io.ReadAll(rec.Body); bytes.Contains(body, []byte(listings.PhotoKeyPrefix+"/")) {
		t.Fatal("an object key leaked")
	}
}
