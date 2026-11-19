package legal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/i18n"
)

// Audit actions and target of the legal administration.
const (
	ActionVersionCreated   = "LEGAL_VERSION_CREATED"
	ActionVersionUpdated   = "LEGAL_VERSION_UPDATED"
	ActionVersionDeleted   = "LEGAL_VERSION_DELETED"
	ActionVersionPublished = "LEGAL_VERSION_PUBLISHED"
	ActionVersionRetired   = "LEGAL_VERSION_RETIRED"
	TargetVersion          = "LEGAL_DOCUMENT_VERSION"
)

// Deps are the service's collaborators.
type Deps struct {
	Store       Store
	Acceptances AcceptanceStore
	// KnownMarket reports whether an ISO country code is a market.
	KnownMarket func(ctx context.Context, code string) (bool, error)
	Now         func() time.Time
}

// Service holds the legal documents' rules.
type Service struct {
	store       Store
	acceptances AcceptanceStore
	knownMarket func(ctx context.Context, code string) (bool, error)
	now         func() time.Time
}

// NewService builds the service.
func NewService(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{store: d.Store, acceptances: d.Acceptances, knownMarket: d.KnownMarket, now: d.Now}
}

func (s *Service) clock() time.Time { return s.now().UTC() }

var marketRe = regexp.MustCompile(`^[A-Z]{2}$`)

// Market normalises a market parameter: "" and GLOBAL are GLOBAL; anything
// else must be a known country code (a ValidationError on field otherwise).
func (s *Service) Market(ctx context.Context, raw, field string) (string, error) {
	m := strings.ToUpper(strings.TrimSpace(raw))
	if m == "" || m == MarketGlobal {
		return MarketGlobal, nil
	}
	if !marketRe.MatchString(m) {
		return "", &ValidationError{Fields: map[string]string{field: i18n.T(ctx, "field.unknown_market", nil)}}
	}
	if s.knownMarket != nil {
		ok, err := s.knownMarket(ctx, m)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", &ValidationError{Fields: map[string]string{field: i18n.T(ctx, "field.unknown_market", nil)}}
		}
	}
	return m, nil
}

// ParseKind upper-cases a kind from a path; ErrNotFound when unknown.
func ParseKind(raw string) (string, error) {
	k := strings.ToUpper(strings.TrimSpace(raw))
	if !IsKind(k) {
		return "", ErrNotFound
	}
	return k, nil
}

// Resolved is a version with how it was found.
type Resolved struct {
	Version Version
	Resolution
}

func (s *Service) published(ctx context.Context, kind string) ([]Version, error) {
	return s.store.Versions(ctx, Filter{Kind: kind, Status: StatusPublished})
}

// Current returns the current version of kind for market and lang.
func (s *Service) Current(ctx context.Context, kind, market, lang string) (Resolved, error) {
	all, err := s.published(ctx, kind)
	if err != nil {
		return Resolved{}, err
	}
	v, res, ok := Resolve(all, kind, market, lang, s.clock())
	if !ok {
		return Resolved{}, ErrNotFound
	}
	return Resolved{Version: v, Resolution: res}, nil
}

// CurrentAll returns the current version of every kind that has one.
func (s *Service) CurrentAll(ctx context.Context, market, lang string) ([]Resolved, error) {
	all, err := s.store.Versions(ctx, Filter{Status: StatusPublished})
	if err != nil {
		return nil, err
	}
	out := []Resolved{}
	now := s.clock()
	for _, kind := range Kinds() {
		if v, res, ok := Resolve(all, kind, market, lang, now); ok {
			out = append(out, Resolved{Version: v, Resolution: res})
		}
	}
	return out, nil
}

// lookupOrder is the market and language order of every fallback.
func lookupOrder(market, lang string) (markets, langs []string) {
	markets, langs = []string{market}, []string{lang}
	if market != MarketGlobal {
		markets = append(markets, MarketGlobal)
	}
	if lang != i18n.DefaultLanguage {
		langs = append(langs, i18n.DefaultLanguage)
	}
	return markets, langs
}

