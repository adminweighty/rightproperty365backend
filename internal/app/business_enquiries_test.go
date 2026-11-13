package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/httpx"
)

const (
	plumbersEmail     = "sample-plumbers@example.test"
	electriciansEmail = "sample-electricians@example.test"
)

// enquiry returns a fresh, valid guest enquiry to the given profile.
func (e *env) enquiry(kind, slug string) obj {
	e.seq++
	return obj{
		"recipient":                obj{"kind": kind, "slug": slug},
		"service":                  "Burst pipes and leaks",
		"message":                  fmt.Sprintf("Enquiry %d: the geyser is leaking, can you help? Budget < 300.", e.seq),
		"preferred_contact_method": "EMAIL",
		"idempotency_key":          fmt.Sprintf("enquiry-key-%04d", e.seq),
		"guest_contact":            obj{"name": "Tariro Moyo", "email": "Tariro@Example.com", "phone": "+263771234567"},
		"consent":                  obj{"privacy_version": "2026-01", "accepted": true},
	}
}

type enquiryReceipt struct {
	EnquiryID string    `json:"enquiry_id"`
	Reference string    `json:"reference"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type enquiryInboxBody struct {
	Items []struct {
		EnquiryID string `json:"enquiry_id"`
		Reference string `json:"reference"`
		Recipient struct {
			Kind string `json:"kind"`
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"recipient"`
		Service                *string `json:"service"`
		Message                string  `json:"message"`
		PreferredContactMethod string  `json:"preferred_contact_method"`
		Contact                struct {
			Name  string  `json:"name"`
			Email string  `json:"email"`
			Phone *string `json:"phone"`
		} `json:"contact"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"items"`
}

// enquiryInbox returns the "kind/slug" of every enquiry in the caller's inbox.
func (e *env) enquiryInbox(email string) []string {
	e.t.Helper()
	// A fresh client IP per login keeps helpers clear of the auth rate limit.
	e.seq++
	login := e.post("/v1/auth/login", obj{"email": email, "password": seedPassword, "token_delivery": "bearer"},
		withIP(fmt.Sprintf("192.0.2.%d", 1+e.seq%90)))
	wantStatus(e.t, login, http.StatusOK)
	rec := e.get("/v1/inbox/business-enquiries", withBearer(decode[authBody](e.t, login).AccessToken))
	wantStatus(e.t, rec, http.StatusOK)
	got := []string{}
	for _, it := range decode[enquiryInboxBody](e.t, rec).Items {
		got = append(got, it.Recipient.Kind+"/"+it.Recipient.Slug)
	}
	sort.Strings(got)
	return got
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBusinessEnquiryCreated(t *testing.T) {
	e := newEnv(t)
	body := e.enquiry("service-providers", "sample-plumbers")
	rec := e.post("/v1/business-enquiries", body)
	wantStatus(t, rec, http.StatusCreated)

	got := decode[enquiryReceipt](t, rec)
	if _, err := uuid.Parse(got.EnquiryID); err != nil {
		t.Fatalf("enquiry_id %q is not a UUID", got.EnquiryID)
	}
	if !regexp.MustCompile(`^BE-[2-9A-HJKMNP-TV-Z]{8}$`).MatchString(got.Reference) {
		t.Fatalf("reference = %q, want BE-XXXXXXXX", got.Reference)
	}
	if got.Status != "RECEIVED" || !got.CreatedAt.Equal(e.now) {
		t.Fatalf("unexpected receipt: %+v", got)
	}
	// The receipt is exactly these four fields: nothing about the recipient
	// or the customer is echoed, and nothing claims delivery.
	if want := fmt.Sprintf(`{"enquiry_id":"%s","reference":"%s","status":"RECEIVED","created_at":"2026-11-01T09:00:00Z"}`, got.EnquiryID, got.Reference); strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("body = %s\nwant   %s", rec.Body, want)
	}

	// The enquiry, timeline, activity and outbox records are all persisted.
	enqs := e.enquiries.Enquiries()
	if len(enqs) != 1 || len(e.enquiries.Timeline()) != 1 || len(e.enquiries.Activity()) != 1 || len(e.enquiries.Outbox()) != 1 {
		t.Fatalf("persisted enquiries=%d timeline=%d activity=%d outbox=%d, want 1 each",
			len(enqs), len(e.enquiries.Timeline()), len(e.enquiries.Activity()), len(e.enquiries.Outbox()))
	}
	stored := enqs[0]
	if stored.Status != businessenquiries.StatusReceived || stored.Recipient == nil || stored.Recipient.UserID != nil {
		t.Fatalf("unexpected routing: status=%s recipient=%+v", stored.Status, stored.Recipient)
	}
	if stored.GuestEmail != "tariro@example.com" || stored.GuestName != "Tariro Moyo" || stored.GuestPhone != "+263771234567" ||
		stored.Service != "Burst pipes and leaks" || stored.CustomerUserID != nil || stored.PrivacyVersion != "2026-01" {
		t.Fatalf("unexpected stored enquiry: %+v", stored)
	}
	if tl := e.enquiries.Timeline()[0]; tl.EventType != businessenquiries.TimelineEnquiryReceived || tl.EnquiryID != stored.ID {
		t.Fatalf("unexpected timeline: %+v", tl)
	}
	if act := e.enquiries.Activity()[0]; act.ActivityType != businessenquiries.ActivityEnquirySubmitted || act.GuestEmail != "tariro@example.com" || act.UserID != nil {
		t.Fatalf("unexpected activity: %+v", act)
	}

	// Outbox: BUSINESS_ENQUIRY_CREATED with identifiers only.
	out := e.enquiries.Outbox()[0]
	if out.EventType != "BUSINESS_ENQUIRY_CREATED" || out.AggregateID != stored.ID {
		t.Fatalf("unexpected outbox event: %+v", out)
	}
	payload := strings.ToLower(string(out.Payload))
	for _, personal := range []string{"tariro", "moyo", "example.com", "263771234567", "geyser", "burst pipes", "guest", "contact", "email", "phone", "name", "sample-plumbers"} {
		if strings.Contains(payload, personal) {
			t.Fatalf("outbox payload must carry identifiers only, found %q in %s", personal, out.Payload)
		}
	}
	if !strings.Contains(payload, stored.ID.String()) || !strings.Contains(payload, strings.ToLower(got.Reference)) {
		t.Fatalf("outbox payload lacks the enquiry identifiers: %s", out.Payload)
	}

	// Logs carry identifiers only as well.
	logs := strings.ToLower(e.logs.String())
	for _, personal := range []string{"tariro", "263771234567", "geyser"} {
		if strings.Contains(logs, personal) {
			t.Fatalf("logs contain personal data (%q)", personal)
		}
	}
	if !strings.Contains(logs, stored.ID.String()) {
		t.Fatal("the acceptance log line should carry the enquiry id")
	}
}

