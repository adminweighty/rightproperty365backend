package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/notifications"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/ics"
	"rightproperty365.com/backend/internal/platform/mail"
)

// icsProps unfolds a calendar file and returns its properties by name (the
// part before the first ':' or ';'), last value wins.
func icsProps(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	s := string(raw)
	if strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\n") {
		t.Fatalf("not CRLF:\n%q", s)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n ", ""), "\r\n") {
		i := strings.IndexAny(line, ":;")
		if i < 0 {
			continue
		}
		out[line[:i]] = line
	}
	return out
}

// invite is the one calendar attachment of an email ("" when none).
func invite(t *testing.T, m mail.Message) []byte {
	t.Helper()
	switch len(m.Attachments) {
	case 0:
		return nil
	case 1:
		a := m.Attachments[0]
		if !strings.HasPrefix(a.ContentType, "text/calendar; charset=utf-8; method=") || !strings.HasSuffix(a.Filename, ".ics") {
			t.Fatalf("%s attachment %s (%s)", m.Kind, a.Filename, a.ContentType)
		}
		return a.Content
	}
	t.Fatalf("%s has %d attachments", m.Kind, len(m.Attachments))
	return nil
}

func calendarHandlers(e *env) (*notifications.Handlers, *captureSender) {
	e.t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	renderer, err := mail.NewRenderer(testOrigin)
	if err != nil {
		e.t.Fatal(err)
	}
	sender := &captureSender{}
	h := notifications.NewHandlers(mail.NewService(sender, mail.NewMemoryDeliveryLog(), logger), renderer,
		notifications.NewMemorySource(e.identity, e.requests, e.enquiries), logger)
	h.SetAppointmentSource(e.appts.Get)
	h.SetVenueSource(VenueNotices(e.venues, e.listings, e.marketStore, e.reference, logger))
	h.SetCalendar(ics.Organizer(config.DefaultEmailFrom), e.appts.Timeline)
	return h, sender
}

