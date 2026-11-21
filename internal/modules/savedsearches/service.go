package savedsearches

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Store persists saved searches.
type Store interface {
	List(ctx context.Context, userID uuid.UUID) ([]SavedSearch, error)
	Get(ctx context.Context, id uuid.UUID) (SavedSearch, error)
	// Create stores a search; ErrLimit when the user already has MaxPerUser
	// (checked under a lock).
	Create(ctx context.Context, s SavedSearch) error
	// Update writes name, frequency, the watermark, next check and updated_at.
	Update(ctx context.Context, s SavedSearch) error
	// Delete removes the user's search (ErrNotFound for another's).
	Delete(ctx context.Context, userID, id uuid.UUID) error
	// Unsubscribe switches the search with that token OFF and returns it
	// (ErrInvalidToken when none has it).
	Unsubscribe(ctx context.Context, token string, now time.Time) (SavedSearch, error)
	// Claim returns up to limit searches whose next check is due at now and
	// pushes their next check to now+lease, so no other worker takes them.
	Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]SavedSearch, error)
	// Record stores a run: the watermark becomes checkedAt, the next check
	// is checkedAt + the CURRENT frequency's interval, and, with an alert,
	// last_alert_at and one EventMatched are written in the same
	// transaction.
	Record(ctx context.Context, id uuid.UUID, checkedAt time.Time, alert *Alert) error
}

// Checker validates a search's query string and returns its canonical form,
// or per-parameter messages.
type Checker interface {
	Canonical(ctx context.Context, query string) (string, map[string]string, error)
}

// Matcher runs a saved search for listings published in (after, upto].
type Matcher interface {
	Match(ctx context.Context, query string, after, upto time.Time) (Alert, error)
}

// Deps is what the Service needs. Checker is needed by the API, Matcher by
// the worker.
type Deps struct {
	Store   Store
	Checker Checker
	Matcher Matcher
	Now     func() time.Time
}

// Service holds the rules.
type Service struct{ d Deps }

// NewService builds the Service.
func NewService(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{d: d}
}

func (s *Service) now() time.Time { return s.d.Now().UTC().Truncate(time.Microsecond) }

// Input is the body of POST and PATCH /v1/me/saved-searches.
type Input struct {
	Name      *string `json:"name"`
	Query     *string `json:"query"`
	Frequency *string `json:"frequency"`
}

func cleanName(raw string) (string, string) {
	name := strings.Join(strings.Fields(raw), " ")
	switch {
	case name == "":
		return "", "is required"
	case len([]rune(name)) > MaxNameRunes:
		return "", fmt.Sprintf("must be at most %d characters", MaxNameRunes)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", "must not contain control characters"
	}
	return name, ""
}

func nextCheck(frequency string, from time.Time) *time.Time {
	if d := Interval(frequency); d > 0 {
		t := from.Add(d)
		return &t
	}
	return nil
}

func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// List returns the user's saved searches, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]SavedSearch, error) {
	return s.d.Store.List(ctx, userID)
}

// Create saves a search for the user.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, in Input) (SavedSearch, error) {
	f := map[string]string{}
	now := s.now()
	ss := SavedSearch{UserID: userID, Frequency: FrequencyDaily, LastCheckedAt: now, CreatedAt: now, UpdatedAt: now}
	if in.Name == nil {
		f["name"] = "is required"
	} else if name, msg := cleanName(*in.Name); msg != "" {
		f["name"] = msg
	} else {
		ss.Name = name
	}
	if in.Frequency != nil {
		if !slices.Contains(Frequencies(), *in.Frequency) {
			f["frequency"] = "must be one of " + strings.Join(Frequencies(), ", ")
		} else {
			ss.Frequency = *in.Frequency
		}
	}
	switch {
	case in.Query == nil:
		f["query"] = "is required"
	case len(*in.Query) > MaxQueryLength:
		f["query"] = fmt.Sprintf("must be at most %d characters", MaxQueryLength)
	default:
		canonical, fields, err := s.d.Checker.Canonical(ctx, *in.Query)
		if err != nil {
			return SavedSearch{}, err
		}
		for k, v := range fields {
			f["query."+k] = v
		}
		ss.Query = canonical
	}
	if len(f) > 0 {
		return SavedSearch{}, &ValidationError{Fields: f}
	}
	var err error
	if ss.ID, err = uuid.NewV7(); err != nil {
		return SavedSearch{}, err
	}
	if ss.UnsubscribeToken, err = newToken(); err != nil {
		return SavedSearch{}, err
	}
	ss.NextCheckAt = nextCheck(ss.Frequency, now)
	if err := s.d.Store.Create(ctx, ss); err != nil {
		return SavedSearch{}, err
	}
	return ss, nil
}

// Update renames a search or changes its frequency.
func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in Input) (SavedSearch, error) {
	ss, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return SavedSearch{}, err
	}
	if ss.UserID != userID {
		return SavedSearch{}, ErrNotFound
	}
	f := map[string]string{}
	if in.Query != nil {
		f["query"] = "cannot be changed; save a new search instead"
	}
	if in.Name != nil {
		if name, msg := cleanName(*in.Name); msg != "" {
			f["name"] = msg
		} else {
			ss.Name = name
		}
	}
	now := s.now()
	if in.Frequency != nil {
		switch {
		case !slices.Contains(Frequencies(), *in.Frequency):
			f["frequency"] = "must be one of " + strings.Join(Frequencies(), ", ")
		case *in.Frequency != ss.Frequency:
			if ss.Frequency == FrequencyOff {
				// Switched back on: alert about listings from now, never the backlog.
				ss.LastCheckedAt = now
			}
			ss.Frequency = *in.Frequency
			ss.NextCheckAt = nextCheck(ss.Frequency, now)
		}
	}
	if len(f) > 0 {
		return SavedSearch{}, &ValidationError{Fields: f}
	}
	ss.UpdatedAt = now
	if err := s.d.Store.Update(ctx, ss); err != nil {
		return SavedSearch{}, err
	}
	return ss, nil
}

// Delete removes one of the user's searches.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.d.Store.Delete(ctx, userID, id)
}

// Unsubscribe switches off the alerts of the search behind token.
func (s *Service) Unsubscribe(ctx context.Context, token string) (SavedSearch, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 100 {
		return SavedSearch{}, ErrInvalidToken
	}
	return s.d.Store.Unsubscribe(ctx, token, s.now())
}

// Sweep runs the due searches once and returns how many alerts it wrote.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	if s.d.Matcher == nil {
		return 0, errors.New("savedsearches: no matcher")
	}
	now := s.now()
	due, err := s.d.Store.Claim(ctx, now, claimLease, sweepBatch)
	if err != nil {
		return 0, fmt.Errorf("claim saved searches: %w", err)
	}
	alerts := 0
	var errs []error
	for _, ss := range due {
		found, err := s.d.Matcher.Match(ctx, ss.Query, ss.LastCheckedAt, now)
		if err != nil {
			// Left claimed: tried again when the lease ends.
			errs = append(errs, fmt.Errorf("match %s: %w", ss.ID, err))
			continue
		}
		var alert *Alert
		if found.Total > 0 {
			if len(found.Lines) > MaxLines {
				found.Lines = found.Lines[:MaxLines]
			}
			alert = &found
			alerts++
		}
		if err := s.d.Store.Record(ctx, ss.ID, now, alert); err != nil {
			errs = append(errs, fmt.Errorf("record %s: %w", ss.ID, err))
		}
	}
	return alerts, errors.Join(errs...)
}