func TestBusinessEnquiryIdempotency(t *testing.T) {
	e := newEnv(t)
	body := e.enquiry("service-providers", "sample-plumbers")
	first := e.post("/v1/business-enquiries", body)
	wantStatus(t, first, http.StatusCreated)

	// Identical content (after normalisation) replays the original response.
	same := obj{}
	for k, v := range body {
		same[k] = v
	}
	same["message"] = "  " + body["message"].(string) + "  "
	same["guest_contact"] = obj{"name": " Tariro Moyo ", "email": "tariro@example.com", "phone": "+263771234567"}
	replay := e.post("/v1/business-enquiries", same)
	wantStatus(t, replay, http.StatusOK)
	if a, b := decode[enquiryReceipt](t, first), decode[enquiryReceipt](t, replay); a != b {
		t.Fatalf("replay differs:\n first %+v\nreplay %+v", a, b)
	}

	// Same key, different content.
	for name, mutate := range map[string]func(obj){
		"different message":   func(b obj) { b["message"] = "A completely different question." },
		"different recipient": func(b obj) { b["recipient"] = obj{"kind": "service-providers", "slug": "sample-electricians"} },
		"different service":   func(b obj) { b["service"] = "Geysers" },
	} {
		changed := obj{}
		for k, v := range body {
			changed[k] = v
		}
		mutate(changed)
		rec := e.post("/v1/business-enquiries", changed)
		wantError(t, rec, http.StatusConflict, businessenquiries.CodeIdempotencyConflict)
		_ = name
	}
	if n := len(e.enquiries.Enquiries()); n != 1 || len(e.enquiries.Outbox()) != 1 {
		t.Fatalf("stored %d enquiries and %d outbox events, want exactly 1 each", n, len(e.enquiries.Outbox()))
	}

	t.Run("key sources: header only, and the body wins over the header", func(t *testing.T) {
		b := e.enquiry("service-providers", "sample-plumbers")
		delete(b, "idempotency_key")
		wantStatus(t, e.post("/v1/business-enquiries", b, withHeader("Idempotency-Key", "header-key-0001")), http.StatusCreated)
		wantStatus(t, e.post("/v1/business-enquiries", b, withHeader("Idempotency-Key", "header-key-0001")), http.StatusOK)

		b["idempotency_key"] = "body-key-0001"
		wantStatus(t, e.post("/v1/business-enquiries", b, withHeader("Idempotency-Key", "header-key-0001")), http.StatusCreated)
		if n := len(e.enquiries.Enquiries()); n != 3 {
			t.Fatalf("stored %d enquiries, want 3", n)
		}
	})

	t.Run("a property-request key does not collide with an enquiry key", func(t *testing.T) {
		pr := e.propertyRequest(1)
		pr["idempotency_key"] = "shared-key-0001"
		wantStatus(t, e.post("/v1/property-requests", pr), http.StatusCreated)
		b := e.enquiry("service-providers", "sample-plumbers")
		b["idempotency_key"] = "shared-key-0001"
		wantStatus(t, e.post("/v1/business-enquiries", b), http.StatusCreated)
	})
}

