package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/notifications"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/mail"
)

const (
	lawFirmEmail  = "sample-law-chambers@example.test"
	guestEmail    = "tariro@example.com"
	guestPhone    = "+263771234567"
	consumerPhone = "+263772000111"
)

type apptRef struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Reference string `json:"reference"`
}

type apptBody struct {
	ID               string     `json:"id"`
	Reference        string     `json:"reference"`
	Type             string     `json:"type"`
	Status           string     `json:"status"`
	StatusLabel      string     `json:"status_label"`
	StartUTC         time.Time  `json:"start_utc"`
	EndUTC           time.Time  `json:"end_utc"`
	Timezone         string     `json:"timezone"`
	DurationMinutes  int        `json:"duration_minutes"`
	ProposedStartUTC *time.Time `json:"proposed_start_utc"`
	ProposedBy       *string    `json:"proposed_by"`
	Listing          *apptRef   `json:"listing"`
	With             *struct {
		Kind string `json:"kind"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"with"`
	LocationNote             *string `json:"location_note"`
	Note                     *string `json:"note"`
	PropertyRequestReference *string `json:"property_request_reference"`
	CancelledBy              *string `json:"cancelled_by"`
	CancelReason             *string `json:"cancel_reason"`
	Timeline                 []struct {
		Event   string  `json:"event"`
		From    *string `json:"from"`
		To      string  `json:"to"`
		ToLabel string  `json:"to_label"`
		Actor   string  `json:"actor"`
		Note    *string `json:"note"`
	} `json:"timeline"`
	CanCancel     bool `json:"can_cancel"`
	CanReschedule bool `json:"can_reschedule"`
	CanAccept     bool `json:"can_accept"`
	CanConfirm    bool `json:"can_confirm"`
	CanComplete   bool `json:"can_complete"`
	CanAssign     bool `json:"can_assign"`
	Contact       *struct {
		Name  string  `json:"name"`
		Email string  `json:"email"`
		Phone *string `json:"phone"`
	} `json:"contact"`
	AssignedTo *struct {
		UserID string `json:"user_id"`
		Name   string `json:"name"`
	} `json:"assigned_to"`
}

type apptOne struct {
	Appointment apptBody `json:"appointment"`
}

type apptPage struct {
	Items      []apptBody `json:"items"`
	Page       int        `json:"page"`
	PageSize   int        `json:"page_size"`
	Total      int        `json:"total"`
	TotalPages int        `json:"total_pages"`
}

// cast signs people in once (as the mobile app does) and remembers the tokens.
type cast struct {
	e      *env
	tokens map[string]string
}

func newCast(e *env) *cast { return &cast{e: e, tokens: map[string]string{}} }

func (c *cast) as(email string) reqOpt {
	if _, ok := c.tokens[email]; !ok {
		c.tokens[email] = c.e.loginBearer(email, seedPassword)
	}
	return withBearer(c.tokens[email])
}

// slot returns a start time d after the test clock, as the API expects it.
func (e *env) slot(d time.Duration) string { return e.now.Add(d).UTC().Format(time.RFC3339) }

// viewingRequest is a guest viewing request for listing n with a preferred slot.
func (e *env) viewingRequest(n int, after time.Duration) obj {
	body := e.propertyRequest(n)
	body["request_type"] = "VIEWING_REQUEST"
	body["preferred_slot_utc"] = e.slot(after)
	return body
}

// book creates an appointment for the signed-in customer and returns it.
func (c *cast) book(email string, body obj) apptBody {
	c.e.t.Helper()
	rec := c.e.post("/v1/appointments", body, c.as(email))
	wantStatus(c.e.t, rec, http.StatusCreated)
	return decode[apptOne](c.e.t, rec).Appointment
}

func (e *env) appointmentInput(n int, after time.Duration) obj {
	e.seq++
	return obj{
		"type": "VIEWING", "listing_id": propertyrequests.SeedListingID(n).String(), "start_utc": e.slot(after),
		"note": fmt.Sprintf("Viewing %d please", e.seq), "preferred_contact_method": "PHONE", "phone": consumerPhone,
		"idempotency_key": fmt.Sprintf("appt-key-%04d", e.seq),
		"consent":         obj{"privacy_version": "2026-01", "accepted": true},
	}
}

func (c *cast) act(email, path string, body any) *httptest.ResponseRecorder {
	return c.e.post(path, body, c.as(email))
}

func (c *cast) mustAct(email, path string, body any) apptBody {
	c.e.t.Helper()
	rec := c.act(email, path, body)
	wantStatus(c.e.t, rec, http.StatusOK)
	return decode[apptOne](c.e.t, rec).Appointment
}

func (c *cast) inboxIDs(email, query string) []string {
	c.e.t.Helper()
	rec := c.e.get("/v1/inbox/appointments"+query, c.as(email))
	wantStatus(c.e.t, rec, http.StatusOK)
	var ids []string
	for _, it := range decode[apptPage](c.e.t, rec).Items {
		ids = append(ids, it.ID)
	}
	return ids
}

func has(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func TestAppointmentFromViewingRequest(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)

	t.Run("a viewing request with a slot creates a REQUESTED appointment, once", func(t *testing.T) {
		body := e.viewingRequest(1, 48*time.Hour)
		rec := e.post("/v1/property-requests", body)
		wantStatus(t, rec, http.StatusCreated)
		res := decode[struct {
			RequestID   string `json:"request_id"`
			Reference   string `json:"reference"`
			Appointment *struct {
				ID        string `json:"id"`
				Reference string `json:"reference"`
			} `json:"appointment"`
		}](t, rec)
		if res.Appointment == nil || !strings.HasPrefix(res.Appointment.Reference, "AP-") || len(res.Appointment.Reference) != 11 {
			t.Fatalf("appointment = %+v, want {id, reference AP-XXXXXXXX}", res.Appointment)
		}
		all := e.appts.All()
		if len(all) != 1 {
			t.Fatalf("%d appointments stored, want 1", len(all))
		}
		a := all[0]
		if a.Status != appointments.StatusRequested || a.Type != appointments.TypeViewing || a.PropertyRequestID == nil ||
			a.PropertyRequestID.String() != res.RequestID || a.LinkedReference != res.Reference || a.CustomerUserID != nil ||
			a.CustomerEmail != guestEmail || a.CustomerPhone != guestPhone || a.Timezone != "Africa/Harare" ||
			a.DurationMinutes != 30 || a.AssignedUserID == nil || *a.AssignedUserID != identity.SeedAgentUserID ||
			a.OrganisationID == nil || *a.OrganisationID != devseed.SampleEstates.ID || a.ListingTitle == "" || a.WithSlug != "tendai-ncube" {
			t.Fatalf("unexpected appointment: %+v", a)
		}
		// The same key again: 200, the same appointment, nothing new.
		again := e.post("/v1/property-requests", body)
		wantStatus(t, again, http.StatusOK)
		if !strings.Contains(again.Body.String(), res.Appointment.ID) || len(e.appts.All()) != 1 {
			t.Fatalf("a replay must return the same appointment: %s", again.Body)
		}
		// The agent sees it with the guest's contact details.
		rec = e.get("/v1/inbox/appointments/"+res.Appointment.ID, c.as(identity.SeedAgentEmail))
		wantStatus(t, rec, http.StatusOK)
		got := decode[apptOne](t, rec).Appointment
		if got.Contact == nil || got.Contact.Email != guestEmail || got.Contact.Phone == nil || *got.Contact.Phone != guestPhone ||
			got.PropertyRequestReference == nil || *got.PropertyRequestReference != res.Reference || got.StatusLabel != "Requested" {
			t.Fatalf("inbox view = %s", rec.Body)
		}
	})

	t.Run("no slot, or another request type, creates none", func(t *testing.T) {
		before := len(e.appts.All())
		noSlot := e.propertyRequest(2)
		noSlot["request_type"] = "VIEWING_REQUEST"
		rec := e.post("/v1/property-requests", noSlot)
		wantStatus(t, rec, http.StatusCreated)
		if !strings.Contains(rec.Body.String(), `"appointment":null`) {
			t.Fatalf("appointment must be null without a slot: %s", rec.Body)
		}
		wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(2)), http.StatusCreated)
		if len(e.appts.All()) != before {
			t.Fatal("an appointment was created without a slot")
		}
	})

	t.Run("a signed-in customer's request appears in their appointments; a consultation request is a CONSULTATION", func(t *testing.T) {
		body := e.viewingRequest(3, 72*time.Hour)
		body["request_type"] = "CONSULTATION_REQUEST"
		delete(body, "guest_contact")
		wantStatus(t, e.post("/v1/property-requests", body, c.as(identity.SeedConsumerEmail)), http.StatusCreated)
		rec := e.get("/v1/me/appointments", c.as(identity.SeedConsumerEmail))
		wantStatus(t, rec, http.StatusOK)
		page := decode[apptPage](t, rec)
		if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Type != "CONSULTATION" || page.Page != 1 || page.PageSize != 20 || page.TotalPages != 1 {
			t.Fatalf("my appointments = %s", rec.Body)
		}
	})

	t.Run("one event per appointment, identifiers only", func(t *testing.T) {
		for _, ev := range e.appts.Outbox() {
			if ev.Type != appointments.EventRequested || strings.Contains(string(ev.Payload), "@") || strings.Contains(string(ev.Payload), "+263") {
				t.Fatalf("unexpected event %s %s", ev.Type, ev.Payload)
			}
		}
		if len(e.appts.Outbox()) != len(e.appts.All()) {
			t.Fatalf("%d events for %d appointments", len(e.appts.Outbox()), len(e.appts.All()))
		}
	})
}

