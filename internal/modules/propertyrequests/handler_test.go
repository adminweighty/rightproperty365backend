package propertyrequests

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/httpx"
)

type fixture struct {
	store   *MemoryStore
	handler http.Handler
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	store := NewMemoryStore()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandler(NewService(store), logger)
	return fixture{store: store, handler: httpx.RequestID(http.HandlerFunc(h.Create))}
}

// validBody returns a fresh, valid request body for seeded listing 1.
func validBody() map[string]any {
	return map[string]any{
		"listing_id":               SeedListingID(1).String(),
		"request_type":             TypeEnquiry,
		"message":                  "Is this property still available? Budget is < 250k.",
		"preferred_contact_method": ContactEmail,
		"idempotency_key":          "test-key-0001",
		"guest_contact": map[string]any{
			"name":  "Tariro Moyo",
			"email": "Tariro@Example.com",
			"phone": "+263771234567",
		},
		"consent": map[string]any{"privacy_version": "2026-01", "accepted": true},
	}
}

func (f fixture) post(t *testing.T, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/property-requests", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return v
}

type successBody struct {
	RequestID string    `json:"request_id"`
	Reference string    `json:"reference"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func TestCreate_Created(t *testing.T) {
	f := newFixture(t)
	rec := f.post(t, validBody(), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	got := decode[successBody](t, rec)
	if _, err := uuid.Parse(got.RequestID); err != nil {
		t.Fatalf("request_id %q is not a UUID", got.RequestID)
	}
	if !strings.HasPrefix(got.Reference, "RP-") || len(got.Reference) != 11 {
		t.Fatalf("reference = %q, want RP-XXXXXXXX", got.Reference)
	}
	if got.Status != StatusReceived || got.CreatedAt.IsZero() {
		t.Fatalf("unexpected body: %+v", got)
	}

	// The request, timeline, activity and outbox records are all persisted.
	reqs := f.store.Requests()
	if len(reqs) != 1 || len(f.store.Timeline()) != 1 || len(f.store.Activity()) != 1 || len(f.store.Outbox()) != 1 {
		t.Fatalf("persisted requests=%d timeline=%d activity=%d outbox=%d, want 1 each",
			len(reqs), len(f.store.Timeline()), len(f.store.Activity()), len(f.store.Outbox()))
	}
	stored := reqs[0]
	if stored.Status != StatusReceived || stored.Recipient == nil || stored.Recipient.Type != RecipientOrganisation {
		t.Fatalf("unexpected routing: status=%s recipient=%+v", stored.Status, stored.Recipient)
	}
	if stored.GuestEmail != "tariro@example.com" {
		t.Fatalf("email not normalised: %q", stored.GuestEmail)
	}
	if out := f.store.Outbox()[0]; out.EventType != EventPropertyRequestCreated || out.AggregateID != stored.ID {
		t.Fatalf("unexpected outbox event: %+v", out)
	}
	if strings.Contains(string(f.store.Outbox()[0].Payload), "tariro") {
		t.Fatal("outbox payload must not contain personal data")
	}
}

func TestCreate_DirectSellerRecipient(t *testing.T) {
	f := newFixture(t)
	body := validBody()
	body["listing_id"] = SeedListingID(6).String()
	if rec := f.post(t, body, nil); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if r := f.store.Requests()[0].Recipient; r == nil || r.Type != RecipientSeller {
		t.Fatalf("recipient = %+v, want SELLER", r)
	}
}

func TestCreate_Idempotency(t *testing.T) {
	f := newFixture(t)

	first := f.post(t, validBody(), nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}

	// Identical content (after normalisation) replays the original response.
	same := validBody()
	same["message"] = "  Is this property still available? Budget is < 250k.  "
	replay := f.post(t, same, nil)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200; body %s", replay.Code, replay.Body)
	}
	if a, b := decode[successBody](t, first), decode[successBody](t, replay); a != b {
		t.Fatalf("replay body differs:\n first %+v\nreplay %+v", a, b)
	}

	// Same key, different content.
	changed := validBody()
	changed["message"] = "A completely different question."
	conflict := f.post(t, changed, nil)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", conflict.Code)
	}
	if env := decode[httpx.ErrorEnvelope](t, conflict); env.Error.Code != CodeIdempotencyConflict || env.TraceID == "" {
		t.Fatalf("unexpected envelope: %+v", env)
	}

	if n := len(f.store.Requests()); n != 1 {
		t.Fatalf("stored %d requests, want exactly 1", n)
	}
	if n := len(f.store.Outbox()); n != 1 {
		t.Fatalf("stored %d outbox events, want exactly 1", n)
	}
}

func TestCreate_IdempotencyKeySources(t *testing.T) {
	f := newFixture(t)

	// Header only.
	body := validBody()
	delete(body, "idempotency_key")
	if rec := f.post(t, body, map[string]string{HeaderIdempotencyKey: "header-key-1"}); rec.Code != http.StatusCreated {
		t.Fatalf("header key: status = %d, want 201; body %s", rec.Code, rec.Body)
	}

	// Body wins when both are present.
	body = validBody()
	body["idempotency_key"] = "body-key-0001"
	if rec := f.post(t, body, map[string]string{HeaderIdempotencyKey: "header-key-1"}); rec.Code != http.StatusCreated {
		t.Fatalf("body key should win and create a new request: status = %d", rec.Code)
	}
	keys := map[string]bool{}
	for _, r := range f.store.Requests() {
		keys[r.IdempotencyKey] = true
	}
	if !keys["header-key-1"] || !keys["body-key-0001"] {
		t.Fatalf("stored keys = %v", keys)
	}
}

func TestCreate_Errors(t *testing.T) {
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name       string
		mutate     func(b map[string]any)
		raw        string // overrides the JSON body when set
		wantStatus int
		wantCode   string
		wantField  string
	}{
		{"bad request type", func(b map[string]any) { b["request_type"] = "OFFER" }, "", 400, httpx.CodeValidationFailed, "request_type"},
		{"html in message", func(b map[string]any) { b["message"] = "Hello <b>there</b>" }, "", 400, httpx.CodeValidationFailed, "message"},
		{"script in message", func(b map[string]any) { b["message"] = "<script>alert(1)</script>" }, "", 400, httpx.CodeValidationFailed, "message"},
		{"empty message", func(b map[string]any) { b["message"] = "   " }, "", 400, httpx.CodeValidationFailed, "message"},
		{"message too long", func(b map[string]any) { b["message"] = strings.Repeat("a", 2001) }, "", 400, httpx.CodeValidationFailed, "message"},
		{"consent not accepted", func(b map[string]any) { b["consent"] = map[string]any{"privacy_version": "2026-01", "accepted": false} }, "", 400, httpx.CodeValidationFailed, "consent.accepted"},
		{"consent missing", func(b map[string]any) { delete(b, "consent") }, "", 400, httpx.CodeValidationFailed, "consent"},
		{"no idempotency key anywhere", func(b map[string]any) { delete(b, "idempotency_key") }, "", 400, httpx.CodeValidationFailed, "idempotency_key"},
		{"short idempotency key", func(b map[string]any) { b["idempotency_key"] = "short" }, "", 400, httpx.CodeValidationFailed, "idempotency_key"},
		{"bad contact method", func(b map[string]any) { b["preferred_contact_method"] = "FAX" }, "", 400, httpx.CodeValidationFailed, "preferred_contact_method"},
		{"bad email", func(b map[string]any) { b["guest_contact"].(map[string]any)["email"] = "not-an-email" }, "", 400, httpx.CodeValidationFailed, "guest_contact.email"},
		{"bad phone", func(b map[string]any) { b["guest_contact"].(map[string]any)["phone"] = "0771234567" }, "", 400, httpx.CodeValidationFailed, "guest_contact.phone"},
		{"whatsapp without phone", func(b map[string]any) {
			b["preferred_contact_method"] = ContactWhatsApp
			delete(b["guest_contact"].(map[string]any), "phone")
		}, "", 400, httpx.CodeValidationFailed, "guest_contact.phone"},
		{"guest contact missing", func(b map[string]any) { delete(b, "guest_contact") }, "", 400, httpx.CodeValidationFailed, "guest_contact"},
		{"listing id not a uuid", func(b map[string]any) { b["listing_id"] = "42" }, "", 400, httpx.CodeValidationFailed, "listing_id"},
		{"slot on plain enquiry", func(b map[string]any) { b["preferred_slot_utc"] = future }, "", 400, httpx.CodeValidationFailed, "preferred_slot_utc"},
		{"slot in the past", func(b map[string]any) {
			b["request_type"] = TypeViewingRequest
			b["preferred_slot_utc"] = past
		}, "", 400, httpx.CodeValidationFailed, "preferred_slot_utc"},
		{"slot not rfc3339", func(b map[string]any) {
			b["request_type"] = TypeViewingRequest
			b["preferred_slot_utc"] = "tomorrow at 10"
		}, "", 400, httpx.CodeValidationFailed, "preferred_slot_utc"},
		{"malformed json", nil, `{"listing_id":`, 400, httpx.CodeInvalidJSON, ""},
		{"unknown field", func(b map[string]any) { b["admin"] = true }, "", 400, httpx.CodeInvalidJSON, ""},
		{"unknown listing", func(b map[string]any) { b["listing_id"] = "0195c0de-0000-7000-8000-00000000dead" }, "", 404, CodeListingNotFound, ""},
		{"expired listing", func(b map[string]any) { b["listing_id"] = SeedExpiredListing.String() }, "", 410, CodeListingUnavailable, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			var body any = tt.raw
			if tt.raw == "" {
				b := validBody()
				tt.mutate(b)
				body = b
			}
			rec := f.post(t, body, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			env := decode[httpx.ErrorEnvelope](t, rec)
			if env.Error.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", env.Error.Code, tt.wantCode)
			}
			if env.Error.Message == "" || env.TraceID == "" || env.TraceID != rec.Header().Get(httpx.HeaderRequestID) {
				t.Fatalf("incomplete envelope: %+v", env)
			}
			if tt.wantField != "" {
				if _, ok := env.Error.Fields[tt.wantField]; !ok {
					t.Fatalf("fields = %v, want an error for %q", env.Error.Fields, tt.wantField)
				}
			}
			if n := len(f.store.Requests()); n != 0 {
				t.Fatalf("a rejected request was stored (%d rows)", n)
			}
		})
	}
}

func TestCreate_ViewingRequestWithSlot(t *testing.T) {
	f := newFixture(t)
	body := validBody()
	body["request_type"] = TypeViewingRequest
	body["preferred_slot_utc"] = time.Now().Add(72 * time.Hour).Format(time.RFC3339)
	if rec := f.post(t, body, nil); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	slot := f.store.Requests()[0].PreferredSlot
	if slot == nil || slot.Location() != time.UTC {
		t.Fatalf("slot = %v, want a UTC time", slot)
	}
}

func TestCreate_UnavailableListings(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	org := SeedOrganisationID
	tests := []struct {
		name    string
		listing Listing
	}{
		{"published but past expires_at", Listing{Status: ListingStatusPublished, ExpiresAt: &past, OrganisationID: &org, OrganisationEligible: true}},
		{"draft", Listing{Status: "DRAFT", OrganisationID: &org, OrganisationEligible: true}},
		{"paused", Listing{Status: "PAUSED", OrganisationID: &org, OrganisationEligible: true}},
		{"sold", Listing{Status: "SOLD", OrganisationID: &org, OrganisationEligible: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.listing.ID = uuid.New()
			tt.listing.MarketID = markets.ZimbabweID
			f.store.PutListing(tt.listing)

			body := validBody()
			body["listing_id"] = tt.listing.ID.String()
			rec := f.post(t, body, nil)
			if rec.Code != http.StatusGone {
				t.Fatalf("status = %d, want 410", rec.Code)
			}
		})
	}
}

func TestCreate_RoutingException(t *testing.T) {
	f := newFixture(t)
	org := SeedOrganisationID
	listing := Listing{
		ID: uuid.New(), MarketID: markets.ZimbabweID, Status: ListingStatusPublished,
		OrganisationID: &org, OrganisationEligible: false, // e.g. suspended organisation, no agent, no seller
	}
	f.store.PutListing(listing)

	body := validBody()
	body["listing_id"] = listing.ID.String()
	rec := f.post(t, body, nil)

	// The customer still gets a normal acknowledgement...
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	if got := decode[successBody](t, rec); got.Status != StatusReceived {
		t.Fatalf("public status = %q, want RECEIVED", got.Status)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "deliver") || strings.Contains(rec.Body.String(), StatusRoutingException) {
		t.Fatalf("response leaks routing state: %s", rec.Body)
	}

	// ...while the request is recorded internally as a routing exception.
	stored := f.store.Requests()[0]
	if stored.Status != StatusRoutingException || stored.Recipient != nil {
		t.Fatalf("stored status=%s recipient=%+v, want ROUTING_EXCEPTION with no recipient", stored.Status, stored.Recipient)
	}
	timeline := f.store.Timeline()
	if len(timeline) != 2 || timeline[1].EventType != TimelineRoutingException {
		t.Fatalf("timeline = %+v, want REQUEST_RECEIVED then ROUTING_EXCEPTION_RAISED", timeline)
	}
	if len(f.store.Outbox()) != 1 {
		t.Fatal("routing exceptions must still emit an outbox event")
	}

	// A replay still returns 200/RECEIVED.
	if replay := f.post(t, body, nil); replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", replay.Code)
	}
}

func TestCreate_UnsupportedMediaType(t *testing.T) {
	f := newFixture(t)
	rec := f.post(t, validBody(), map[string]string{"Content-Type": "text/plain"})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
}