func TestBusinessEnquiryValidation(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(obj)
		wantFields []string
	}{
		{"recipient missing", func(b obj) { delete(b, "recipient") }, []string{"recipient.kind", "recipient.slug"}},
		{"recipient kind missing", func(b obj) { b["recipient"] = obj{"slug": "sample-plumbers"} }, []string{"recipient.kind"}},
		{"recipient kind unknown", func(b obj) { b["recipient"] = obj{"kind": "plumbers", "slug": "sample-plumbers"} }, []string{"recipient.kind"}},
		{"recipient kind wrong case", func(b obj) { b["recipient"] = obj{"kind": "Service-Providers", "slug": "sample-plumbers"} }, []string{"recipient.kind"}},
		{"recipient slug missing", func(b obj) { b["recipient"] = obj{"kind": "service-providers"} }, []string{"recipient.slug"}},
		{"recipient slug malformed", func(b obj) { b["recipient"] = obj{"kind": "service-providers", "slug": "../admin"} }, []string{"recipient.slug"}},
		{"service too long", func(b obj) { b["service"] = strings.Repeat("s", 121) }, []string{"service"}},
		{"service with HTML", func(b obj) { b["service"] = "<b>Geysers</b>" }, []string{"service"}},
		{"service with control characters", func(b obj) { b["service"] = "Geysers\nand more" }, []string{"service"}},
		{"message missing", func(b obj) { delete(b, "message") }, []string{"message"}},
		{"message blank", func(b obj) { b["message"] = "   " }, []string{"message"}},
		{"message too long", func(b obj) { b["message"] = strings.Repeat("m", 2001) }, []string{"message"}},
		{"message with HTML", func(b obj) { b["message"] = `Hello <script>alert(1)</script>` }, []string{"message"}},
		{"contact method missing", func(b obj) { delete(b, "preferred_contact_method") }, []string{"preferred_contact_method"}},
		{"contact method unknown", func(b obj) { b["preferred_contact_method"] = "SMS" }, []string{"preferred_contact_method"}},
		{"guest contact missing", func(b obj) { delete(b, "guest_contact") }, []string{"guest_contact.name", "guest_contact.email"}},
		{"guest name missing", func(b obj) { b["guest_contact"] = obj{"email": "t@example.com"} }, []string{"guest_contact.name"}},
		{"guest name with HTML", func(b obj) { b["guest_contact"] = obj{"name": "<i>T</i>", "email": "t@example.com"} }, []string{"guest_contact.name"}},
		{"guest email missing", func(b obj) { b["guest_contact"] = obj{"name": "T"} }, []string{"guest_contact.email"}},
		{"guest email invalid", func(b obj) { b["guest_contact"] = obj{"name": "T", "email": "nope"} }, []string{"guest_contact.email"}},
		{"guest phone invalid", func(b obj) { b["guest_contact"] = obj{"name": "T", "email": "t@example.com", "phone": "0771234567"} }, []string{"guest_contact.phone"}},
		{"phone required for PHONE", func(b obj) {
			b["preferred_contact_method"] = "PHONE"
			b["guest_contact"] = obj{"name": "T", "email": "t@example.com"}
		}, []string{"guest_contact.phone"}},
		{"phone required for WHATSAPP", func(b obj) {
			b["preferred_contact_method"] = "WHATSAPP"
			b["guest_contact"] = obj{"name": "T", "email": "t@example.com"}
		}, []string{"guest_contact.phone"}},
		{"consent missing", func(b obj) { delete(b, "consent") }, []string{"consent.accepted"}},
		{"consent not accepted", func(b obj) { b["consent"] = obj{"privacy_version": "2026-01", "accepted": false} }, []string{"consent.accepted"}},
		{"idempotency key missing", func(b obj) { delete(b, "idempotency_key") }, []string{"idempotency_key"}},
		{"idempotency key too short", func(b obj) { b["idempotency_key"] = "short" }, []string{"idempotency_key"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			body := e.enquiry("service-providers", "sample-plumbers")
			tt.mutate(body)
			env := wantError(t, e.post("/v1/business-enquiries", body), http.StatusBadRequest, httpx.CodeValidationFailed)
			if len(env.Error.Fields) != len(tt.wantFields) {
				t.Fatalf("fields = %v, want exactly %v", env.Error.Fields, tt.wantFields)
			}
			for _, f := range tt.wantFields {
				if env.Error.Fields[f] == "" {
					t.Fatalf("fields = %v, want %v", env.Error.Fields, tt.wantFields)
				}
			}
			if len(e.enquiries.Enquiries()) != 0 || len(e.enquiries.Outbox()) != 0 {
				t.Fatal("an invalid enquiry was stored")
			}
		})
	}

	t.Run("optional and boundary values are accepted", func(t *testing.T) {
		e := newEnv(t)
		for _, mutate := range []func(obj){
			func(b obj) { delete(b, "service") },
			func(b obj) { b["service"] = "" },
			func(b obj) { b["service"] = strings.Repeat("s", 120) },
			func(b obj) { b["message"] = strings.Repeat("m", 2000) },
			func(b obj) { b["message"] = "Two lines\nand 3 < 4" },
			func(b obj) { b["guest_contact"] = obj{"name": "T", "email": "t@example.com"} },
			func(b obj) { b["preferred_contact_method"] = "WHATSAPP" },
		} {
			body := e.enquiry("service-providers", "sample-plumbers")
			mutate(body)
			wantStatus(t, e.post("/v1/business-enquiries", body), http.StatusCreated)
		}
		enqs := e.enquiries.Enquiries()
		if enqs[0].Service != "" || enqs[1].Service != "" {
			t.Fatalf("an omitted or empty service must be stored as none: %q %q", enqs[0].Service, enqs[1].Service)
		}
	})

	t.Run("strict JSON, content type and body size still apply", func(t *testing.T) {
		e := newEnv(t)
		body := e.enquiry("service-providers", "sample-plumbers")
		body["recipient_user_id"] = uuid.NewString()
		wantError(t, e.post("/v1/business-enquiries", body), http.StatusBadRequest, httpx.CodeInvalidJSON)

		ok := e.enquiry("service-providers", "sample-plumbers")
		wantError(t, e.post("/v1/business-enquiries", ok, withHeader("Content-Type", "text/plain")), http.StatusUnsupportedMediaType, httpx.CodeUnsupportedMediaType)

		ok["message"] = strings.Repeat("x", 70<<10)
		wantError(t, e.post("/v1/business-enquiries", ok), http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge)
		if len(e.enquiries.Enquiries()) != 0 {
			t.Fatal("a rejected enquiry was stored")
		}
	})
}