func TestCreateAppointment(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	consumer := identity.SeedConsumerEmail

	t.Run("anonymous is 401", func(t *testing.T) {
		wantError(t, e.post("/v1/appointments", e.appointmentInput(1, 48*time.Hour)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	body := e.appointmentInput(1, 48*time.Hour)
	created := c.book(consumer, body)

	t.Run("created, idempotent replay, conflict", func(t *testing.T) {
		if created.Status != "REQUESTED" || created.StatusLabel != "Requested" || created.Type != "VIEWING" || created.Timezone != "Africa/Harare" ||
			created.DurationMinutes != 30 || !created.EndUTC.Equal(created.StartUTC.Add(30*time.Minute)) || created.Listing == nil ||
			created.With == nil || created.With.Kind != "agents" || created.With.Slug != "tendai-ncube" || !created.CanCancel || !created.CanReschedule ||
			created.CanAccept || len(created.Timeline) != 1 || created.Timeline[0].Event != "REQUESTED" || created.Timeline[0].Actor != "CUSTOMER" || created.Contact != nil {
			t.Fatalf("created = %+v", created)
		}
		rec := e.post("/v1/appointments", body, c.as(consumer))
		wantStatus(t, rec, http.StatusOK)
		if decode[apptOne](t, rec).Appointment.ID != created.ID || len(e.appts.All()) != 1 {
			t.Fatal("a replay must return the original appointment")
		}
		changed := obj{}
		for k, v := range body {
			changed[k] = v
		}
		changed["note"] = "Something else"
		wantError(t, e.post("/v1/appointments", changed, c.as(consumer)), http.StatusConflict, appointments.CodeIdempotencyConflict)
		// The key may also arrive in the header.
		header := e.appointmentInput(2, 50*time.Hour)
		key := header["idempotency_key"].(string)
		delete(header, "idempotency_key")
		wantStatus(t, e.post("/v1/appointments", header, c.as(consumer), withHeader("Idempotency-Key", key)), http.StatusCreated)
	})

	t.Run("a consultation with a solicitor", func(t *testing.T) {
		in := e.appointmentInput(1, 80*time.Hour)
		delete(in, "listing_id")
		in["type"], in["recipient"], in["duration_minutes"] = "CONSULTATION", obj{"kind": "solicitors", "slug": "sample-law-chambers"}, 60
		a := c.book(consumer, in)
		if a.Listing != nil || a.With == nil || a.With.Name != "Sample Law Chambers" || a.DurationMinutes != 60 {
			t.Fatalf("consultation = %+v", a)
		}
	})

	t.Run("validation", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(obj)
			field  string
		}{
			{"past time", func(b obj) { b["start_utc"] = e.slot(-time.Hour) }, "start_utc"},
			{"not a time", func(b obj) { b["start_utc"] = "tomorrow" }, "start_utc"},
			{"too far ahead", func(b obj) { b["start_utc"] = e.slot(400 * 24 * time.Hour) }, "start_utc"},
			{"both targets", func(b obj) { b["recipient"] = obj{"kind": "agents", "slug": "tendai-ncube"} }, "recipient"},
			{"neither target", func(b obj) { delete(b, "listing_id") }, "listing_id"},
			{"viewing of a profile", func(b obj) {
				delete(b, "listing_id")
				b["recipient"] = obj{"kind": "agents", "slug": "tendai-ncube"}
			}, "listing_id"},
			{"unknown directory", func(b obj) {
				delete(b, "listing_id")
				b["type"], b["recipient"] = "CONSULTATION", obj{"kind": "wizards", "slug": "x"}
			}, "recipient.kind"},
			{"short duration", func(b obj) { b["duration_minutes"] = 10 }, "duration_minutes"},
			{"long duration", func(b obj) { b["duration_minutes"] = 241 }, "duration_minutes"},
			{"unknown type", func(b obj) { b["type"] = "PARTY" }, "type"},
			{"html note", func(b obj) { b["note"] = "<b>hi</b>" }, "note"},
			{"phone needed", func(b obj) { delete(b, "phone") }, "phone"},
			{"contact method", func(b obj) { b["preferred_contact_method"] = "FAX" }, "preferred_contact_method"},
			{"consent", func(b obj) { b["consent"] = obj{"privacy_version": "2026-01", "accepted": false} }, "consent.accepted"},
			{"listing id", func(b obj) { b["listing_id"] = "not-a-uuid" }, "listing_id"},
			{"no key", func(b obj) { delete(b, "idempotency_key") }, "idempotency_key"},
		}
		for i, tc := range cases {
			in := e.appointmentInput(1, 90*time.Hour)
			tc.mutate(in)
			env := wantError(t, e.post("/v1/appointments", in, c.as(consumer), withIP(fmt.Sprintf("198.51.100.%d", i+1))), http.StatusBadRequest, httpx.CodeValidationFailed)
			if _, ok := env.Error.Fields[tc.field]; !ok {
				t.Fatalf("%s: fields = %v, want %q", tc.name, env.Error.Fields, tc.field)
			}
		}
	})

	t.Run("unknown and unavailable targets", func(t *testing.T) {
		in := e.appointmentInput(1, 90*time.Hour)
		in["listing_id"] = uuid.NewString()
		other := withIP("198.51.100.200")
		wantError(t, e.post("/v1/appointments", in, c.as(consumer), other), http.StatusNotFound, appointments.CodeListingNotFound)

		in = e.appointmentInput(99, 90*time.Hour)
		wantError(t, e.post("/v1/appointments", in, c.as(consumer), other), http.StatusGone, appointments.CodeListingUnavailable)

		in = e.appointmentInput(1, 90*time.Hour)
		delete(in, "listing_id")
		in["type"], in["recipient"] = "CONSULTATION", obj{"kind": "agents", "slug": "nobody-here"}
		wantError(t, e.post("/v1/appointments", in, c.as(consumer), other), http.StatusNotFound, appointments.CodeRecipientNotFound)
	})

	t.Run("unknown fields are refused and the route has its own rate limit", func(t *testing.T) {
		in := e.appointmentInput(1, 90*time.Hour)
		in["customer_user_id"] = uuid.NewString()
		wantError(t, e.post("/v1/appointments", in, c.as(consumer), withIP("198.51.100.201")), http.StatusBadRequest, httpx.CodeInvalidJSON)
		limited := false
		for i := 0; i < 15 && !limited; i++ {
			rec := e.post("/v1/appointments", obj{}, c.as(consumer), withIP("203.0.113.77"))
			limited = rec.Code == http.StatusTooManyRequests
		}
		if !limited {
			t.Fatal("POST /v1/appointments was never rate limited")
		}
		wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(1), withIP("203.0.113.77")), http.StatusCreated)
	})
}