// Specific returns one PUBLISHED or RETIRED version by label (market, then
// GLOBAL; language, then English). A DRAFT is never returned.
func (s *Service) Specific(ctx context.Context, kind, market, lang, label string) (Resolved, error) {
	all, err := s.store.Versions(ctx, Filter{Kind: kind})
	if err != nil {
		return Resolved{}, err
	}
	markets, langs := lookupOrder(market, lang)
	for mi, m := range markets {
		for li, l := range langs {
			for _, v := range all {
				if v.MarketCode == m && v.Lang == l && v.Label == label && v.Status != StatusDraft {
					return Resolved{Version: v, Resolution: Resolution{FallbackMarket: mi > 0, FallbackLang: li > 0}}, nil
				}
			}
		}
	}
	return Resolved{}, ErrNotFound
}

// Preview returns, for an administrator of the market, the newest DRAFT of
// kind for market (language, then English), else the current version.
func (s *Service) Preview(ctx context.Context, p *auth.Principal, kind, market, lang string) (Resolved, error) {
	if !MayManage(p, market) {
		return Resolved{}, ErrPreviewForbidden
	}
	drafts, err := s.store.Versions(ctx, Filter{Kind: kind, Market: market, Status: StatusDraft})
	if err != nil {
		return Resolved{}, err
	}
	_, langs := lookupOrder(market, lang)
	for li, l := range langs {
		for _, v := range drafts { // newest first
			if v.Lang == l {
				return Resolved{Version: v, Resolution: Resolution{FallbackLang: li > 0}}, nil
			}
		}
	}
	return s.Current(ctx, kind, market, lang)
}

