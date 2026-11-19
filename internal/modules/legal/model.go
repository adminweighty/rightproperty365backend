// Package legal holds the per-market legal documents, their versions and the
// acceptances people give (API 0.43.0, BR-001).
//
// THE TEXT IS NOT LEGAL ADVICE. This package is structure only: the content
// must come from the business's lawyers. Production starts with no document
// (migration 0045 inserts none); the development and staging seed publishes
// GLOBAL placeholders that say "DRAFT — awaiting legal review" and carry
// DraftNotice.
//
// A document is a kind (TERMS, PRIVACY, ...) in a market (GLOBAL, the
// fallback, or an ISO 3166-1 alpha-2 code) and a language (en). Each has
// versions: DRAFT -> PUBLISHED -> RETIRED. A published or retired version
// never changes (the database refuses it too, legal_document_versions_frozen):
// a change is always a new version.
//
// The current version (Resolve) is the PUBLISHED one with the latest
// effective_from that is not in the future, for the market, else for GLOBAL;
// a missing language falls back to English at each step.
//
// Acceptances are rows of user_consents (migration 0002, widened by 0045):
// PRIVACY is stored as the privacy notice's existing consent type. They are
// kept on account deletion, as the privacy-notice records always were.
package legal

import (
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/i18n"
)

// Kinds.
const (
	KindTerms      = "TERMS"
	KindPrivacy    = "PRIVACY"
	KindCookies    = "COOKIES"
	KindRefund     = "REFUND"
	KindEULA       = "EULA"
	KindAgentTerms = "AGENT_TERMS"
)

// Kinds returns every kind, in the order documents are listed.
func Kinds() []string {
	return []string{KindTerms, KindPrivacy, KindCookies, KindRefund, KindEULA, KindAgentTerms}
}

// IsKind reports whether kind is known.
func IsKind(kind string) bool { return slices.Contains(Kinds(), kind) }

// MarketGlobal is the fallback market.
const MarketGlobal = "GLOBAL"

// Statuses.
const (
	StatusDraft     = "DRAFT"
	StatusPublished = "PUBLISHED"
	StatusRetired   = "RETIRED"
)

// Acceptance contexts.
const (
	ContextRegistration = "REGISTRATION"
	ContextReacceptance = "REACCEPTANCE"
)

// ConsentPrivacyNotice is user_consents' consent type for PRIVACY (0002).
const ConsentPrivacyNotice = "PRIVACY_NOTICE"

// ConsentType is the user_consents consent type of a kind.
func ConsentType(kind string) string {
	if kind == KindPrivacy {
		return ConsentPrivacyNotice
	}
	return kind
}

// KindOfConsent is the kind of a user_consents consent type.
func KindOfConsent(consentType string) string {
	if consentType == ConsentPrivacyNotice {
		return KindPrivacy
	}
	return consentType
}

// Limits.
const (
	MaxLabelLength   = 32
	MaxBodyLength    = 200000
	MaxSummaryLength = 2000
)

// Error codes.
const (
	CodeDocumentNotFound  = "LEGAL_DOCUMENT_NOT_FOUND"
	CodeVersionNotFound   = "LEGAL_VERSION_NOT_FOUND"
	CodePreviewForbidden  = "LEGAL_PREVIEW_FORBIDDEN"
	CodeMarketForbidden   = "LEGAL_MARKET_FORBIDDEN"
	CodeVersionImmutable  = "LEGAL_VERSION_IMMUTABLE"
	CodeVersionExists     = "LEGAL_VERSION_EXISTS"
	CodeVersionNotAllowed = "LEGAL_VERSION_NOT_ACCEPTABLE"
)

