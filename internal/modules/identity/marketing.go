package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Marketing consent (API 0.37.1, BR-017, FR-003).
//
// users.marketing_opt_in is the current value; marketing_consent_events is the
// append-only record of every change (migration 0041). Only a real change is
// recorded: setting the value an account already has writes nothing.
//
// No send is marketing today. Every future marketing send (newsletter,
// promotions) MUST call Service.MarketingAllowed for the recipient at send
// time, not when the send is queued.

// CurrentMarketingConsentVersion names the row of marketing_consent_texts
// whose wording clients must show now (served as consent_text). Rows are
// immutable: new wording is a new version (a migration inserts it) and this
// constant then points at it; an opt-in naming an older version is refused
// with 400.
const CurrentMarketingConsentVersion = "marketing-2026-01"

// Statuses of a consent wording. A DRAFT awaits the owner's legal wording;
// approving it means inserting a NEW version with status APPROVED.
const (
	ConsentTextDraft    = "DRAFT"
	ConsentTextApproved = "APPROVED"
)

// MarketingConsentText is one marketing_consent_texts row.
type MarketingConsentText struct {
	Version   string
	Text      string
	Status    string
	CreatedAt time.Time
}

// builtInMarketingConsentTexts are the rows migration 0041 inserts, for the
// memory store. They must match the migration exactly.
func builtInMarketingConsentTexts() []MarketingConsentText {
	return []MarketingConsentText{{
		Version: "marketing-2026-01",
		Text:    "Send me news, tips and offers from RightProperty365 by email. I can stop them at any time.",
		Status:  ConsentTextDraft,
	}}
}

// ErrConsentTextNotFound is returned when a consent wording version does not exist.
var ErrConsentTextNotFound = errors.New("marketing consent text not found")

// Limits of marketing consent.
const (
	// MaxConsentTextVersionLength bounds consent_text_version (characters).
	MaxConsentTextVersionLength = 40
	// MarketingHistoryLimit is the number of records the preferences show.
	MarketingHistoryLimit = 20
)

// Contexts of a marketing consent record. ADMIN (a support correction) and
// UNSUBSCRIBE (a one-click unsubscribe link) are reserved: nothing writes
// them yet.
const (
	MarketingContextRegistration = "REGISTRATION"
	MarketingContextPreferences  = "PREFERENCES"
	MarketingContextMigration    = "MIGRATION"
	MarketingContextAdmin        = "ADMIN"
	MarketingContextUnsubscribe  = "UNSUBSCRIBE"
)

// Messages for the marketing preference fields.
const (
	msgMarketingOptInRequired = "Choose yes or no."
	msgConsentVersionInvalid  = "must be a version of 1 to 40 characters"
	msgConsentVersionStale    = "The consent wording has changed; show the current wording and try again."
)

// MarketingConsentEvent is one marketing_consent_events row.
type MarketingConsentEvent struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	OptIn     bool
	ChangedAt time.Time
	// Source is web, app or api (httpx.SourceFromClient).
	Source  string
	Context string
	// ConsentTextVersion is the wording agreed to: set on opt-in only ("" is
	// null).
	ConsentTextVersion string
}

// MarketingState is an account's current value and its newest records
// (newest first).
type MarketingState struct {
	OptIn   bool
	History []MarketingConsentEvent
}

// Latest is the newest record, or false when the account never chose.
func (m MarketingState) Latest() (MarketingConsentEvent, bool) {
	if len(m.History) == 0 {
		return MarketingConsentEvent{}, false
	}
	return m.History[0], true
}

// MarketingEligibility is what MarketingAllowed judges.
type MarketingEligibility struct {
	Status        string
	EmailVerified bool
	OptIn         bool
}

// MarketingStore persists marketing consent. It is part of Store.
type MarketingStore interface {
	// MarketingState returns the account's current value and at most limit
	// records, newest first (changed_at, then id). ErrNotFound when the
	// account does not exist.
	MarketingState(ctx context.Context, userID uuid.UUID, limit int) (MarketingState, error)
	// ChangeMarketingOptIn sets users.marketing_opt_in to e.OptIn and appends
	// e, in one transaction under a lock on the users row. When the account
	// already has that value it writes nothing and returns false.
	// ErrNotFound when the account does not exist.
	ChangeMarketingOptIn(ctx context.Context, e MarketingConsentEvent) (bool, error)
	// MarketingEligibility returns the account's status, whether its email is
	// verified and its current value. ErrNotFound when it does not exist.
	MarketingEligibility(ctx context.Context, userID uuid.UUID) (MarketingEligibility, error)
	// MarketingConsentText returns one wording version, or
	// ErrConsentTextNotFound.
	MarketingConsentText(ctx context.Context, version string) (MarketingConsentText, error)
}