// History returns the PUBLISHED and RETIRED versions that apply to market
// (its own and GLOBAL's, in lang and English), newest first, and the ID of
// the current one (uuid.Nil when none).
func (s *Service) History(ctx context.Context, kind, market, lang string) ([]Version, uuid.UUID, error) {
	all, err := s.store.Versions(ctx, Filter{Kind: kind})
	if err != nil {
		return nil, uuid.Nil, err
	}
	markets, langs := lookupOrder(market, lang)
	out := []Version{}
	for _, v := range all {
		if v.Status != StatusDraft && contains(markets, v.MarketCode) && contains(langs, v.Lang) {
			out = append(out, v)
		}
	}
	current := uuid.Nil
	if v, _, ok := Resolve(all, kind, market, lang, s.clock()); ok {
		current = v.ID
	}
	return out, current, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Acceptances
// ---------------------------------------------------------------------------

// Pending returns the current versions (for market) that require
// re-acceptance and that the person has not accepted. Nothing is blocked.
func (s *Service) Pending(ctx context.Context, userID uuid.UUID, market string) ([]Version, error) {
	if s.acceptances == nil {
		return []Version{}, nil
	}
	current, err := s.CurrentAll(ctx, market, i18n.DefaultLanguage)
	if err != nil {
		return nil, err
	}
	var mine []Acceptance
	out := []Version{}
	for _, r := range current {
		if !r.Version.RequiresReacceptance {
			continue
		}
		if mine == nil {
			if mine, err = s.acceptances.Acceptances(ctx, userID); err != nil {
				return nil, err
			}
		}
		if !covered(mine, r.Version) {
			out = append(out, r.Version)
		}
	}
	return out, nil
}

func covered(list []Acceptance, v Version) bool {
	for _, a := range list {
		if a.Covers(v) {
			return true
		}
	}
	return false
}

// AcceptInput is the body of POST /v1/me/legal-acceptances.
type AcceptInput struct {
	Kind       string  `json:"kind"`
	Version    string  `json:"version"`
	MarketCode *string `json:"market_code"`
}

// Accept records that the person accepted a PUBLISHED, effective version.
// It is idempotent: an existing acceptance is returned with created false.
func (s *Service) Accept(ctx context.Context, userID uuid.UUID, in AcceptInput) (Acceptance, bool, error) {
	fields := map[string]string{}
	kind := strings.ToUpper(strings.TrimSpace(in.Kind))
	if kind == "" {
		fields["kind"] = i18n.T(ctx, "field.required", nil)
	} else if !IsKind(kind) {
		fields["kind"] = "must be one of " + strings.Join(Kinds(), ", ")
	}
	label := strings.TrimSpace(in.Version)
	if label == "" {
		fields["version"] = i18n.T(ctx, "field.required", nil)
	}
	raw := ""
	if in.MarketCode != nil {
		raw = *in.MarketCode
	}
	market, err := s.Market(ctx, raw, "market_code")
	var verr *ValidationError
	if errors.As(err, &verr) {
		fields["market_code"] = verr.Fields["market_code"]
	} else if err != nil {
		return Acceptance{}, false, err
	}
	if len(fields) > 0 {
		return Acceptance{}, false, &ValidationError{Fields: fields}
	}
	all, err := s.store.Versions(ctx, Filter{Kind: kind, Market: market, Lang: i18n.DefaultLanguage})
	if err != nil {
		return Acceptance{}, false, err
	}
	var doc *Version
	for i := range all {
		if all[i].Label == label && all[i].Status != StatusDraft {
			doc = &all[i]
			break
		}
	}
	now := s.clock()
	switch {
	case doc == nil:
		return Acceptance{}, false, ErrNotFound
	case doc.Status != StatusPublished || doc.EffectiveFrom.After(now):
		return Acceptance{}, false, ErrNotAcceptable
	}
	if s.acceptances == nil {
		return Acceptance{}, false, errors.New("legal: no acceptance store")
	}
	mine, err := s.acceptances.Acceptances(ctx, userID)
	if err != nil {
		return Acceptance{}, false, err
	}
	for _, a := range mine {
		if a.Covers(*doc) {
			return a, false, nil
		}
	}
	m, c := doc.MarketCode, ContextReacceptance
	a := Acceptance{UserID: userID, Kind: kind, Version: doc.Label, MarketCode: &m, Context: &c, AcceptedAt: now}
	if err := s.acceptances.Accept(ctx, a); err != nil {
		return Acceptance{}, false, err
	}
	return a, true, nil
}

// MyAcceptances returns the person's acceptances, newest first.
func (s *Service) MyAcceptances(ctx context.Context, userID uuid.UUID) ([]Acceptance, error) {
	if s.acceptances == nil {
		return []Acceptance{}, nil
	}
	return s.acceptances.Acceptances(ctx, userID)
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

// VersionInput is the body of POST and PATCH /v1/admin/legal.
type VersionInput struct {
	Kind                 *string    `json:"kind"`
	MarketCode           *string    `json:"market_code"`
	Lang                 *string    `json:"lang"`
	Version              *string    `json:"version"`
	EffectiveFrom        *time.Time `json:"effective_from"`
	Body                 *string    `json:"body_markdown"`
	Summary              *string    `json:"summary_of_changes"`
	RequiresReacceptance *bool      `json:"requires_reacceptance"`
	DraftNotice          *bool      `json:"draft_notice"`
}

var (
	htmlTagRe  = regexp.MustCompile(`<[/!?]?[A-Za-z][^>]*>|<!--`)
	autolinkRe = regexp.MustCompile(`<(https?://|mailto:)[^>\s]*>`)
)

func hasHTML(s string) bool { return htmlTagRe.MatchString(autolinkRe.ReplaceAllString(s, "")) }

func printableASCII(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func versionState(v Version) map[string]any {
	sum := sha256.Sum256([]byte(v.Body))
	m := map[string]any{"kind": v.Kind, "market_code": v.MarketCode, "lang": v.Lang, "version": v.Label, "status": v.Status,
		"effective_from": v.EffectiveFrom.UTC().Format(time.RFC3339), "requires_reacceptance": v.RequiresReacceptance,
		"draft_notice": v.DraftNotice, "body_length": utf8.RuneCountInString(v.Body), "body_sha256": hex.EncodeToString(sum[:8])}
	return m
}

func versionLabel(v Version) string {
	return fmt.Sprintf("%s %s %s %s", v.Kind, v.MarketCode, v.Lang, v.Label)
}

// List returns versions for administrators (any status).
func (s *Service) List(ctx context.Context, f Filter) ([]Version, error) {
	f.Kind, f.Status = strings.ToUpper(f.Kind), strings.ToUpper(f.Status)
	if f.Market != "" {
		m, err := s.Market(ctx, f.Market, "market")
		if err != nil {
			return nil, err
		}
		f.Market = m
	}
	return s.store.Versions(ctx, f)
}

// Get returns one version for administrators.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Version, error) {
	return s.store.Version(ctx, id)
}

// applyText validates and applies the editable fields of in to v.
func applyText(ctx context.Context, in VersionInput, v *Version, fields map[string]string, creating bool) {
	if in.Version != nil || creating {
		label := ""
		if in.Version != nil {
			label = strings.TrimSpace(*in.Version)
		}
		switch {
		case label == "":
			fields["version"] = i18n.T(ctx, "field.required", nil)
		case len(label) > MaxLabelLength || !printableASCII(label):
			fields["version"] = fmt.Sprintf("must be 1 to %d printable ASCII characters", MaxLabelLength)
		}
		v.Label = label
	}
	if in.EffectiveFrom != nil {
		v.EffectiveFrom = in.EffectiveFrom.UTC()
	} else if creating {
		fields["effective_from"] = i18n.T(ctx, "field.required", nil)
	}
	if in.Body != nil || creating {
		body := ""
		if in.Body != nil {
			body = strings.TrimSpace(*in.Body)
		}
		switch {
		case body == "":
			fields["body_markdown"] = i18n.T(ctx, "field.required", nil)
		case utf8.RuneCountInString(body) > MaxBodyLength:
			fields["body_markdown"] = i18n.T(ctx, "field.too_long", i18n.Args{"max": MaxBodyLength})
		case hasHTML(body) || !utf8.ValidString(body):
			fields["body_markdown"] = i18n.T(ctx, "field.no_html", nil)
		}
		v.Body = body + "\n"
	}
	if in.Summary != nil {
		summary := strings.TrimSpace(*in.Summary)
		if utf8.RuneCountInString(summary) > MaxSummaryLength {
			fields["summary_of_changes"] = i18n.T(ctx, "field.too_long", i18n.Args{"max": MaxSummaryLength})
		} else if hasHTML(summary) {
			fields["summary_of_changes"] = i18n.T(ctx, "field.no_html", nil)
		}
		v.Summary = summary
	}
	if in.RequiresReacceptance != nil {
		v.RequiresReacceptance = *in.RequiresReacceptance
	}
	if in.DraftNotice != nil {
		v.DraftNotice = *in.DraftNotice
	}
}

// Create adds a DRAFT version.
func (s *Service) Create(ctx context.Context, p *auth.Principal, in VersionInput) (Version, error) {
	fields := map[string]string{}
	now := s.clock()
	id, err := uuid.NewV7()
	if err != nil {
		return Version{}, err
	}
	creator := p.UserID
	v := Version{ID: id, Status: StatusDraft, DraftNotice: true, CreatedBy: &creator, CreatedByName: p.Name, CreatedAt: now, UpdatedAt: now}
	if v.CreatedByName == "" {
		v.CreatedByName = "Administrator"
	}
	if in.Kind == nil || strings.TrimSpace(*in.Kind) == "" {
		fields["kind"] = i18n.T(ctx, "field.required", nil)
	} else if v.Kind = strings.ToUpper(strings.TrimSpace(*in.Kind)); !IsKind(v.Kind) {
		fields["kind"] = "must be one of " + strings.Join(Kinds(), ", ")
	}
	raw := ""
	if in.MarketCode != nil {
		raw = *in.MarketCode
	}
	if in.MarketCode == nil || strings.TrimSpace(raw) == "" {
		fields["market_code"] = i18n.T(ctx, "field.required", nil) + " (GLOBAL or a country code)"
	} else if m, err := s.Market(ctx, raw, "market_code"); err != nil {
		var verr *ValidationError
		if !errors.As(err, &verr) {
			return Version{}, err
		}
		fields["market_code"] = verr.Fields["market_code"]
	} else {
		v.MarketCode = m
	}
	v.Lang = i18n.DefaultLanguage
	if in.Lang != nil && strings.TrimSpace(*in.Lang) != "" {
		v.Lang = strings.ToLower(strings.TrimSpace(*in.Lang))
		if !i18n.Supported(v.Lang) {
			fields["lang"] = "must be a supported language (GET /v1/i18n/languages)"
		}
	}
	applyText(ctx, in, &v, fields, true)
	if len(fields) > 0 {
		return Version{}, &ValidationError{Fields: fields}
	}
	if !MayManage(p, v.MarketCode) {
		return Version{}, ErrMarketForbidden
	}
	audit := adminaudit.New(p.UserID, p.Name, ActionVersionCreated, TargetVersion, v.ID.String(), versionLabel(v), "", nil, versionState(v), now)
	if err := s.store.Create(ctx, v, audit); err != nil {
		return Version{}, err
	}
	return v, nil
}

// Update changes a DRAFT (kind, market and language never change).
func (s *Service) Update(ctx context.Context, p *auth.Principal, id uuid.UUID, in VersionInput) (Version, error) {
	fields := map[string]string{}
	if in.Kind != nil {
		fields["kind"] = "cannot be changed (create a new version)"
	}
	if in.MarketCode != nil {
		fields["market_code"] = "cannot be changed (create a new version)"
	}
	if in.Lang != nil {
		fields["lang"] = "cannot be changed (create a new version)"
	}
	if len(fields) > 0 {
		return Version{}, &ValidationError{Fields: fields}
	}
	now := s.clock()
	return s.store.Change(ctx, id, func(v *Version) (adminaudit.Event, error) {
		if !MayManage(p, v.MarketCode) {
			return adminaudit.Event{}, ErrMarketForbidden
		}
		if v.Status != StatusDraft {
			return adminaudit.Event{}, ErrImmutable
		}
		before := versionState(*v)
		applyText(ctx, in, v, fields, false)
		if len(fields) > 0 {
			return adminaudit.Event{}, &ValidationError{Fields: fields}
		}
		v.UpdatedAt = now
		return adminaudit.New(p.UserID, p.Name, ActionVersionUpdated, TargetVersion, v.ID.String(), versionLabel(*v), "", before, versionState(*v), now), nil
	})
}

// Delete removes a DRAFT.
func (s *Service) Delete(ctx context.Context, p *auth.Principal, id uuid.UUID) error {
	now := s.clock()
	return s.store.Delete(ctx, id, func(v Version) (adminaudit.Event, error) {
		if !MayManage(p, v.MarketCode) {
			return adminaudit.Event{}, ErrMarketForbidden
		}
		if v.Status != StatusDraft {
			return adminaudit.Event{}, ErrImmutable
		}
		return adminaudit.New(p.UserID, p.Name, ActionVersionDeleted, TargetVersion, v.ID.String(), versionLabel(v), "", versionState(v), nil, now), nil
	})
}

// Publish makes a DRAFT PUBLISHED (it becomes current at effective_from).
func (s *Service) Publish(ctx context.Context, p *auth.Principal, id uuid.UUID) (Version, error) {
	now := s.clock()
	return s.store.Change(ctx, id, func(v *Version) (adminaudit.Event, error) {
		if !MayManage(p, v.MarketCode) {
			return adminaudit.Event{}, ErrMarketForbidden
		}
		if v.Status != StatusDraft {
			return adminaudit.Event{}, ErrImmutable
		}
		before := versionState(*v)
		publisher := p.UserID
		v.Status, v.PublishedBy, v.PublishedByName, v.PublishedAt, v.UpdatedAt = StatusPublished, &publisher, p.Name, &now, now
		if v.PublishedByName == "" {
			v.PublishedByName = "Administrator"
		}
		return adminaudit.New(p.UserID, p.Name, ActionVersionPublished, TargetVersion, v.ID.String(), versionLabel(*v), "", before, versionState(*v), now), nil
	})
}

// Retire makes a PUBLISHED version RETIRED (kept in the history).
func (s *Service) Retire(ctx context.Context, p *auth.Principal, id uuid.UUID) (Version, error) {
	now := s.clock()
	return s.store.Change(ctx, id, func(v *Version) (adminaudit.Event, error) {
		if !MayManage(p, v.MarketCode) {
			return adminaudit.Event{}, ErrMarketForbidden
		}
		if v.Status != StatusPublished {
			return adminaudit.Event{}, ErrImmutable
		}
		before := versionState(*v)
		v.Status, v.RetiredAt, v.UpdatedAt = StatusRetired, &now, now
		return adminaudit.New(p.UserID, p.Name, ActionVersionRetired, TargetVersion, v.ID.String(), versionLabel(*v), "", before, versionState(*v), now), nil
	})
}
