package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/notifications"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/mail"
)

type slotsBody struct {
	Configured  bool   `json:"configured"`
	Timezone    string `json:"timezone"`
	SlotMinutes *int   `json:"slot_minutes"`
	Services    []struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		DurationMinutes int    `json:"duration_minutes"`
		Channel         string `json:"channel"`
	} `json:"services"`
	Days []struct {
		Date  string `json:"date"`
		Slots []struct {
			StartUTC time.Time `json:"start_utc"`
			EndUTC   time.Time `json:"end_utc"`
		} `json:"slots"`
	} `json:"days"`
}

type serviceBody struct {
	Service struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		DurationMinutes int    `json:"duration_minutes"`
		Channel         string `json:"channel"`
		Active          bool   `json:"active"`
	} `json:"service"`
}

type apptService struct {
	Appointment struct {
		ID              string `json:"id"`
		DurationMinutes int    `json:"duration_minutes"`
		Service         *struct {
			ID      *string `json:"id"`
			Name    string  `json:"name"`
			Channel string  `json:"channel"`
		} `json:"service"`
	} `json:"appointment"`
}

// API 0.51.0: availability, consultation services, offered slots and reminders.
func TestAppointmentAvailability(t *testing.T) {
	e := newEnv(t) // the clock: Sunday 1 November 2026, 09:00 UTC
	c := newCast(e)
	consumer, agent := identity.SeedConsumerEmail, identity.SeedAgentEmail
	listing := propertyrequests.SeedListingID(1).String()
	slotsOf := func(query string) slotsBody {
		t.Helper()
		rec := e.get("/v1/appointments/slots?" + query)
		wantStatus(t, rec, http.StatusOK)
		return decode[slotsBody](t, rec)
	}

	// Off by default: no slots, the free time picker stays.
	if s := slotsOf("type=VIEWING&listing_id=" + listing); s.Configured || len(s.Days) != 0 || s.Timezone != "Africa/Harare" {
		t.Fatalf("off: %+v", s)
	}
	wantError(t, e.get("/v1/me/appointment-settings", c.as(consumer)), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get("/v1/me/appointment-settings"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.put("/v1/me/appointment-settings", obj{"enabled": true, "timezone": "Africa/Harare"}, c.as(agent)),
		http.StatusBadRequest, httpx.CodeValidationFailed)

	hours := []obj{}
	for d := 1; d <= 7; d++ {
		hours = append(hours, obj{"weekday": d, "opens": "09:00", "closes": "12:00"})
	}
	rec := e.put("/v1/me/appointment-settings", obj{"enabled": true, "timezone": "Africa/Harare", "working_hours": hours,
		"exceptions":   []obj{{"date": "2026-11-03", "closed": true, "note": "Public holiday"}},
		"slot_minutes": 30, "buffer_minutes": 0, "lead_time_hours": 0, "horizon_days": 30}, c.as(agent))
	wantStatus(t, rec, http.StatusOK)
	rec = e.get("/v1/me/appointment-settings", c.as(agent))
	wantStatus(t, rec, http.StatusOK)
	if s := decode[obj](t, rec); s["enabled"] != true || s["slot_minutes"] != float64(30) {
		t.Fatalf("settings: %v", s)
	}

	// Monday 2 November: 09:00 to 12:00 in Harare (07:00 to 10:00 UTC), six
	// half hours; Tuesday is a closed exception.
	s := slotsOf("type=VIEWING&listing_id=" + listing + "&from=2026-11-02&days=2")
	if !s.Configured || *s.SlotMinutes != 30 || len(s.Days) != 2 || len(s.Days[0].Slots) != 6 || len(s.Days[1].Slots) != 0 ||
		s.Days[0].Slots[0].StartUTC.Format(time.RFC3339) != "2026-11-02T07:00:00Z" {
		t.Fatalf("slots: %+v", s)
	}
	wantError(t, e.get("/v1/appointments/slots?type=VIEWING&listing_id="+listing+"&days=90"), http.StatusBadRequest, httpx.CodeValidationFailed)

	// A time that is not offered is refused; an offered one is booked and
	// leaves the list.
	in := e.appointmentInput(1, 0)
	in["start_utc"] = "2026-11-02T07:10:00Z"
	if f := wantError(t, e.post("/v1/appointments", in, c.as(consumer)), http.StatusBadRequest, httpx.CodeValidationFailed).Error.Fields; f["start_utc"] == "" {
		t.Fatalf("fields: %v", f)
	}
	in = e.appointmentInput(1, 0)
	in["start_utc"], in["duration_minutes"] = "2026-11-02T07:00:00Z", 90
	viewing := c.book(consumer, in)
	if viewing.DurationMinutes != 30 {
		t.Fatalf("the slot length wins: %d", viewing.DurationMinutes)
	}
	if s := slotsOf("type=VIEWING&listing_id=" + listing + "&from=2026-11-02&days=1"); len(s.Days[0].Slots) != 5 ||
		s.Days[0].Slots[0].StartUTC.Format(time.RFC3339) != "2026-11-02T07:30:00Z" {
		t.Fatalf("after booking: %+v", s.Days[0].Slots)
	}

	// A viewing request's preferred slot follows the same rule.
	req := e.viewingRequest(1, 0)
	req["preferred_slot_utc"] = "2026-11-02T07:00:00Z" // taken
	if f := wantError(t, e.post("/v1/property-requests", req), http.StatusBadRequest, httpx.CodeValidationFailed).Error.Fields; f["preferred_slot_utc"] == "" {
		t.Fatalf("fields: %v", f)
	}
	req = e.viewingRequest(1, 0)
	req["preferred_slot_utc"] = "2026-11-02T08:00:00Z"
	wantStatus(t, e.post("/v1/property-requests", req), http.StatusCreated)

	// The customer's counter-proposal must be offered too.
	mine := "/v1/me/appointments/" + viewing.ID
	wantError(t, c.act(consumer, mine+"/reschedule", obj{"start_utc": "2026-11-03T07:00:00Z"}), http.StatusBadRequest, httpx.CodeValidationFailed)
	c.mustAct(consumer, mine+"/reschedule", obj{"start_utc": "2026-11-02T09:00:00Z"})

	// Consultation services: free only, the owner's alone.
	price := e.post("/v1/me/consultation-services", obj{"name": "Valuation", "duration_minutes": 60, "channel": "VIDEO", "price": "20.00"}, c.as(agent))
	wantError(t, price, http.StatusBadRequest, httpx.CodeValidationFailed)
	rec = e.post("/v1/me/consultation-services", obj{"name": "Valuation", "duration_minutes": 60, "channel": "VIDEO", "notes": "We look at the market."}, c.as(agent))
	wantStatus(t, rec, http.StatusCreated)
	service := decode[serviceBody](t, rec).Service
	wantError(t, e.patch("/v1/me/consultation-services/"+service.ID, obj{"name": "Mine now"}, c.as(identity.SeedAgentBEmail)),
		http.StatusNotFound, appointments.CodeServiceNotFound)

	s = slotsOf("type=CONSULTATION&recipient_kind=agents&recipient_slug=tendai-ncube&from=2026-11-04&days=1&service_id=" + service.ID)
	if !s.Configured || len(s.Services) != 1 || s.Services[0].Name != "Valuation" || *s.SlotMinutes != 60 {
		t.Fatalf("consultation slots: %+v", s)
	}
	consult := e.appointmentInput(1, 0)
	delete(consult, "listing_id")
	consult["type"], consult["recipient"] = "CONSULTATION", obj{"kind": "agents", "slug": "tendai-ncube"}
	consult["start_utc"], consult["service_id"] = s.Days[0].Slots[0].StartUTC.Format(time.RFC3339), service.ID
	rec = e.post("/v1/appointments", consult, c.as(consumer))
	wantStatus(t, rec, http.StatusCreated)
	if got := decode[apptService](t, rec).Appointment; got.DurationMinutes != 60 || got.Service == nil || got.Service.Name != "Valuation" ||
		got.Service.Channel != "VIDEO" {
		t.Fatalf("consultation: %+v", got)
	}
	viewingWithService := e.appointmentInput(1, 0)
	viewingWithService["start_utc"], viewingWithService["service_id"] = "2026-11-02T09:30:00Z", service.ID
	wantError(t, e.post("/v1/appointments", viewingWithService, c.as(consumer)), http.StatusBadRequest, httpx.CodeValidationFailed)

	// Archived: no longer offered.
	wantStatus(t, e.deleteReq("/v1/me/consultation-services/"+service.ID, c.as(agent)), http.StatusNoContent)
	if s := slotsOf("type=CONSULTATION&recipient_kind=agents&recipient_slug=tendai-ncube&from=2026-11-02&days=1"); len(s.Services) != 0 {
		t.Fatalf("archived: %+v", s.Services)
	}

	// Reminders: the confirmed appointment a day ahead, once, to both.
	c.mustAct(agent, "/v1/inbox/appointments/"+viewing.ID+"/confirm", nil) // accepts 09:00 UTC on 2 November
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	renderer, err := mail.NewRenderer(testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	sender := &captureSender{}
	handlers := notifications.NewHandlers(mail.NewService(sender, mail.NewMemoryDeliveryLog(), logger), renderer,
		notifications.NewMemorySource(e.identity, e.requests, e.enquiries), logger)
	handlers.SetAppointmentSource(e.appts.Get)
	sweeper := appointments.NewService(appointments.Deps{Store: e.appts, Now: func() time.Time { return e.now }})
	for _, at := range []time.Time{e.now, e.now.Add(time.Hour)} { // within a day of 09:00 on the 2nd both times
		e.now = at
		if _, err := sweeper.SweepReminders(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	reminders := 0
	for _, ev := range e.appts.Outbox() {
		if ev.Type != appointments.EventReminder {
			continue
		}
		reminders++
		if err := handlers.Handle(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if reminders != 1 {
		t.Fatalf("%d reminder events, want 1", reminders)
	}
	got := map[string]string{}
	for _, m := range sender.sent {
		got[m.Kind] = m.To
	}
	if got["appointment-reminder"] != consumer || got["appointment-reminder-professional"] != agent {
		t.Fatalf("reminders went to %v", got)
	}
}