func TestAppointmentStateMachine(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	consumer, agent := identity.SeedConsumerEmail, identity.SeedAgentEmail

	a := c.book(consumer, e.appointmentInput(1, 48*time.Hour))
	mine, inbox := "/v1/me/appointments/"+a.ID, "/v1/inbox/appointments/"+a.ID
	conflict := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		wantError(t, rec, http.StatusConflict, appointments.CodeInvalidState)
	}

	// REQUESTED: nothing to accept, nothing to complete.
	conflict(c.act(consumer, mine+"/accept", nil))
	conflict(c.act(agent, inbox+"/complete", nil))
	conflict(c.act(agent, inbox+"/no-show", nil))

	// The professional confirms the requested time, adding where to meet.
	got := c.mustAct(agent, inbox+"/confirm", obj{"location_note": "Meet at the gate.", "duration_minutes": 45})
	if got.Status != "CONFIRMED" || got.StatusLabel != "Confirmed" || got.LocationNote == nil || *got.LocationNote != "Meet at the gate." ||
		got.DurationMinutes != 45 || !got.StartUTC.Equal(a.StartUTC) || got.CanConfirm || !got.CanCancel {
		t.Fatalf("after confirm: %+v", got)
	}
	conflict(c.act(agent, inbox+"/confirm", nil))
	conflict(c.act(consumer, mine+"/accept", nil))
	conflict(c.act(agent, inbox+"/complete", nil)) // not before the start time

	// The customer proposes a new time; the professional accepts it with an empty confirm.
	newStart := e.now.Add(72 * time.Hour).Truncate(time.Second)
	got = c.mustAct(consumer, mine+"/reschedule", obj{"start_utc": newStart.Format(time.RFC3339), "note": "Could we do a day later?"})
	if got.Status != "RESCHEDULE_REQUESTED" || got.StatusLabel != "Reschedule Requested" || got.ProposedBy == nil || *got.ProposedBy != "CUSTOMER" ||
		got.ProposedStartUTC == nil || !got.ProposedStartUTC.Equal(newStart) || !got.StartUTC.Equal(a.StartUTC) || got.CanAccept || got.CanReschedule {
		t.Fatalf("after customer reschedule: %+v", got)
	}
	conflict(c.act(consumer, mine+"/accept", nil))                                          // one cannot accept one's own proposal
	conflict(c.act(consumer, mine+"/reschedule", obj{"start_utc": e.slot(96 * time.Hour)})) // nor propose again
	got = c.mustAct(agent, inbox+"/confirm", nil)
	if got.Status != "CONFIRMED" || !got.StartUTC.Equal(newStart) || got.ProposedStartUTC != nil || got.ProposedBy != nil {
		t.Fatalf("after accepting the customer's time: %+v", got)
	}

	// The professional proposes another time; the customer counter-proposes;
	// the professional proposes again through confirm with a different time;
	// the customer accepts.
	wantError(t, c.act(agent, inbox+"/reschedule", obj{"start_utc": newStart.Format(time.RFC3339)}), http.StatusBadRequest, httpx.CodeValidationFailed)
	got = c.mustAct(agent, inbox+"/reschedule", obj{"start_utc": e.slot(100 * time.Hour)})
	if got.ProposedBy == nil || *got.ProposedBy != "PROFESSIONAL" {
		t.Fatalf("after professional reschedule: %+v", got)
	}
	conflict(c.act(agent, inbox+"/confirm", nil)) // waiting for the customer
	c.mustAct(consumer, mine+"/reschedule", obj{"start_utc": e.slot(110 * time.Hour)})
	final := e.now.Add(120 * time.Hour).Truncate(time.Second)
	got = c.mustAct(agent, inbox+"/confirm", obj{"start_utc": final.Format(time.RFC3339)})
	if got.Status != "RESCHEDULE_REQUESTED" || got.ProposedBy == nil || *got.ProposedBy != "PROFESSIONAL" || !got.ProposedStartUTC.Equal(final) {
		t.Fatalf("a confirm with a different time must become a proposal: %+v", got)
	}
	rec := e.get(mine, c.as(consumer))
	wantStatus(t, rec, http.StatusOK)
	if v := decode[apptOne](t, rec).Appointment; !v.CanAccept || !v.CanReschedule || !v.CanCancel {
		t.Fatalf("the customer must be able to accept: %+v", v)
	}
	got = c.mustAct(consumer, mine+"/accept", nil)
	if got.Status != "CONFIRMED" || !got.StartUTC.Equal(final) {
		t.Fatalf("after accept: %+v", got)
	}

	// Completed only after the start time; then nothing else is possible.
	e.now = final.Add(time.Hour)
	got = c.mustAct(agent, inbox+"/complete", nil)
	if got.Status != "COMPLETED" || got.StatusLabel != "Completed" || got.CanCancel {
		t.Fatalf("after complete: %+v", got)
	}
	conflict(c.act(agent, inbox+"/cancel", obj{"reason": "too late"}))
	conflict(c.act(consumer, mine+"/cancel", nil))
	conflict(c.act(agent, inbox+"/no-show", nil))
	conflict(c.act(consumer, mine+"/reschedule", obj{"start_utc": e.slot(24 * time.Hour)}))

	// The timeline holds every transition in order, and one event each.
	want := []string{"REQUESTED", "CONFIRMED", "RESCHEDULE_REQUESTED", "CONFIRMED", "RESCHEDULE_REQUESTED", "RESCHEDULE_REQUESTED", "RESCHEDULE_REQUESTED", "CONFIRMED", "COMPLETED"}
	if len(got.Timeline) != len(want) {
		t.Fatalf("timeline has %d entries, want %d: %+v", len(got.Timeline), len(want), got.Timeline)
	}
	for i, entry := range got.Timeline {
		if entry.Event != want[i] || entry.To != want[i] || entry.ToLabel != appointments.StatusLabel(want[i]) {
			t.Fatalf("timeline[%d] = %+v, want %s", i, entry, want[i])
		}
	}
	if got.Timeline[2].Actor != "CUSTOMER" || got.Timeline[2].Note == nil || got.Timeline[1].Actor != "PROFESSIONAL" || *got.Timeline[1].From != "REQUESTED" {
		t.Fatalf("timeline actors: %+v", got.Timeline)
	}
	if n := len(e.appts.Outbox()); n != len(want) {
		t.Fatalf("%d outbox events, want one per transition (%d)", n, len(want))
	}

	t.Run("no-show and both cancellations", func(t *testing.T) {
		b := c.book(consumer, e.appointmentInput(2, 24*time.Hour))
		c.mustAct(agent, "/v1/inbox/appointments/"+b.ID+"/confirm", nil)
		e.now = e.now.Add(25 * time.Hour)
		if got := c.mustAct(agent, "/v1/inbox/appointments/"+b.ID+"/no-show", nil); got.Status != "NO_SHOW" || got.StatusLabel != "No Show" {
			t.Fatalf("no-show: %+v", got)
		}

		d := c.book(consumer, e.appointmentInput(2, 24*time.Hour))
		got := c.mustAct(consumer, "/v1/me/appointments/"+d.ID+"/cancel", obj{"reason": "Found another place"})
		if got.Status != "CANCELLED" || got.StatusLabel != "Cancelled" || *got.CancelledBy != "CUSTOMER" || *got.CancelReason != "Found another place" || got.CanCancel {
			t.Fatalf("customer cancel: %+v", got)
		}
		conflict(c.act(agent, "/v1/inbox/appointments/"+d.ID+"/confirm", nil))

		f := c.book(consumer, e.appointmentInput(2, 24*time.Hour))
		env := wantError(t, c.act(agent, "/v1/inbox/appointments/"+f.ID+"/cancel", obj{}), http.StatusBadRequest, httpx.CodeValidationFailed)
		if _, ok := env.Error.Fields["reason"]; !ok {
			t.Fatalf("a professional must give a reason: %v", env.Error.Fields)
		}
		got = c.mustAct(agent, "/v1/inbox/appointments/"+f.ID+"/cancel", obj{"reason": "The owner is away"})
		if got.Status != "CANCELLED" || *got.CancelledBy != "PROFESSIONAL" {
			t.Fatalf("professional cancel: %+v", got)
		}

		// A requested time that has passed cannot be confirmed any more.
		g := c.book(consumer, e.appointmentInput(2, time.Hour))
		e.now = e.now.Add(2 * time.Hour)
		conflict(c.act(agent, "/v1/inbox/appointments/"+g.ID+"/confirm", nil))
		wantError(t, c.act(agent, "/v1/inbox/appointments/"+g.ID+"/reschedule", obj{"start_utc": e.slot(-time.Minute)}), http.StatusBadRequest, httpx.CodeValidationFailed)
	})
}