// Errors.
var (
	ErrNotFound         = errors.New("legal document or version not found")
	ErrPreviewForbidden = errors.New("preview is for administrators")
	ErrMarketForbidden  = errors.New("outside the administrator's market")
	ErrImmutable        = errors.New("a published or retired version is immutable")
	ErrDuplicate        = errors.New("a version with this label exists")
	ErrNotAcceptable    = errors.New("the version is retired or not yet effective")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// Version is one version of a legal document.
type Version struct {
	ID                   uuid.UUID
	Kind                 string
	MarketCode           string
	Lang                 string
	Label                string
	EffectiveFrom        time.Time
	Status               string
	Body                 string
	Summary              string
	RequiresReacceptance bool
	DraftNotice          bool
	CreatedBy            *uuid.UUID
	CreatedByName        string
	PublishedBy          *uuid.UUID
	PublishedByName      string
	PublishedAt          *time.Time
	RetiredAt            *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Clone returns a deep copy.
func (v Version) Clone() Version {
	for _, p := range []**uuid.UUID{&v.CreatedBy, &v.PublishedBy} {
		if *p != nil {
			c := **p
			*p = &c
		}
	}
	for _, p := range []**time.Time{&v.PublishedAt, &v.RetiredAt} {
		if *p != nil {
			c := **p
			*p = &c
		}
	}
	return v
}

// FrozenChange reports whether next is a change a PUBLISHED or RETIRED
// version may not undergo (the Go twin of the legal_document_versions_frozen
// trigger, used by the memory store). A DRAFT may change freely.
func FrozenChange(old, next Version) bool {
	if old.Status == StatusDraft {
		return false
	}
	content := next.Kind != old.Kind || next.MarketCode != old.MarketCode || next.Lang != old.Lang || next.Label != old.Label ||
		!next.EffectiveFrom.Equal(old.EffectiveFrom) || next.Body != old.Body || next.Summary != old.Summary ||
		next.RequiresReacceptance != old.RequiresReacceptance || next.DraftNotice != old.DraftNotice ||
		!sameTime(next.PublishedAt, old.PublishedAt) || !next.CreatedAt.Equal(old.CreatedAt)
	status := next.Status != old.Status && !(old.Status == StatusPublished && next.Status == StatusRetired)
	retired := !sameTime(next.RetiredAt, old.RetiredAt) && old.Status != StatusPublished
	relink := (!sameID(next.CreatedBy, old.CreatedBy) && next.CreatedBy != nil) || (!sameID(next.PublishedBy, old.PublishedBy) && next.PublishedBy != nil)
	return content || status || retired || relink
}

func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

func sameID(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// Filter selects versions; empty fields match everything.
type Filter struct {
	Kind, Market, Lang, Status string
}

// Matches reports whether v passes the filter.
func (f Filter) Matches(v Version) bool {
	return (f.Kind == "" || v.Kind == f.Kind) && (f.Market == "" || v.MarketCode == f.Market) &&
		(f.Lang == "" || v.Lang == f.Lang) && (f.Status == "" || v.Status == f.Status)
}

// SortVersions orders newest first: effective_from, then creation, then ID.
func SortVersions(list []Version) {
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].EffectiveFrom.Equal(list[j].EffectiveFrom) {
			return list[i].EffectiveFrom.After(list[j].EffectiveFrom)
		}
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		}
		return list[i].ID.String() > list[j].ID.String()
	})
}

// Resolution says how the current version was found.
type Resolution struct {
	FallbackMarket bool
	FallbackLang   bool
}

// Resolve returns the current version of kind for market and lang among
// versions (any statuses and kinds; only PUBLISHED ones with effective_from
// <= now count): the market's own, else GLOBAL's; at each step the language,
// else English. It is a pure function.
func Resolve(versions []Version, kind, market, lang string, now time.Time) (Version, Resolution, bool) {
	if market == "" {
		market = MarketGlobal
	}
	if lang == "" {
		lang = i18n.DefaultLanguage
	}
	markets := []string{market}
	if market != MarketGlobal {
		markets = append(markets, MarketGlobal)
	}
	langs := []string{lang}
	if lang != i18n.DefaultLanguage {
		langs = append(langs, i18n.DefaultLanguage)
	}
	for mi, m := range markets {
		for li, l := range langs {
			if v, ok := latestEffective(versions, kind, m, l, now); ok {
				return v, Resolution{FallbackMarket: mi > 0, FallbackLang: li > 0}, true
			}
		}
	}
	return Version{}, Resolution{}, false
}

func latestEffective(versions []Version, kind, market, lang string, now time.Time) (Version, bool) {
	var best *Version
	for i := range versions {
		v := &versions[i]
		if v.Kind != kind || v.MarketCode != market || v.Lang != lang || v.Status != StatusPublished || v.EffectiveFrom.After(now) {
			continue
		}
		if best == nil || v.EffectiveFrom.After(best.EffectiveFrom) ||
			(v.EffectiveFrom.Equal(best.EffectiveFrom) && v.CreatedAt.After(best.CreatedAt)) {
			best = v
		}
	}
	if best == nil {
		return Version{}, false
	}
	return best.Clone(), true
}

// Acceptance is one acceptance of a document version (a user_consents row).
type Acceptance struct {
	UserID uuid.UUID
	Kind   string
	// Version is the version label accepted.
	Version string
	// MarketCode is the document's market; nil for records written before
	// 0.43.0 (read as GLOBAL).
	MarketCode *string
	// Context is REGISTRATION or REACCEPTANCE; nil before 0.43.0.
	Context    *string
	AcceptedAt time.Time
}

// market returns the acceptance's market, GLOBAL when none was recorded.
func (a Acceptance) market() string {
	if a.MarketCode == nil || *a.MarketCode == "" {
		return MarketGlobal
	}
	return *a.MarketCode
}

// Covers reports whether a is an acceptance of v.
func (a Acceptance) Covers(v Version) bool {
	return a.Kind == v.Kind && a.Version == v.Label && a.market() == v.MarketCode
}

// AdminRoles may manage legal documents: SUPER_ADMIN, CONTENT_ADMIN, and
// MARKET_ADMIN for its own market (never GLOBAL).
func AdminRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformContentAdmin, auth.PlatformMarketAdmin}
}

// MayManage reports whether p may manage (and preview) market's documents.
func MayManage(p *auth.Principal, market string) bool {
	if p == nil {
		return false
	}
	if p.HasPlatformRole(auth.PlatformContentAdmin) {
		return true
	}
	if market == MarketGlobal || !p.HasPlatformRole(auth.PlatformMarketAdmin) {
		return false
	}
	for _, r := range p.PlatformRoles {
		if r.Role == auth.PlatformMarketAdmin && r.MarketCode == market {
			return true
		}
	}
	return false
}