func TestBusinessEnquiryRecipientNotFoundOrUnavailable(t *testing.T) {
	e := newEnv(t)

	t.Run("unknown recipient is 404", func(t *testing.T) {
		for _, r := range []obj{
			{"kind": "service-providers", "slug": "no-such-business"},
			{"kind": "hosts", "slug": "sample-plumbers"}, // right slug, wrong kind
			{"kind": "agents", "slug": "sample-estates"},
		} {
			body := e.enquiry("service-providers", "sample-plumbers")
			body["recipient"] = r
			wantError(t, e.post("/v1/business-enquiries", body), http.StatusNotFound, businessenquiries.CodeRecipientNotFound)
		}
	})

	t.Run("a profile that is not ACTIVE is 410", func(t *testing.T) {
		org := devseed.Organisations()[0].ID
		for slug, status := range map[string]string{
			"suspended-business":  businessenquiries.ProfileSuspended,
			"unverified-business": businessenquiries.ProfilePendingVerification,
		} {
			e.enquiries.PutProfile(businessenquiries.Profile{
				ID: uuid.New(), Kind: businessenquiries.KindServiceProviders, Slug: slug, Name: slug,
				MarketID: markets.ZimbabweID, Status: status, OrganisationID: org, OrganisationEligible: true,
			})
			wantError(t, e.post("/v1/business-enquiries", e.enquiry("service-providers", slug)), http.StatusGone, businessenquiries.CodeRecipientUnavailable)
		}
	})

	if len(e.enquiries.Enquiries()) != 0 {
		t.Fatal("an enquiry to a missing or unavailable profile was stored")
	}

	t.Run("a retry still replays after the profile is suspended", func(t *testing.T) {
		body := e.enquiry("hosts", "sample-showrooms")
		wantStatus(t, e.post("/v1/business-enquiries", body), http.StatusCreated)
		p, _ := e.enquiries.GetProfile(t.Context(), "hosts", "sample-showrooms")
		p.Status = businessenquiries.ProfileSuspended
		e.enquiries.PutProfile(p)
		wantStatus(t, e.post("/v1/business-enquiries", body), http.StatusOK)
		wantError(t, e.post("/v1/business-enquiries", e.enquiry("hosts", "sample-showrooms")), http.StatusGone, businessenquiries.CodeRecipientUnavailable)
	})
}