func TestAppointmentSlotTaken(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	consumer, agent := identity.SeedConsumerEmail, identity.SeedAgentEmail
	confirm := func(a apptBody) *httptest.ResponseRecorder {
		return c.act(agent, "/v1/inbox/appointments/"+a.ID+"/confirm", nil)
	}

	// Listings 1, 2 and 5 all belong to the same agent.
	first := c.book(consumer, e.appointmentInput(1, 48*time.Hour))
	overlapping := c.book(consumer, e.appointmentInput(2, 48*time.Hour+15*time.Minute))
	backToBack := c.book(consumer, e.appointmentInput(5, 48*time.Hour+30*time.Minute))
	wantStatus(t, confirm(first), http.StatusOK)

	wantError(t, confirm(overlapping), http.StatusConflict, appointments.CodeSlotTaken)
	if got := e.get("/v1/inbox/appointments/"+overlapping.ID, c.as(agent)); decode[apptOne](t, got).Appointment.Status != "REQUESTED" {
		t.Fatal("a refused confirm must change nothing")
	}
	// Back to back is fine: one ends exactly when the next starts.
	wantStatus(t, confirm(backToBack), http.StatusOK)

	// Accepting a proposed time is subject to the same rule.
	proposal := obj{"start_utc": first.StartUTC.Add(-10 * time.Minute).Format(time.RFC3339)}
	c.mustAct(agent, "/v1/inbox/appointments/"+overlapping.ID+"/reschedule", proposal)
	wantError(t, c.act(consumer, "/v1/me/appointments/"+overlapping.ID+"/accept", nil), http.StatusConflict, appointments.CodeSlotTaken)

	// Another agent is not affected: listing 7 belongs to the second agent.
	other := c.book(consumer, e.appointmentInput(7, 48*time.Hour))
	wantStatus(t, c.act(devseed.EstatesAgent2Email, "/v1/inbox/appointments/"+other.ID+"/confirm", nil), http.StatusOK)

	// Once the first is cancelled, its time is free again.
	c.mustAct(agent, "/v1/inbox/appointments/"+first.ID+"/cancel", obj{"reason": "Owner unavailable"})
	c.mustAct(consumer, "/v1/me/appointments/"+overlapping.ID+"/accept", nil)
}