// registrationMarketingEvent is the record a registration writes when it
// changes the account's value (from nothing or from an earlier pending
// registration): context REGISTRATION, the registration's source, and the
// current wording on opt-in.
func registrationMarketingEvent(u NewUser) (MarketingConsentEvent, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return MarketingConsentEvent{}, fmt.Errorf("generate consent id: %w", err)
	}
	e := MarketingConsentEvent{
		ID: id, UserID: u.ID, OptIn: u.MarketingOptIn, ChangedAt: u.CreatedAt,
		Source: u.MarketingSource, Context: MarketingContextRegistration,
	}
	if e.Source == "" {
		e.Source = sourceAPI
	}
	if e.OptIn {
		e.ConsentTextVersion = u.MarketingConsentVersion
		if e.ConsentTextVersion == "" {
			e.ConsentTextVersion = CurrentMarketingConsentVersion
		}
	}
	return e, nil
}

// sourceAPI is the source when the client did not name itself (see
// httpx.SourceFromClient).
const sourceAPI = "api"

// validMarketingSource reports whether s is web, app or api.
func validMarketingSource(s string) bool { return s == "web" || s == "app" || s == sourceAPI }

// MarketingPreferences returns the caller's marketing consent: the current
// value and up to MarketingHistoryLimit records, newest first.
func (s *Service) MarketingPreferences(ctx context.Context, userID uuid.UUID) (MarketingState, error) {
	return s.store.MarketingState(ctx, userID, MarketingHistoryLimit)
}

// CurrentMarketingConsentText returns the wording clients must show now.
// Its absence is a deployment error (migration 0041 inserts it).
func (s *Service) CurrentMarketingConsentText(ctx context.Context) (MarketingConsentText, error) {
	t, err := s.store.MarketingConsentText(ctx, CurrentMarketingConsentVersion)
	if err != nil {
		return MarketingConsentText{}, fmt.Errorf("current marketing consent text %s: %w", CurrentMarketingConsentVersion, err)
	}
	return t, nil
}

// MarketingConsentText returns one wording version (public), or
// ErrConsentTextNotFound.
func (s *Service) MarketingConsentText(ctx context.Context, version string) (MarketingConsentText, error) {
	if version == "" || utf8.RuneCountInString(version) > MaxConsentTextVersionLength {
		return MarketingConsentText{}, ErrConsentTextNotFound
	}
	return s.store.MarketingConsentText(ctx, version)
}

// MarketingPreferenceInput is a validated PUT /v1/me/marketing-preferences.
type MarketingPreferenceInput struct {
	OptIn bool
	// ConsentTextVersion is the version the client showed; nil when absent
	// (opting in then agrees to the current one). Ignored on opt-out.
	ConsentTextVersion *string
	// Source is web, app or api (from X-Client).
	Source string
}

// SetMarketingPreferences records a change of the caller's marketing consent
// and returns the state after it. Putting the value the account already has
// writes nothing and returns the state unchanged. Opting in must name the
// current wording (or none, which means the current one): any other version
// is a ValidationError on consent_text_version.
func (s *Service) SetMarketingPreferences(ctx context.Context, userID uuid.UUID, in MarketingPreferenceInput) (MarketingState, error) {
	version := ""
	if in.OptIn {
		version = CurrentMarketingConsentVersion
		if in.ConsentTextVersion != nil {
			v := *in.ConsentTextVersion
			switch n := utf8.RuneCountInString(v); {
			case n < 1 || n > MaxConsentTextVersionLength:
				return MarketingState{}, &ValidationError{Fields: map[string]string{"consent_text_version": msgConsentVersionInvalid}}
			case v != CurrentMarketingConsentVersion:
				return MarketingState{}, &ValidationError{Fields: map[string]string{"consent_text_version": msgConsentVersionStale}}
			}
		}
	}
	source := in.Source
	if !validMarketingSource(source) {
		source = sourceAPI
	}
	id, err := uuid.NewV7()
	if err != nil {
		return MarketingState{}, fmt.Errorf("generate consent id: %w", err)
	}
	changed, err := s.store.ChangeMarketingOptIn(ctx, MarketingConsentEvent{
		ID: id, UserID: userID, OptIn: in.OptIn, ChangedAt: s.clock(),
		Source: source, Context: MarketingContextPreferences, ConsentTextVersion: version,
	})
	if err != nil {
		return MarketingState{}, err
	}
	if changed {
		s.logger.InfoContext(ctx, "marketing consent changed",
			slog.String("consent_user_id", userID.String()), slog.Bool("opt_in", in.OptIn), slog.String("source", source))
	}
	return s.store.MarketingState(ctx, userID, MarketingHistoryLimit)
}

// MarketingAllowed reports whether a marketing message may be sent to the
// account NOW: it is ACTIVE, its email address is verified and it has opted
// in to marketing. Every marketing send must call it at send time (not at
// queue time); service and account mail does not (notification preferences
// govern it). An unknown account, or a failure to read it, is false.
func (s *Service) MarketingAllowed(ctx context.Context, userID uuid.UUID) bool {
	e, err := s.store.MarketingEligibility(ctx, userID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.logger.ErrorContext(ctx, "marketing eligibility unavailable", slog.String("error", err.Error()))
		}
		return false
	}
	return e.Status == StatusActive && e.EmailVerified && e.OptIn
}