func TestAuthenticatedBusinessEnquiry(t *testing.T) {
	e := newEnv(t)
	cookie := e.loginCookie(identity.SeedConsumerEmail, seedPassword)

	t.Run("without guest_contact: the account is used and the user id recorded", func(t *testing.T) {
		body := e.enquiry("service-providers", "sample-plumbers")
		delete(body, "guest_contact")
		wantStatus(t, e.post("/v1/business-enquiries", body, browser(cookie)), http.StatusCreated)

		enqs := e.enquiries.Enquiries()
		stored := enqs[len(enqs)-1]
		if stored.CustomerUserID == nil || *stored.CustomerUserID != identity.SeedConsumerUserID {
			t.Fatalf("customer user id = %v, want %s", stored.CustomerUserID, identity.SeedConsumerUserID)
		}
		if stored.GuestName != "Casey Consumer" || stored.GuestEmail != identity.SeedConsumerEmail || stored.GuestPhone != "" {
			t.Fatalf("contact snapshot = %q %q %q", stored.GuestName, stored.GuestEmail, stored.GuestPhone)
		}
		act := e.enquiries.Activity()
		if last := act[len(act)-1]; last.UserID == nil || *last.UserID != identity.SeedConsumerUserID || last.GuestEmail != "" {
			t.Fatalf("activity not attributed to the account: %+v", last)
		}

		// Replay for the same customer; conflict for anyone else, including a guest.
		wantStatus(t, e.post("/v1/business-enquiries", body, browser(cookie)), http.StatusOK)
		wantError(t, e.post("/v1/business-enquiries", body, withBearer(e.loginBearer(identity.SeedAgentEmail, seedPassword))),
			http.StatusConflict, businessenquiries.CodeIdempotencyConflict)
	})

	t.Run("identity in the body is ignored; only the phone is read", func(t *testing.T) {
		body := e.enquiry("service-providers", "sample-plumbers")
		body["guest_contact"] = obj{"name": "Someone Else", "email": "spoof@example.com", "phone": "+263771234567"}
		wantStatus(t, e.post("/v1/business-enquiries", body, browser(cookie)), http.StatusCreated)
		enqs := e.enquiries.Enquiries()
		stored := enqs[len(enqs)-1]
		if stored.GuestEmail != identity.SeedConsumerEmail || stored.GuestName != "Casey Consumer" || stored.GuestPhone != "+263771234567" {
			t.Fatalf("contact = %q %q %q", stored.GuestName, stored.GuestEmail, stored.GuestPhone)
		}
	})

	t.Run("phone still required for PHONE and WHATSAPP", func(t *testing.T) {
		for _, method := range []string{"PHONE", "WHATSAPP"} {
			body := e.enquiry("service-providers", "sample-plumbers")
			delete(body, "guest_contact")
			body["preferred_contact_method"] = method
			env := wantError(t, e.post("/v1/business-enquiries", body, browser(cookie)), http.StatusBadRequest, httpx.CodeValidationFailed)
			if _, ok := env.Error.Fields["guest_contact.phone"]; !ok || len(env.Error.Fields) != 1 {
				t.Fatalf("fields = %v, want only guest_contact.phone", env.Error.Fields)
			}
			body["guest_contact"] = obj{"phone": "+263771234567"}
			wantStatus(t, e.post("/v1/business-enquiries", body, browser(cookie)), http.StatusCreated)
		}
	})

	t.Run("consent still required", func(t *testing.T) {
		body := e.enquiry("service-providers", "sample-plumbers")
		delete(body, "guest_contact")
		delete(body, "consent")
		env := wantError(t, e.post("/v1/business-enquiries", body, browser(cookie)), http.StatusBadRequest, httpx.CodeValidationFailed)
		if _, ok := env.Error.Fields["consent.accepted"]; !ok {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
	})

	t.Run("CSRF rules as for property requests", func(t *testing.T) {
		before := len(e.enquiries.Enquiries())
		wantError(t, e.post("/v1/business-enquiries", e.enquiry("hosts", "sample-showrooms"), withCookie(cookie)), http.StatusForbidden, httpx.CodeCSRFOrigin)
		wantError(t, e.post("/v1/business-enquiries", e.enquiry("hosts", "sample-showrooms"), withCookie(cookie), withOrigin("https://evil.example")), http.StatusForbidden, httpx.CodeCSRFOrigin)
		if len(e.enquiries.Enquiries()) != before {
			t.Fatal("an enquiry rejected for CSRF was stored")
		}
		wantStatus(t, e.post("/v1/business-enquiries", e.enquiry("hosts", "sample-showrooms"), withBearer(e.loginBearer(identity.SeedConsumerEmail, seedPassword))), http.StatusCreated)
	})
}

func TestBusinessEnquiryRoutingToAnOrganisation(t *testing.T) {
	e := newEnv(t)
	first := e.enquiry("service-providers", "sample-plumbers")
	wantStatus(t, e.post("/v1/business-enquiries", first), http.StatusCreated)
	e.now = e.now.Add(time.Minute)
	second := e.enquiry("service-providers", "sample-plumbers")
	delete(second, "service")
	second["guest_contact"] = obj{"name": "Farai Dube", "email": "farai@example.com"}
	wantStatus(t, e.post("/v1/business-enquiries", second), http.StatusCreated)

	t.Run("the owning organisation sees them, newest first, with the contact details", func(t *testing.T) {
		rec := e.get("/v1/inbox/business-enquiries", withCookie(e.loginCookie(plumbersEmail, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		inbox := decode[enquiryInboxBody](t, rec)
		if len(inbox.Items) != 2 {
			t.Fatalf("items = %d, want 2: %s", len(inbox.Items), rec.Body)
		}
		newest, oldest := inbox.Items[0], inbox.Items[1]
		for _, it := range inbox.Items {
			if _, err := uuid.Parse(it.EnquiryID); err != nil || !strings.HasPrefix(it.Reference, "BE-") || it.CreatedAt.IsZero() ||
				it.Status != "RECEIVED" || it.PreferredContactMethod != "EMAIL" ||
				it.Recipient.Kind != "service-providers" || it.Recipient.Slug != "sample-plumbers" || it.Recipient.Name != "Sample Plumbers" {
				t.Fatalf("unexpected item: %+v", it)
			}
		}
		if oldest.Message != first["message"] || oldest.Service == nil || *oldest.Service != "Burst pipes and leaks" ||
			oldest.Contact.Name != "Tariro Moyo" || oldest.Contact.Email != "tariro@example.com" ||
			oldest.Contact.Phone == nil || *oldest.Contact.Phone != "+263771234567" {
			t.Fatalf("unexpected oldest item: %+v", oldest)
		}
		if newest.Service != nil || newest.Contact.Name != "Farai Dube" || newest.Contact.Phone != nil || !newest.CreatedAt.After(oldest.CreatedAt) {
			t.Fatalf("unexpected newest item: %+v", newest)
		}
		if !strings.Contains(rec.Body.String(), `"service":null`) || !strings.Contains(rec.Body.String(), `"phone":null`) {
			t.Fatalf("absent service and phone must be null: %s", rec.Body)
		}
	})

	t.Run("another organisation gets nothing", func(t *testing.T) {
		rec := e.get("/v1/inbox/business-enquiries", withCookie(e.loginCookie(electriciansEmail, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[]}` {
			t.Fatalf("body = %s, want an empty items array", got)
		}
		for _, email := range []string{"sample-rapid-plumbing@example.test", devseed.EstatesOwnerEmail, identity.SeedAgentEmail, identity.SeedAgentBEmail} {
			if got := e.enquiryInbox(email); len(got) != 0 {
				t.Fatalf("%s saw %v", email, got)
			}
		}
	})

	t.Run("scope cannot be widened by the client", func(t *testing.T) {
		plumbersOrg := ""
		for _, p := range devseed.Profiles() {
			if p.Slug == "sample-plumbers" {
				plumbersOrg = p.Organisation.ID.String()
			}
		}
		rec := e.get("/v1/inbox/business-enquiries?organisation_id="+plumbersOrg+"&slug=sample-plumbers",
			withBearer(e.loginBearer(electriciansEmail, seedPassword)), withHeader("X-Organisation-Id", plumbersOrg))
		wantStatus(t, rec, http.StatusOK)
		if len(decode[enquiryInboxBody](t, rec).Items) != 0 {
			t.Fatal("another organisation read Sample Plumbers' enquiries")
		}
	})
}

func TestBusinessEnquiryRoutingToAnAgent(t *testing.T) {
	e := newEnv(t)
	for _, r := range [][2]string{
		{"agents", "tendai-ncube"},        // an AGENT of Sample Estates
		{"agents", "rudo-moyo"},           // the OWNER of Sample Estates
		{"agencies", "sample-estates"},    // the agency itself
		{"agents", "farai-chikwanha"},     // another agency
		{"agencies", "sample-commercial"}, // another agency
	} {
		wantStatus(t, e.post("/v1/business-enquiries", e.enquiry(r[0], r[1])), http.StatusCreated)
	}

	enqs := e.enquiries.Enquiries()
	if r := enqs[0].Recipient; r == nil || r.UserID == nil || *r.UserID != identity.SeedAgentUserID || r.OrganisationID != devseed.SampleEstates.ID {
		t.Fatalf("an agent enquiry must route to the agent within the agency: %+v", r)
	}
	if r := enqs[2].Recipient; r == nil || r.UserID != nil || r.OrganisationID != devseed.SampleEstates.ID {
		t.Fatalf("an agency enquiry must route to the organisation: %+v", r)
	}

	tests := []struct {
		who, email string
		want       []string
	}{
		{"Tendai (AGENT) sees only what is addressed to his own profile", identity.SeedAgentEmail, []string{"agents/tendai-ncube"}},
		{"Chipo (another AGENT of the same agency) sees none of it", devseed.EstatesAgent2Email, nil},
		{"Rudo (OWNER) sees everything routed to the agency, including its agents' profiles", devseed.EstatesOwnerEmail,
			[]string{"agencies/sample-estates", "agents/rudo-moyo", "agents/tendai-ncube"}},
		{"Farai (OWNER of another agency) sees only his agency's", devseed.CommercialAgentEmail,
			[]string{"agencies/sample-commercial", "agents/farai-chikwanha"}},
		{"Nomsa (OWNER of a third agency) sees nothing", devseed.LandAgentEmail, nil},
		{"Blake (AGENT elsewhere) sees nothing", identity.SeedAgentBEmail, nil},
		{"Sample Plumbers' owner sees nothing", plumbersEmail, nil},
	}
	for _, tt := range tests {
		t.Run(tt.who, func(t *testing.T) {
			want := append([]string{}, tt.want...)
			sort.Strings(want)
			if got := e.enquiryInbox(tt.email); !sameStrings(got, want) {
				t.Fatalf("inbox = %v, want %v", got, want)
			}
		})
	}

	t.Run("STAFF and MANAGER", func(t *testing.T) {
		hash, err := identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}.Hash(seedPassword)
		if err != nil {
			t.Fatal(err)
		}
		for email, role := range map[string]string{"estates-staff@example.com": "STAFF", "estates-manager@example.com": "MANAGER"} {
			id, verified := uuid.New(), e.now
			if err := e.identity.CreateUser(t.Context(), identity.NewUser{ID: id, Name: email, Email: email, PasswordHash: hash, EmailVerifiedAt: &verified, CreatedAt: e.now}); err != nil {
				t.Fatal(err)
			}
			if err := e.identity.AddMembership(id, devseed.SampleEstates.ID, "ZW", role); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.enquiryInbox("estates-staff@example.com"); len(got) != 0 {
			t.Fatalf("STAFF without an agent profile saw %v", got)
		}
		if got := e.enquiryInbox("estates-manager@example.com"); len(got) != 3 {
			t.Fatalf("MANAGER saw %v, want all three of the agency's", got)
		}
	})
}

func TestBusinessEnquiryRoutingException(t *testing.T) {
	e := newEnv(t)
	plumbers, err := e.enquiries.GetProfile(t.Context(), "service-providers", "sample-plumbers")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("nobody can receive it: stored as ROUTING_EXCEPTION, customer still gets 201 RECEIVED", func(t *testing.T) {
		orphan := plumbers
		orphan.ID, orphan.Slug, orphan.Name, orphan.OrganisationEligible = uuid.New(), "orphaned-business", "Orphaned Business", false
		e.enquiries.PutProfile(orphan)

		rec := e.post("/v1/business-enquiries", e.enquiry("service-providers", "orphaned-business"))
		wantStatus(t, rec, http.StatusCreated)
		if got := decode[enquiryReceipt](t, rec); got.Status != "RECEIVED" || !strings.HasPrefix(got.Reference, "BE-") {
			t.Fatalf("unexpected receipt: %+v", got)
		}
		enqs := e.enquiries.Enquiries()
		stored := enqs[len(enqs)-1]
		if stored.Status != businessenquiries.StatusRoutingException || stored.Recipient != nil {
			t.Fatalf("stored status=%s recipient=%+v, want ROUTING_EXCEPTION and no recipient", stored.Status, stored.Recipient)
		}
		tl := e.enquiries.Timeline()
		if len(tl) != 2 || tl[1].EventType != businessenquiries.TimelineRoutingException || tl[1].ActorType != businessenquiries.ActorSystem {
			t.Fatalf("timeline = %+v, want the routing exception recorded", tl)
		}
		if out := e.enquiries.Outbox(); len(out) != 1 || !strings.Contains(string(out[0].Payload), `"status":"ROUTING_EXCEPTION"`) ||
			strings.Contains(string(out[0].Payload), "recipient_organisation_id") {
			t.Fatalf("unexpected outbox: %s", out[0].Payload)
		}
		// It is in nobody's inbox, not even the organisation named on the profile.
		if got := e.enquiryInbox(plumbersEmail); len(got) != 0 {
			t.Fatalf("a routing exception appeared in an inbox: %v", got)
		}
	})

	t.Run("an agent who cannot receive it falls back to the agency", func(t *testing.T) {
		gone := identity.SeedAgentUserID
		e.enquiries.PutProfile(businessenquiries.Profile{
			ID: uuid.New(), Kind: businessenquiries.KindAgents, Slug: "departed-agent", Name: "Departed Agent",
			MarketID: markets.ZimbabweID, Status: businessenquiries.ProfileActive,
			OrganisationID: devseed.SampleEstates.ID, OrganisationEligible: true,
			AgentUserID: &gone, AgentEligible: false,
		})
		wantStatus(t, e.post("/v1/business-enquiries", e.enquiry("agents", "departed-agent")), http.StatusCreated)
		enqs := e.enquiries.Enquiries()
		if stored := enqs[len(enqs)-1]; stored.Status != businessenquiries.StatusReceived || stored.Recipient == nil || stored.Recipient.UserID != nil {
			t.Fatalf("unexpected routing: %s %+v", stored.Status, stored.Recipient)
		}
		if got := e.enquiryInbox(devseed.EstatesOwnerEmail); !sameStrings(got, []string{"agents/departed-agent"}) {
			t.Fatalf("owner inbox = %v", got)
		}
		if got := e.enquiryInbox(identity.SeedAgentEmail); len(got) != 0 {
			t.Fatalf("agent inbox = %v, want nothing", got)
		}
	})
}

func TestBusinessEnquiryInboxAccess(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.post("/v1/business-enquiries", e.enquiry("service-providers", "sample-plumbers")), http.StatusCreated)

	t.Run("anonymous is 401", func(t *testing.T) {
		wantError(t, e.get("/v1/inbox/business-enquiries"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.get("/v1/inbox/business-enquiries", withBearer("not-a-token")), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("no organisation membership is 403", func(t *testing.T) {
		cookies := map[string]*http.Cookie{
			"consumer":                 e.loginCookie(identity.SeedConsumerEmail, seedPassword),
			"direct seller":            e.loginCookie(devseed.SellerEmail, seedPassword),
			"platform administrator":   e.adminLogin(identity.SeedAdminEmail, seedPassword),
			"self-declared provider":   e.selfRegister("wannabe-plumber@example.com", "SERVICE_PROVIDER", "AGENT", "AGENCY_OWNER"),
			"self-declared private":    e.selfRegister("wannabe-seller@example.com", "PRIVATE_SELLER"),
			"customer who enquired":    e.selfRegister("customer@example.com", "CONSUMER"),
			"self-declared everything": e.selfRegister("everything@example.com", identity.SelectableAccountRoles()...),
		}
		for who, c := range cookies {
			rec := e.get("/v1/inbox/business-enquiries", withCookie(c))
			wantError(t, rec, http.StatusForbidden, httpx.CodeForbidden)
			if strings.Contains(rec.Body.String(), "tariro") {
				t.Fatalf("%s: a refused response leaked contact details", who)
			}
		}
	})

	t.Run("contact details are not readable anywhere else", func(t *testing.T) {
		for _, rec := range []*httptest.ResponseRecorder{
			e.get("/v1/business-enquiries"),
			e.get("/v1/business-enquiries/" + e.enquiries.Enquiries()[0].ID.String()),
			e.get("/v1/inbox/business-enquiries/" + e.enquiries.Enquiries()[0].ID.String()),
			e.get("/v1/agent/property-requests", withCookie(e.loginCookie(plumbersEmail, seedPassword))),
			e.get("/v1/me", withCookie(e.loginCookie(plumbersEmail, seedPassword))),
		} {
			if rec.Code == http.StatusOK && strings.Contains(strings.ToLower(rec.Body.String()), "tariro") {
				t.Fatalf("contact details leaked: %s", rec.Body)
			}
			if strings.Contains(rec.Body.String(), "263771234567") {
				t.Fatalf("phone number leaked: %s", rec.Body)
			}
		}
	})
}

func TestBusinessEnquiryRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := range businessEnquiriesBurst {
		if rec := e.post("/v1/business-enquiries", e.enquiry("service-providers", "sample-plumbers")); rec.Code != http.StatusCreated {
			t.Fatalf("enquiry %d: status %d", i+1, rec.Code)
		}
	}
	rec := e.post("/v1/business-enquiries", e.enquiry("service-providers", "sample-plumbers"))
	wantError(t, rec, http.StatusTooManyRequests, httpx.CodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if n := len(e.enquiries.Enquiries()); n != businessEnquiriesBurst {
		t.Fatalf("stored %d enquiries, want %d", n, businessEnquiriesBurst)
	}
	// Its own bucket: property requests, auth and another IP are unaffected.
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(1)), http.StatusCreated)
	e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	wantStatus(t, e.post("/v1/business-enquiries", e.enquiry("service-providers", "sample-plumbers"), withIP("203.0.113.9")), http.StatusCreated)
}

// Every one of the 29 sample profiles accepts an enquiry and somebody
// receives it.
func TestEverySampleProfileAcceptsAnEnquiry(t *testing.T) {
	e := newEnv(t)
	profiles := devseed.Profiles()
	if len(profiles) != devseed.ProfileCount {
		t.Fatalf("%d profiles, want %d", len(profiles), devseed.ProfileCount)
	}
	for i, p := range profiles {
		rec := e.post("/v1/business-enquiries", e.enquiry(p.Kind, p.Slug), withIP(fmt.Sprintf("198.51.100.%d", i+1)))
		wantStatus(t, rec, http.StatusCreated)
	}
	for i, enq := range e.enquiries.Enquiries() {
		p := profiles[i]
		if enq.Status != businessenquiries.StatusReceived || enq.Recipient == nil || enq.Recipient.OrganisationID != p.Organisation.ID ||
			enq.ProfileID != p.ID || enq.ProfileName != p.Name {
			t.Fatalf("%s was not routed to its organisation: %+v", p.Key(), enq.Recipient)
		}
		if (enq.Recipient.UserID != nil) != (p.Kind == businessenquiries.KindAgents) {
			t.Fatalf("%s: only agent profiles are addressed to a person", p.Key())
		}
	}
	// Each business owner sees exactly their own profile's enquiry.
	for _, a := range devseed.Accounts() {
		if a.Organisation == nil || len(a.Profiles) != 1 || strings.HasPrefix(a.Profiles[0], "agen") {
			continue
		}
		if got := e.enquiryInbox(a.Email); !sameStrings(got, a.Profiles) {
			t.Fatalf("%s inbox = %v, want %v", a.Email, got, a.Profiles)
		}
	}
}
