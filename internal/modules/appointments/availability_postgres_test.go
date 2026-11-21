package appointments

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/venues"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database. Skipped when
// unset; never read from .env.
func openTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("RP365_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RP365_TEST_DATABASE_URL is not set: the Postgres store is only exercised against a throwaway database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.NewRunner(conn, migs).Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type availabilityRig interface {
	Store
	AvailabilityStore
	ReminderStore
}

// availabilityContract checks settings, services, busy time and reminders
// on a store. user is an existing account; market an existing market;
// reminders counts the reminder events written for an appointment.
func availabilityContract(t *testing.T, s availabilityRig, user, market uuid.UUID, reminders func(id uuid.UUID) int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, ok, err := s.Settings(ctx, user); err != nil || ok {
		t.Fatalf("no settings yet: %v %v", ok, err)
	}
	opens, closes, note := "13:00", "15:00", "Half day"
	st := Settings{UserID: user, Enabled: true, Timezone: "Africa/Harare", SlotMinutes: 45, BufferMinutes: 10, LeadTimeHours: 2,
		HorizonDays: 20, MaxPerDay: 4, UpdatedAt: &now,
		WorkingHours: []venues.OpeningRange{{Weekday: 1, Opens: "09:00", Closes: "17:00"}},
		Exceptions:   []venues.Exception{{Date: "2027-01-04", Opens: &opens, Closes: &closes, Note: &note}}}
	for range 2 { // an update replaces
		if err := s.SaveSettings(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.Settings(ctx, user)
	if err != nil || !ok || !got.Enabled || got.SlotMinutes != 45 || got.MaxPerDay != 4 || len(got.WorkingHours) != 1 ||
		len(got.Exceptions) != 1 || *got.Exceptions[0].Opens != "13:00" || *got.Exceptions[0].Note != "Half day" {
		t.Fatalf("settings: %+v %v %v", got, ok, err)
	}

	// Services: newest first, archived last; another id is not found.
	first := ConsultationService{ID: uuid.New(), UserID: user, Name: "First meeting", DurationMinutes: 30, Channel: ChannelVideo,
		Active: true, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	second := ConsultationService{ID: uuid.New(), UserID: user, Name: "Valuation", DurationMinutes: 60, Channel: ChannelInPerson,
		Location: "Our office", Notes: "Bring the title deeds.", Active: true, CreatedAt: now, UpdatedAt: now}
	for _, c := range []ConsultationService{first, second} {
		if err := s.SaveService(ctx, c, true); err != nil {
			t.Fatal(err)
		}
	}
	first.Active = false
	if err := s.SaveService(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	list, err := s.Services(ctx, user)
	if err != nil || len(list) != 2 || list[0].ID != second.ID || list[1].Active || list[0].Location != "Our office" {
		t.Fatalf("services: %+v %v", list, err)
	}
	if _, err := s.Service(ctx, uuid.New()); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("unknown service: %v", err)
	}
	if err := s.SaveService(ctx, ConsultationService{ID: uuid.New(), UserID: user, Name: "x", DurationMinutes: 30, Channel: ChannelPhone,
		CreatedAt: now, UpdatedAt: now}, false); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("update of a missing service: %v", err)
	}

	// An appointment for the user, two hours from now: busy; confirmed:
	// one reminder for that start, then a new one after it moves.
	start := now.Add(2 * time.Hour)
	svc := second.ID
	a := Appointment{ID: uuid.New(), Reference: "AV" + uuid.NewString()[:6], Type: TypeConsultation, Status: StatusRequested, MarketID: market,
		Timezone: "Africa/Harare", ProfileKind: "AGENT", ProfileSlug: "availability-test", CustomerName: "Tendai", CustomerEmail: "t@example.test",
		PreferredContactMethod: "EMAIL", RecipientType: RecipientAgent, AssignedUserID: &user, StartUTC: start, DurationMinutes: 60,
		ServiceID: &svc, ServiceName: "Valuation", ServiceChannel: ChannelInPerson, CreatedAt: now, UpdatedAt: now}
	entry := TimelineEntry{ID: uuid.New(), AppointmentID: a.ID, Event: TimelineRequested, ActorType: PartyCustomer, ToStatus: StatusRequested, StartUTC: &start, At: now}
	if _, _, err := s.Create(ctx, a, entry); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Get(ctx, a.ID)
	if err != nil || stored.ServiceName != "Valuation" || stored.ServiceID == nil || *stored.ServiceID != svc || stored.ServiceChannel != ChannelInPerson {
		t.Fatalf("service snapshot: %+v %v", stored, err)
	}
	busy, err := s.BusyFor(ctx, user, now, now.Add(24*time.Hour), uuid.Nil)
	if err != nil || len(busy) != 1 || !busy[0].Start.Equal(start) || !busy[0].End.Equal(start.Add(time.Hour)) {
		t.Fatalf("busy: %+v %v", busy, err)
	}
	if busy, _ := s.BusyFor(ctx, user, now, now.Add(24*time.Hour), a.ID); len(busy) != 0 {
		t.Fatalf("except: %+v", busy)
	}
	if busy, _ := s.BusyFor(ctx, uuid.New(), now, now.Add(24*time.Hour), uuid.Nil); len(busy) != 0 {
		t.Fatalf("another user: %+v", busy)
	}

	move := func(to time.Time) {
		t.Helper()
		_, err := s.Update(ctx, a.ID, func(cur Appointment) (Mutation, error) {
			m, err := confirmAt(cur, to, now)
			m.Entry.ID, m.Entry.AppointmentID, m.Entry.ActorType, m.Entry.FromStatus, m.Entry.ToStatus, m.Entry.At =
				uuid.New(), cur.ID, PartyProfessional, cur.Status, StatusConfirmed, now
			m.Next.UpdatedAt = now
			return m, err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SendReminders(ctx, now, ReminderAhead, 500); err != nil {
		t.Fatal(err)
	}
	if n := reminders(a.ID); n != 0 {
		t.Fatalf("a requested appointment is not reminded: %d", n)
	}
	move(start)
	for range 2 {
		if _, err := s.SendReminders(ctx, now, ReminderAhead, 500); err != nil {
			t.Fatal(err)
		}
	}
	if n := reminders(a.ID); n != 1 {
		t.Fatalf("one reminder per start: %d", n)
	}
	if got, _ := s.Get(ctx, a.ID); got.RemindedForStart == nil || !got.RemindedForStart.Equal(start) {
		t.Fatalf("reminded for: %+v", got.RemindedForStart)
	}
	move(start.Add(3 * time.Hour))
	if _, err := s.SendReminders(ctx, now, ReminderAhead, 500); err != nil {
		t.Fatal(err)
	}
	if n := reminders(a.ID); n != 2 {
		t.Fatalf("a new reminder for the new time: %d", n)
	}
	// Too far ahead: nothing.
	move(start.Add(48 * time.Hour))
	if _, err := s.SendReminders(ctx, now, ReminderAhead, 500); err != nil {
		t.Fatal(err)
	}
	if n := reminders(a.ID); n != 2 {
		t.Fatalf("not yet: %d", n)
	}
}

func TestMemoryAvailabilityContract(t *testing.T) {
	s := NewMemoryStore()
	availabilityContract(t, s, uuid.New(), uuid.New(), func(id uuid.UUID) int {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for _, ev := range s.outbox {
			if ev.AggregateID == id && ev.Type == EventReminder {
				n++
			}
		}
		return n
	})
}

func TestPostgresAvailabilityContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	user := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at, password_hash) VALUES ($1, $2, 'Availability Tester', 'ACTIVE', now(), 'x')`,
		user, "availability."+user.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	var market uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM markets ORDER BY code LIMIT 1`).Scan(&market); err != nil {
		t.Fatalf("a market is needed (seeded by the migrations): %v", err)
	}
	availabilityContract(t, NewPostgresStore(pool), user, market, func(id uuid.UUID) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, id, EventReminder).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}
