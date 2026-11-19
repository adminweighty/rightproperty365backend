package tax

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/i18n"
)

// Service holds the tax rules' administration and lookup.
type Service struct {
	store       Store
	knownMarket func(ctx context.Context, code string) (bool, error)
	now         func() time.Time
}

// NewService builds the service. knownMarket may be nil (any two letters).
func NewService(store Store, knownMarket func(ctx context.Context, code string) (bool, error), now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, knownMarket: knownMarket, now: now}
}

// TaxFor returns the rule in force for market and appliesTo at the moment
// at (the store's rules through the pure TaxFor).
func (s *Service) TaxFor(ctx context.Context, market, appliesTo string, at time.Time) (Rule, bool, error) {
	rules, err := s.store.Rules(ctx, Filter{Market: normaliseMarket(market), AppliesTo: appliesTo, Status: StatusActive})
	if err != nil {
		return Rule{}, false, err
	}
	r, ok := TaxFor(rules, normaliseMarket(market), appliesTo, at.UTC())
	return r, ok, nil
}

// Input is the body of POST and PATCH /v1/admin/tax-rules. For effective_to
// and registration_number_label, an explicit null (Set*) clears the value.
type Input struct {
	MarketCode              *string             `json:"market_code"`
	TaxName                 *string             `json:"tax_name"`
	Rate                    *string             `json:"rate"`
	AppliesTo               *string             `json:"applies_to"`
	Inclusive               *bool               `json:"inclusive"`
	EffectiveFrom           *time.Time          `json:"effective_from"`
	EffectiveTo             Optional[time.Time] `json:"effective_to"`
	RegistrationNumberLabel Optional[string]    `json:"registration_number_label"`
	IsSample                *bool               `json:"is_sample"`
	Notes                   *string             `json:"notes"`
}

// Optional is a JSON field that distinguishes absent, null and a value.
type Optional[T any] struct {
	Set   bool
	Value *T
}