func TestAppointmentCustomersSeeOnlyTheirOwn(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	a := c.book(identity.SeedConsumerEmail, e.appointmentInput(1, 48*time.Hour))
	c.book(identity.SeedConsumerEmail, e.appointmentInput(2, 24*time.Hour))
	mine := "/v1/me/appointments/" + a.ID

	// Another customer (the direct seller is a customer too) gets 404 everywhere.
	other := c.as(devseed.SellerEmail)
	wantError(t, e.get(mine, other), http.StatusNotFound, appointments.CodeAppointmentNotFound)
	wantError(t, e.post(mine+"/cancel", nil, other), http.StatusNotFound, appointments.CodeAppointmentNotFound)
	wantError(t, e.post(mine+"/reschedule", obj{"start_utc": e.slot(99 * time.Hour)}, other), http.StatusNotFound, appointments.CodeAppointmentNotFound)
	wantError(t, e.post(mine+"/accept", nil, other), http.StatusNotFound, appointments.CodeAppointmentNotFound)
	wantError(t, e.get("/v1/me/appointments/not-a-uuid", other), http.StatusNotFound, appointments.CodeAppointmentNotFound)
	if page := decode[apptPage](t, e.get("/v1/me/appointments?when=all", other)); page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("another customer sees %d appointments", page.Total)
	}
	wantError(t, e.get("/v1/me/appointments"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.post(mine+"/cancel", nil), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// A cookie session needs the Origin header on POSTs, like every other route.
	cookie := e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	wantError(t, e.post(mine+"/cancel", nil, withCookie(cookie)), http.StatusForbidden, httpx.CodeCSRFOrigin)

	// The customer's view: soonest first, no contact details of anyone.
	rec := e.get("/v1/me/appointments", c.as(identity.SeedConsumerEmail))
	wantStatus(t, rec, http.StatusOK)
	page := decode[apptPage](t, rec)
	if page.Total != 2 || !page.Items[0].StartUTC.Before(page.Items[1].StartUTC) {
		t.Fatalf("upcoming must be soonest first: %s", rec.Body)
	}
	for _, banned := range []string{`"contact"`, `"email"`, `"phone"`, consumerPhone, "@", `"assigned_to"`, `"timeline"`} {
		if strings.Contains(rec.Body.String(), banned) {
			t.Fatalf("the customer list contains %s: %s", banned, rec.Body)
		}
	}
	detail := e.get(mine, c.as(identity.SeedConsumerEmail))
	for _, banned := range []string{`"contact"`, consumerPhone, "@"} {
		if strings.Contains(detail.Body.String(), banned) {
			t.Fatalf("the customer detail contains %s: %s", banned, detail.Body)
		}
	}

	// when / status filters and paging.
	c.mustAct(identity.SeedConsumerEmail, mine+"/cancel", nil)
	for query, want := range map[string]int{"": 1, "?when=upcoming": 1, "?when=past": 1, "?when=all": 2, "?when=all&status=CANCELLED": 1, "?when=all&status=CANCELLED,REQUESTED": 2, "?when=all&page_size=1&page=2": 1} {
		if got := decode[apptPage](t, e.get("/v1/me/appointments"+query, c.as(identity.SeedConsumerEmail))); len(got.Items) != want {
			t.Fatalf("%q returned %d items, want %d", query, len(got.Items), want)
		}
	}
	for _, bad := range []string{"?when=soon", "?status=DONE", "?page=0", "?page_size=101"} {
		wantError(t, e.get("/v1/me/appointments"+bad, c.as(identity.SeedConsumerEmail)), http.StatusBadRequest, httpx.CodeValidationFailed)
	}
}

func TestAppointmentProfessionalScoping(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	consumer := identity.SeedConsumerEmail
	agent, agent2, owner := identity.SeedAgentEmail, devseed.EstatesAgent2Email, devseed.EstatesOwnerEmail
	otherAgency, seller := devseed.CommercialAgentEmail, devseed.SellerEmail

	// A listing of Sample Estates with no assigned agent: its viewings go to
	// the organisation, its consultations too.
	unassignedListing := propertyrequests.SeedListingID(50)
	org := devseed.SampleEstates.ID
	e.requests.PutListing(propertyrequests.Listing{
		ID: unassignedListing, MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
		OrganisationID: &org, OrganisationEligible: true,
	})

	forAgent := c.book(consumer, e.appointmentInput(1, 30*time.Hour))    // assigned to the first agent
	forAgent2 := c.book(consumer, e.appointmentInput(7, 31*time.Hour))   // assigned to the second agent
	forSeller := c.book(consumer, e.appointmentInput(6, 32*time.Hour))   // a direct owner's listing
	forOther := c.book(consumer, e.appointmentInput(3, 33*time.Hour))    // another agency
	orgViewing := c.book(consumer, e.appointmentInput(50, 34*time.Hour)) // unassigned viewing
	in := e.appointmentInput(50, 35*time.Hour)
	in["type"] = "CONSULTATION"
	orgConsultation := c.book(consumer, in) // unassigned consultation
	in = e.appointmentInput(1, 36*time.Hour)
	delete(in, "listing_id")
	in["type"], in["recipient"] = "CONSULTATION", obj{"kind": "agents", "slug": "tendai-ncube"}
	toAgentProfile := c.book(consumer, in)
	in = e.appointmentInput(1, 37*time.Hour)
	delete(in, "listing_id")
	in["type"], in["recipient"] = "CONSULTATION", obj{"kind": "solicitors", "slug": "sample-law-chambers"}
	forLawFirm := c.book(consumer, in)

	all := []apptBody{forAgent, forAgent2, forSeller, forOther, orgViewing, orgConsultation, toAgentProfile, forLawFirm}
	sees := map[string][]apptBody{
		agent:        {forAgent, orgViewing, toAgentProfile},
		agent2:       {forAgent2, orgViewing},
		owner:        {forAgent, forAgent2, orgViewing, orgConsultation, toAgentProfile},
		otherAgency:  {forOther},
		seller:       {forSeller},
		lawFirmEmail: {forLawFirm},
	}
	for who, want := range sees {
		ids := c.inboxIDs(who, "?page_size=100")
		if len(ids) != len(want) {
			t.Fatalf("%s sees %d appointments, want %d", who, len(ids), len(want))
		}
		for _, a := range all {
			visible := false
			for _, w := range want {
				visible = visible || w.ID == a.ID
			}
			if has(ids, a.ID) != visible {
				t.Fatalf("%s: visibility of %s (%s) = %v, want %v", who, a.Reference, a.Type, !visible, visible)
			}
			// What cannot be seen cannot be read or changed either: 404.
			if !visible {
				path := "/v1/inbox/appointments/" + a.ID
				wantError(t, e.get(path, c.as(who)), http.StatusNotFound, appointments.CodeAppointmentNotFound)
				wantError(t, c.act(who, path+"/confirm", nil), http.StatusNotFound, appointments.CodeAppointmentNotFound)
				wantError(t, c.act(who, path+"/cancel", obj{"reason": "x"}), http.StatusNotFound, appointments.CodeAppointmentNotFound)
				wantError(t, c.act(who, path+"/assign", obj{"user_id": devseed.EstatesAgent2ID.String()}), http.StatusNotFound, appointments.CodeAppointmentNotFound)
			}
		}
	}

	t.Run("401 and 403", func(t *testing.T) {
		wantError(t, e.get("/v1/inbox/appointments"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.get("/v1/inbox/appointments", c.as(consumer)), http.StatusForbidden, httpx.CodeForbidden)
		wantError(t, c.act(consumer, "/v1/inbox/appointments/"+forAgent.ID+"/confirm", nil), http.StatusForbidden, httpx.CodeForbidden)
	})

	t.Run("the inbox carries the customer's contact details and is soonest first", func(t *testing.T) {
		rec := e.get("/v1/inbox/appointments", c.as(owner))
		wantStatus(t, rec, http.StatusOK)
		page := decode[apptPage](t, rec)
		for i, it := range page.Items {
			if it.Contact == nil || it.Contact.Email != identity.SeedConsumerEmail || it.Contact.Phone == nil || *it.Contact.Phone != consumerPhone || it.Note == nil {
				t.Fatalf("item without contact: %+v", it)
			}
			if i > 0 && it.StartUTC.Before(page.Items[i-1].StartUTC) {
				t.Fatal("the inbox must be ordered by start time")
			}
			if !it.CanAssign {
				t.Fatal("an owner may assign")
			}
		}
		if page.Items[0].AssignedTo == nil || page.Items[0].AssignedTo.Name != "Tendai Ncube" {
			t.Fatalf("assigned_to = %+v", page.Items[0].AssignedTo)
		}
	})

	t.Run("filters", func(t *testing.T) {
		for query, want := range map[string]int{
			"?type=CONSULTATION": 2, "?type=VIEWING": 3, "?status=CONFIRMED": 0, "?status=REQUESTED": 5,
			"?listing_id=" + unassignedListing.String():                     2,
			"?from=" + e.slot(33*time.Hour):                                 3,
			"?to=" + e.slot(33*time.Hour):                                   2,
			"?from=" + e.slot(31*time.Hour) + "&to=" + e.slot(35*time.Hour): 2,
		} {
			if got := c.inboxIDs(owner, query); len(got) != want {
				t.Fatalf("%q returned %d, want %d", query, len(got), want)
			}
		}
		for _, bad := range []string{"?type=PARTY", "?from=yesterday", "?listing_id=x", "?from=" + e.slot(5*time.Hour) + "&to=" + e.slot(4*time.Hour)} {
			wantError(t, e.get("/v1/inbox/appointments"+bad, c.as(owner)), http.StatusBadRequest, httpx.CodeValidationFailed)
		}
	})

	t.Run("summary counts are scoped the same way", func(t *testing.T) {
		type summary struct {
			PropertyRequests  struct{ New int } `json:"property_requests"`
			BusinessEnquiries struct{ New int } `json:"business_enquiries"`
			Appointments      appointments.Counts
		}
		wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(1)), http.StatusCreated)
		enquiry := e.enquiry("agencies", "sample-estates")
		wantStatus(t, e.post("/v1/business-enquiries", enquiry), http.StatusCreated)
		c.mustAct(agent, "/v1/inbox/appointments/"+forAgent.ID+"/confirm", nil)
		c.mustAct(consumer, "/v1/me/appointments/"+toAgentProfile.ID+"/reschedule", obj{"start_utc": e.slot(60 * time.Hour)})

		got := decode[summary](t, e.get("/v1/inbox/summary", c.as(agent)))
		if got.Appointments != (appointments.Counts{Requested: 1, RescheduleRequested: 1, UpcomingConfirmed: 1}) || got.PropertyRequests.New != 1 || got.BusinessEnquiries.New != 0 {
			t.Fatalf("agent summary = %+v", got)
		}
		got = decode[summary](t, e.get("/v1/inbox/summary", c.as(owner)))
		if got.Appointments != (appointments.Counts{Requested: 3, RescheduleRequested: 1, UpcomingConfirmed: 1}) || got.PropertyRequests.New != 1 || got.BusinessEnquiries.New != 1 {
			t.Fatalf("owner summary = %+v", got)
		}
		got = decode[summary](t, e.get("/v1/inbox/summary", c.as(otherAgency)))
		if got.Appointments != (appointments.Counts{Requested: 1}) || got.PropertyRequests.New != 0 {
			t.Fatalf("other agency summary = %+v", got)
		}
		got = decode[summary](t, e.get("/v1/inbox/summary", c.as(consumer)))
		if got.Appointments != (appointments.Counts{}) || got.PropertyRequests.New != 0 || got.BusinessEnquiries.New != 0 {
			t.Fatalf("a customer's summary must be all zeros: %+v", got)
		}
		wantError(t, e.get("/v1/inbox/summary"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("assign: OWNER or MANAGER only, and only to a member", func(t *testing.T) {
		path := "/v1/inbox/appointments/" + orgViewing.ID + "/assign"
		to := obj{"user_id": devseed.EstatesAgent2ID.String()}
		wantError(t, c.act(agent, path, to), http.StatusForbidden, httpx.CodeForbidden)
		env := wantError(t, c.act(owner, path, obj{"user_id": devseed.CommercialAgentID.String()}), http.StatusBadRequest, httpx.CodeValidationFailed)
		if _, ok := env.Error.Fields["user_id"]; !ok {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
		wantError(t, c.act(owner, path, obj{"user_id": "nope"}), http.StatusBadRequest, httpx.CodeValidationFailed)
		wantError(t, c.act(otherAgency, path, to), http.StatusNotFound, appointments.CodeAppointmentNotFound)

		got := c.mustAct(owner, path, to)
		if got.AssignedTo == nil || got.AssignedTo.Name != "Chipo Dube" || got.Status != "REQUESTED" {
			t.Fatalf("after assign: %+v", got)
		}
		last := got.Timeline[len(got.Timeline)-1]
		if last.Event != "ASSIGNED" || last.Actor != "PROFESSIONAL" {
			t.Fatalf("timeline = %+v", got.Timeline)
		}
		if has(c.inboxIDs(agent, "?page_size=100"), orgViewing.ID) || !has(c.inboxIDs(agent2, "?page_size=100"), orgViewing.ID) {
			t.Fatal("after assignment only the assignee (and the owner) may see it")
		}
		// The customer does not see the internal assignment.
		detail := e.get("/v1/me/appointments/"+orgViewing.ID, c.as(consumer))
		if strings.Contains(detail.Body.String(), "ASSIGNED") || strings.Contains(detail.Body.String(), "Chipo") {
			t.Fatalf("the customer sees the assignment: %s", detail.Body)
		}
		// A closed appointment cannot be assigned.
		c.mustAct(owner, "/v1/inbox/appointments/"+orgConsultation.ID+"/cancel", obj{"reason": "Not needed"})
		wantError(t, c.act(owner, "/v1/inbox/appointments/"+orgConsultation.ID+"/assign", to), http.StatusConflict, appointments.CodeInvalidState)
	})

	t.Run("a direct owner and a law firm act on their own", func(t *testing.T) {
		if got := c.mustAct(seller, "/v1/inbox/appointments/"+forSeller.ID+"/confirm", nil); got.Status != "CONFIRMED" || got.With != nil {
			t.Fatalf("seller confirm: %+v", got)
		}
		if got := c.mustAct(lawFirmEmail, "/v1/inbox/appointments/"+forLawFirm.ID+"/confirm", nil); got.Status != "CONFIRMED" || got.With == nil || got.With.Kind != "solicitors" {
			t.Fatalf("law firm confirm: %+v", got)
		}
	})
}

func TestAppointmentHistoryOutlivesTheListing(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	a := c.book(identity.SeedConsumerEmail, e.appointmentInput(3, 48*time.Hour))
	c.mustAct(devseed.CommercialAgentEmail, "/v1/inbox/appointments/"+a.ID+"/confirm", nil)

	// The listing expires.
	listing, err := e.requests.GetListing(context.Background(), propertyrequests.SeedListingID(3))
	if err != nil {
		t.Fatal(err)
	}
	expired := e.now.Add(-time.Minute)
	listing.Status, listing.ExpiresAt = "EXPIRED", &expired
	e.requests.PutListing(listing)
	wantError(t, e.post("/v1/appointments", e.appointmentInput(3, 60*time.Hour), c.as(identity.SeedConsumerEmail)), http.StatusGone, appointments.CodeListingUnavailable)

	// The appointment and its history are still there, with the listing's name.
	for _, rec := range []*httptest.ResponseRecorder{
		e.get("/v1/me/appointments/"+a.ID, c.as(identity.SeedConsumerEmail)),
		e.get("/v1/inbox/appointments/"+a.ID, c.as(devseed.CommercialAgentEmail)),
	} {
		wantStatus(t, rec, http.StatusOK)
		got := decode[apptOne](t, rec).Appointment
		if got.Status != "CONFIRMED" || got.Listing == nil || got.Listing.Title != a.Listing.Title || len(got.Timeline) != 2 {
			t.Fatalf("after expiry: %+v", got)
		}
	}
	if got := c.mustAct(devseed.CommercialAgentEmail, "/v1/inbox/appointments/"+a.ID+"/cancel", obj{"reason": "The listing has expired"}); got.Status != "CANCELLED" {
		t.Fatalf("cancel after expiry: %+v", got)
	}
}

// captureSender records what would be sent.
type captureSender struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (s *captureSender) Name() string { return "capture" }

func (s *captureSender) Send(_ context.Context, _ string, m mail.Message) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return "provider-" + uuid.NewString(), nil
}

func TestAppointmentEmails(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	consumer, agent := identity.SeedConsumerEmail, identity.SeedAgentEmail

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	renderer, err := mail.NewRenderer(testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	sender := &captureSender{}
	handlers := notifications.NewHandlers(mail.NewService(sender, mail.NewMemoryDeliveryLog(), logger), renderer,
		notifications.NewMemorySource(e.identity, e.requests, e.enquiries), logger)
	handlers.SetAppointmentSource(e.appts.Get)

	// One of each transition: requested, confirmed, the customer proposes,
	// the professional proposes, accepted, cancelled by each side, completed.
	start := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC) // 10:00 in Harare
	in := e.appointmentInput(1, 0)
	in["start_utc"] = start.Format(time.RFC3339)
	a := c.book(consumer, in)
	mine, inbox := "/v1/me/appointments/"+a.ID, "/v1/inbox/appointments/"+a.ID
	c.mustAct(agent, inbox+"/confirm", obj{"location_note": "Meet at the gate. Call +263 77 555 0000 or agent@example.test"})
	c.mustAct(consumer, mine+"/reschedule", obj{"start_utc": start.Add(24 * time.Hour).Format(time.RFC3339)})
	c.mustAct(agent, inbox+"/reschedule", obj{"start_utc": start.Add(48 * time.Hour).Format(time.RFC3339)})
	c.mustAct(consumer, mine+"/accept", nil)
	c.mustAct(agent, inbox+"/cancel", obj{"reason": "The owner is away that week"})
	b := c.book(consumer, e.appointmentInput(2, 30*time.Hour))
	c.mustAct(consumer, "/v1/me/appointments/"+b.ID+"/cancel", obj{"reason": "Changed my mind"})
	// A guest's appointment: the guest is told by email too.
	rec := e.post("/v1/property-requests", e.viewingRequest(5, 90*time.Hour))
	wantStatus(t, rec, http.StatusCreated)
	guest := e.appts.All()[2]
	c.mustAct(agent, "/v1/inbox/appointments/"+guest.ID.String()+"/confirm", nil)

	events := e.appts.Outbox()
	// The worker may run an event twice: nothing is sent twice.
	for round := 0; round < 2; round++ {
		for _, ev := range events {
			if err := handlers.Handle(context.Background(), ev); err != nil {
				t.Fatalf("%s: %v", ev.Type, err)
			}
		}
	}
	want := map[string]int{
		"appointment-requested": 3, "appointment-confirmed": 3, "appointment-reschedule-proposed": 1,
		"appointment-new-time-proposed": 1, "appointment-cancelled": 1, "appointment-cancelled-by-customer": 1,
	}
	got := map[string]int{}
	for _, m := range sender.sent {
		got[m.Kind]++
		toProfessional := m.Kind == "appointment-requested" || m.Kind == "appointment-reschedule-proposed" || m.Kind == "appointment-cancelled-by-customer"
		if toProfessional != (m.To == agent) {
			t.Fatalf("%s went to %s", m.Kind, m.To)
		}
		body := m.Subject + m.Text + m.HTML
		// No contact details of the other party, in either direction.
		for _, banned := range []string{consumerPhone, guestPhone, "+263 77 555 0000", "agent@example.test", identity.SeedConsumerEmail, guestEmail} {
			if strings.Contains(body, banned) {
				t.Fatalf("%s contains %q:\n%s", m.Kind, banned, m.Text)
			}
		}
		if !strings.Contains(m.Text, "(Central Africa Time)") {
			t.Fatalf("%s does not name the time zone:\n%s", m.Kind, m.Text)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("emails = %v, want %v", got, want)
	}
	for kind, n := range want {
		if got[kind] != n {
			t.Fatalf("%d %s emails, want %d (all: %v)", got[kind], kind, n, got)
		}
	}
	for _, m := range sender.sent {
		switch {
		case m.Kind == "appointment-requested" && strings.Contains(m.Text, a.Reference):
			if !strings.Contains(m.Text, "Requested time: Tuesday 17 November 2026, 10:00 (Central Africa Time)") || !strings.Contains(m.Text, "Status: Requested") {
				t.Fatalf("requested email:\n%s", m.Text)
			}
		case m.Kind == "appointment-confirmed" && m.To == guestEmail:
			if !strings.Contains(m.Text, guest.Reference) || !strings.Contains(m.Text, "With: Tendai Ncube") {
				t.Fatalf("guest confirmation:\n%s", m.Text)
			}
		case m.Kind == "appointment-new-time-proposed":
			if !strings.Contains(m.Text, "Proposed time: Thursday 19 November 2026, 10:00 (Central Africa Time)") {
				t.Fatalf("proposal email:\n%s", m.Text)
			}
		case m.Kind == "appointment-cancelled":
			if !strings.Contains(m.Text, "Reason: The owner is away that week") || !strings.Contains(m.Text, "Status: Cancelled") {
				t.Fatalf("cancellation email:\n%s", m.Text)
			}
		}
	}
}

func TestSampleAppointments(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	devseed.SeedMemoryAppointments(e.appts, e.now)
	if n := len(e.appts.All()); n != devseed.AppointmentCount {
		t.Fatalf("%d sample appointments, want %d", n, devseed.AppointmentCount)
	}
	counts := map[string]int{
		identity.SeedAgentEmail: 4, devseed.EstatesAgent2Email: 2, devseed.EstatesOwnerEmail: 6, lawFirmEmail: 1, devseed.CommercialAgentEmail: 0,
	}
	for who, want := range counts {
		if got := len(c.inboxIDs(who, "?page_size=100")); got != want {
			t.Fatalf("%s sees %d sample appointments, want %d", who, got, want)
		}
	}
	upcoming := decode[apptPage](t, e.get("/v1/me/appointments", c.as(identity.SeedConsumerEmail)))
	past := decode[apptPage](t, e.get("/v1/me/appointments?when=past", c.as(identity.SeedConsumerEmail)))
	if upcoming.Total != 5 || past.Total != 1 || past.Items[0].Status != "COMPLETED" {
		t.Fatalf("consumer: %d upcoming, %d past", upcoming.Total, past.Total)
	}
	// The sample reschedule can be accepted by the customer.
	c.mustAct(identity.SeedConsumerEmail, "/v1/me/appointments/"+devseed.SampleAppointmentID(3).String()+"/accept", nil)
}