// Confirm -> customer's new time -> confirmed -> cancelled by the customer:
// one UID, a rising SEQUENCE, REQUEST then CANCEL; the downloads answer the
// two parties only, and no file carries the other party's contact details.
func TestAppointmentCalendarInvitations(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	consumer, agent, otherAgency := identity.SeedConsumerEmail, identity.SeedAgentEmail, devseed.CommercialAgentEmail
	handlers, sender := calendarHandlers(e)

	tick := func() { e.now = e.now.Add(time.Minute) }
	start := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC) // 10:00 in Harare
	in := e.appointmentInput(1, 0)
	in["start_utc"] = start.Format(time.RFC3339)
	a := c.book(consumer, in)
	mine, inbox := "/v1/me/appointments/"+a.ID, "/v1/inbox/appointments/"+a.ID
	tick()
	c.mustAct(agent, inbox+"/confirm", obj{"location_note": "Meet at the gate, call +263 77 555 0000 or agent@example.test"})
	tick()
	moved := start.Add(48 * time.Hour)
	c.mustAct(consumer, mine+"/reschedule", obj{"start_utc": moved.Format(time.RFC3339)})
	tick()
	c.mustAct(agent, inbox+"/confirm", nil)
	tick()
	c.mustAct(consumer, mine+"/cancel", obj{"reason": "Found another place"})
	// Cancelled before it was ever confirmed: no calendar entry to remove.
	tick()
	b := c.book(consumer, e.appointmentInput(2, 30*time.Hour))
	tick()
	c.mustAct(consumer, "/v1/me/appointments/"+b.ID+"/cancel", nil)

	for round := 0; round < 2; round++ { // a re-run sends nothing twice
		for _, ev := range e.appts.Outbox() {
			if err := handlers.Handle(context.Background(), ev); err != nil {
				t.Fatalf("%s: %v", ev.Type, err)
			}
		}
	}

	uid := "UID:appointment-" + a.ID + "@rightproperty365.com"
	type step struct{ kind, to, method, seq, start string }
	want := []step{
		{"appointment-confirmed", consumer, "REQUEST", "SEQUENCE:1", "DTSTART:20261117T080000Z"},
		{"appointment-confirmed", consumer, "REQUEST", "SEQUENCE:3", "DTSTART:20261119T080000Z"},
		{"appointment-cancelled-by-customer", agent, "CANCEL", "SEQUENCE:4", "DTSTART:20261119T080000Z"},
	}
	var got []step
	for _, m := range sender.sent {
		raw := invite(t, m)
		if raw == nil {
			continue
		}
		p := icsProps(t, raw)
		if p["UID"] != uid {
			t.Fatalf("%s: %s, want %s", m.Kind, p["UID"], uid)
		}
		got = append(got, step{m.Kind, m.To, strings.TrimPrefix(p["METHOD"], "METHOD:"), p["SEQUENCE"], p["DTSTART"]})
		body := string(raw)
		// Only the recipient's own address; never the other party's details.
		banned := []string{consumerPhone, "+263 77 555 0000", "agent@example.test"}
		if m.To == agent {
			banned = []string{consumerPhone, consumer, "Viewing 1 please", "+263 77 555 0000"}
			if !strings.Contains(p["ATTENDEE"], "mailto:"+agent) {
				t.Fatalf("attendee %q", p["ATTENDEE"])
			}
		} else if !strings.Contains(p["ATTENDEE"], "mailto:"+consumer) {
			t.Fatalf("attendee %q", p["ATTENDEE"])
		}
		for _, s := range banned {
			if strings.Contains(strings.ReplaceAll(body, "\r\n ", ""), s) {
				t.Fatalf("%s invitation contains %q:\n%s", m.Kind, s, body)
			}
		}
		if p["ORGANIZER"] != `ORGANIZER;CN="RightProperty365":mailto:no-reply@rightproperty365.com` {
			t.Fatalf("organizer %q", p["ORGANIZER"])
		}
		wantStatus := "STATUS:CONFIRMED"
		if p["METHOD"] == "METHOD:CANCEL" {
			wantStatus = "STATUS:CANCELLED"
		}
		if p["STATUS"] != wantStatus || p["X-WR-TIMEZONE"] != "X-WR-TIMEZONE:Africa/Harare" {
			t.Fatalf("%s: %s %s", m.Kind, p["STATUS"], p["X-WR-TIMEZONE"])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("invitations = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("invitation %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, m := range sender.sent {
		if (m.Kind == "appointment-requested" || m.Kind == "appointment-reschedule-proposed") && len(m.Attachments) > 0 {
			t.Fatalf("%s carries an attachment", m.Kind)
		}
	}

	// Downloads.
	rec := e.get(mine+"/calendar.ics", c.as(consumer))
	wantStatus(t, rec, http.StatusOK)
	h := rec.Header()
	if h.Get("Content-Type") != "text/calendar; charset=utf-8" || h.Get("Cache-Control") != "private, no-store" ||
		h.Get("Content-Disposition") != `attachment; filename="appointment-`+a.Reference+`.ics"` {
		t.Fatalf("headers %v", h)
	}
	p := icsProps(t, rec.Body.Bytes())
	if p["METHOD"] != "METHOD:PUBLISH" || p["UID"] != uid || p["SEQUENCE"] != "SEQUENCE:4" || p["STATUS"] != "STATUS:CANCELLED" || p["ATTENDEE"] != "" {
		t.Fatalf("customer download:\n%s", rec.Body.String())
	}
	if !strings.Contains(p["URL"], testOrigin+"/account/appointments") {
		t.Fatalf("url %q", p["URL"])
	}
	rec = e.get(inbox+"/calendar.ics", c.as(agent))
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); strings.Contains(body, consumer) || strings.Contains(body, consumerPhone) || !strings.Contains(body, uid) ||
		!strings.Contains(body, testOrigin+"/agent/appointments") {
		t.Fatalf("professional download:\n%s", body)
	}
	// Anybody else: 404 (also a caller with no professional scope at all).
	wantStatus(t, e.get(mine+"/calendar.ics", c.as(agent)), http.StatusNotFound)
	wantStatus(t, e.get(inbox+"/calendar.ics", c.as(consumer)), http.StatusNotFound)
	wantStatus(t, e.get(inbox+"/calendar.ics", c.as(otherAgency)), http.StatusNotFound)
	wantStatus(t, e.get("/v1/me/appointments/not-a-uuid/calendar.ics", c.as(consumer)), http.StatusNotFound)
	wantStatus(t, e.get(mine+"/calendar.ics"), http.StatusUnauthorized)
}

// A venue booking: the host confirms (REQUEST to the customer), the customer
// cancels (CANCEL to the host), same UID and a higher SEQUENCE; the downloads
// answer the customer and the host only.
func TestVenueBookingCalendarInvitations(t *testing.T) {
	e := newEnv(t)
	handlers, sender := calendarHandlers(e)
	owner := e.loginCookie(showroomHost, seedPassword)
	tenant := e.loginCookie(devseed.TenantEmail, seedPassword)
	consumer := e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	agent := e.loginCookie(identity.SeedAgentEmail, seedPassword)
	id := devseed.TenantRequestedBookingID.String()
	orgPath := "/v1/agent/organisations/" + showroomOrg(t).String() + "/venue-bookings/" + id

	e.now = e.now.Add(time.Minute)
	wantStatus(t, e.post(orgPath+"/confirm", nil, browser(owner)), http.StatusOK)
	e.now = e.now.Add(time.Minute)
	wantStatus(t, e.post("/v1/me/venue-bookings/"+id+"/cancel", obj{"reason": "Plans changed"}, browser(tenant)), http.StatusOK)
	for _, ev := range e.venues.Outbox() {
		if err := handlers.Handle(context.Background(), ev); err != nil {
			t.Fatalf("%s: %v", ev.Type, err)
		}
	}
	uid := "UID:venue-booking-" + id + "@rightproperty365.com"
	var seen []string
	for _, m := range sender.sent {
		raw := invite(t, m)
		if raw == nil {
			continue
		}
		p := icsProps(t, raw)
		if p["UID"] != uid || p["LOCATION"] != "" {
			t.Fatalf("%s:\n%s", m.Kind, raw)
		}
		if m.To == showroomHost && strings.Contains(string(raw), devseed.TenantEmail) {
			t.Fatalf("the host's invitation names the customer:\n%s", raw)
		}
		seen = append(seen, m.Kind+" "+m.To+" "+p["METHOD"]+" "+p["SEQUENCE"]+" "+p["URL"])
	}
	want := []string{
		"venue-booking-confirmed " + devseed.TenantEmail + " METHOD:REQUEST SEQUENCE:1 URL:" + testOrigin + "/account/venue-bookings",
		"venue-booking-cancelled-by-customer " + showroomHost + " METHOD:CANCEL SEQUENCE:2 URL:" + testOrigin + "/agent/venue-bookings",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("invitations:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}

	rec := e.get("/v1/me/venue-bookings/"+id+"/calendar.ics", browser(tenant))
	wantStatus(t, rec, http.StatusOK)
	if p := icsProps(t, rec.Body.Bytes()); p["METHOD"] != "METHOD:PUBLISH" || p["UID"] != uid || p["STATUS"] != "STATUS:CANCELLED" {
		t.Fatalf("customer download:\n%s", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "text/calendar; charset=utf-8" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), `attachment; filename="venue-booking-`) {
		t.Fatalf("headers %v", rec.Header())
	}
	rec = e.get(orgPath+"/calendar.ics", browser(owner))
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), devseed.TenantEmail) {
		t.Fatalf("host download names the customer:\n%s", rec.Body.String())
	}
	wantStatus(t, e.get("/v1/me/venue-bookings/"+id+"/calendar.ics", browser(consumer)), http.StatusNotFound)
	wantStatus(t, e.get(orgPath+"/calendar.ics", browser(agent)), http.StatusNotFound)
	wantStatus(t, e.get("/v1/me/hosted-venue-bookings/"+id+"/calendar.ics", browser(consumer)), http.StatusNotFound)
	wantStatus(t, e.get("/v1/me/hosted-venue-bookings/"+id+"/calendar.ics", browser(owner)), http.StatusNotFound)
}