// UnmarshalJSON implements json.Unmarshaler (called only when present).
func (o *Optional[T]) UnmarshalJSON(raw []byte) error {
	o.Set = true
	if string(raw) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

func (s *Service) checkMarket(ctx context.Context, code string, fields map[string]string) error {
	if len(code) != 2 || strings.ToUpper(code) != code {
		fields["market_code"] = i18n.T(ctx, "field.unknown_market", nil)
		return nil
	}
	if s.knownMarket == nil {
		return nil
	}
	ok, err := s.knownMarket(ctx, code)
	if err != nil {
		return err
	}
	if !ok {
		fields["market_code"] = i18n.T(ctx, "field.unknown_market", nil)
	}
	return nil
}

func apply(ctx context.Context, in Input, r *Rule, fields map[string]string, creating bool) {
	if in.TaxName != nil || creating {
		name := ""
		if in.TaxName != nil {
			name = strings.TrimSpace(*in.TaxName)
		}
		switch n := utf8.RuneCountInString(name); {
		case n == 0:
			fields["tax_name"] = i18n.T(ctx, "field.required", nil)
		case n > 40:
			fields["tax_name"] = i18n.T(ctx, "field.too_long", i18n.Args{"max": 40})
		}
		r.TaxName = name
	}
	if in.Rate != nil || creating {
		raw := ""
		if in.Rate != nil {
			raw = *in.Rate
		}
		if rate, ok := ParseRate(raw); ok {
			r.Rate = rate
		} else {
			fields["rate"] = "must be a percent from 0 to 100 as a decimal string with at most 4 decimals (for example \"15\" or \"12.5\")"
		}
	}
	if in.Inclusive != nil {
		r.Inclusive = *in.Inclusive
	}
	if in.EffectiveFrom != nil {
		r.EffectiveFrom = in.EffectiveFrom.UTC()
	} else if creating {
		fields["effective_from"] = i18n.T(ctx, "field.required", nil)
	}
	if in.EffectiveTo.Set {
		r.EffectiveTo = nil
		if in.EffectiveTo.Value != nil {
			t := in.EffectiveTo.Value.UTC()
			r.EffectiveTo = &t
		}
	}
	if r.EffectiveTo != nil && !r.EffectiveTo.After(r.EffectiveFrom) {
		fields["effective_to"] = "must be after effective_from"
	}
	if in.RegistrationNumberLabel.Set {
		r.RegistrationNumberLabel = nil
		if in.RegistrationNumberLabel.Value != nil {
			label := strings.TrimSpace(*in.RegistrationNumberLabel.Value)
			if utf8.RuneCountInString(label) > 80 {
				fields["registration_number_label"] = i18n.T(ctx, "field.too_long", i18n.Args{"max": 80})
			}
			if label != "" {
				r.RegistrationNumberLabel = &label
			}
		}
	}
	if in.IsSample != nil {
		r.IsSample = *in.IsSample
	}
	if in.Notes != nil {
		notes := strings.TrimSpace(*in.Notes)
		if utf8.RuneCountInString(notes) > 1000 {
			fields["notes"] = i18n.T(ctx, "field.too_long", i18n.Args{"max": 1000})
		}
		r.Notes = notes
	}
}

func ruleState(r Rule) map[string]any {
	m := map[string]any{"market_code": r.MarketCode, "tax_name": r.TaxName, "rate": r.Rate, "applies_to": r.AppliesTo,
		"inclusive": r.Inclusive, "effective_from": r.EffectiveFrom.Format(time.RFC3339), "status": r.Status, "is_sample": r.IsSample}
	if r.EffectiveTo != nil {
		m["effective_to"] = r.EffectiveTo.Format(time.RFC3339)
	}
	if r.RegistrationNumberLabel != nil {
		m["registration_number_label"] = *r.RegistrationNumberLabel
	}
	return m
}

func ruleLabel(r Rule) string {
	return fmt.Sprintf("%s %s %s%% (%s)", r.MarketCode, r.TaxName, r.Rate, r.AppliesTo)
}

func checkOverlap(r Rule, others []Rule) error {
	for _, o := range others {
		if r.Conflicts(o) {
			return ErrOverlap
		}
	}
	return nil
}

// List returns rules for administrators.
func (s *Service) List(ctx context.Context, f Filter) ([]Rule, error) {
	f.Market, f.AppliesTo, f.Status = normaliseMarket(f.Market), strings.ToUpper(strings.TrimSpace(f.AppliesTo)), strings.ToUpper(strings.TrimSpace(f.Status))
	return s.store.Rules(ctx, f)
}

// Get returns one rule.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Rule, error) { return s.store.Rule(ctx, id) }

// Create adds an ACTIVE rule.
func (s *Service) Create(ctx context.Context, p *auth.Principal, in Input) (Rule, error) {
	fields := map[string]string{}
	now := s.now().UTC()
	id, err := uuid.NewV7()
	if err != nil {
		return Rule{}, err
	}
	r := Rule{ID: id, Status: StatusActive, CreatedAt: now, UpdatedAt: now}
	if in.MarketCode == nil || strings.TrimSpace(*in.MarketCode) == "" {
		fields["market_code"] = i18n.T(ctx, "field.required", nil)
	} else {
		r.MarketCode = normaliseMarket(*in.MarketCode)
		if err := s.checkMarket(ctx, r.MarketCode, fields); err != nil {
			return Rule{}, err
		}
	}
	if in.AppliesTo == nil || strings.TrimSpace(*in.AppliesTo) == "" {
		fields["applies_to"] = i18n.T(ctx, "field.required", nil)
	} else if r.AppliesTo = strings.ToUpper(strings.TrimSpace(*in.AppliesTo)); !slices.Contains(AppliesTo(), r.AppliesTo) {
		fields["applies_to"] = "must be one of " + strings.Join(AppliesTo(), ", ")
	}
	apply(ctx, in, &r, fields, true)
	if len(fields) > 0 {
		return Rule{}, &ValidationError{Fields: fields}
	}
	err = s.store.Create(ctx, r, func(others []Rule) (adminaudit.Event, error) {
		if err := checkOverlap(r, others); err != nil {
			return adminaudit.Event{}, err
		}
		return adminaudit.New(p.UserID, p.Name, ActionRuleCreated, TargetRule, r.ID.String(), ruleLabel(r), "", nil, ruleState(r), now), nil
	})
	if err != nil {
		return Rule{}, err
	}
	return r, nil
}

// Update changes a rule (market and use never change; a RETIRED rule is
// read-only).
func (s *Service) Update(ctx context.Context, p *auth.Principal, id uuid.UUID, in Input) (Rule, error) {
	fields := map[string]string{}
	if in.MarketCode != nil {
		fields["market_code"] = "cannot be changed (create a new rule)"
	}
	if in.AppliesTo != nil {
		fields["applies_to"] = "cannot be changed (create a new rule)"
	}
	if len(fields) > 0 {
		return Rule{}, &ValidationError{Fields: fields}
	}
	now := s.now().UTC()
	return s.store.Change(ctx, id, func(r *Rule, others func() ([]Rule, error)) (adminaudit.Event, error) {
		if r.Status == StatusRetired {
			return adminaudit.Event{}, ErrRetired
		}
		before := ruleState(*r)
		apply(ctx, in, r, fields, false)
		if len(fields) > 0 {
			return adminaudit.Event{}, &ValidationError{Fields: fields}
		}
		list, err := others()
		if err != nil {
			return adminaudit.Event{}, err
		}
		if err := checkOverlap(*r, list); err != nil {
			return adminaudit.Event{}, err
		}
		r.UpdatedAt = now
		return adminaudit.New(p.UserID, p.Name, ActionRuleUpdated, TargetRule, r.ID.String(), ruleLabel(*r), "", before, ruleState(*r), now), nil
	})
}

// Retire makes an ACTIVE rule RETIRED (it no longer applies, ever).
func (s *Service) Retire(ctx context.Context, p *auth.Principal, id uuid.UUID) (Rule, error) {
	now := s.now().UTC()
	return s.store.Change(ctx, id, func(r *Rule, _ func() ([]Rule, error)) (adminaudit.Event, error) {
		if r.Status == StatusRetired {
			return adminaudit.Event{}, ErrRetired
		}
		before := ruleState(*r)
		r.Status, r.UpdatedAt = StatusRetired, now
		return adminaudit.New(p.UserID, p.Name, ActionRuleRetired, TargetRule, r.ID.String(), ruleLabel(*r), "", before, ruleState(*r), now), nil
	})
}
