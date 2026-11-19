package agreements

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Deps is what the service needs.
type Deps struct {
	Store    Store
	Files    storage.Storage
	Mailer   Mailer
	Logger   *slog.Logger
	Now      func() time.Time
	DemoCode string
	// PublicWebURL is printed in the audit report (the verify page).
	PublicWebURL string
}

// Service implements the agreement use cases.
type Service struct {
	store      Store
	files      storage.Storage
	mailer     Mailer
	logger     *slog.Logger
	now        func() time.Time
	demoCode   string
	publicURL  string
	onFinished func(ctx context.Context, a Agreement)
}

// NewService builds a Service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, files: d.Files, mailer: d.Mailer, logger: d.Logger, now: d.Now, demoCode: d.DemoCode,
		publicURL: strings.TrimRight(d.PublicWebURL, "/")}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// SetOnFinished is called once an agreement is completed, declined,
// cancelled (after it was sent) or expired: a lease follows its agreement.
func (s *Service) SetOnFinished(fn func(ctx context.Context, a Agreement)) { s.onFinished = fn }

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

func (s *Service) audit(ctx context.Context, event string, attrs ...slog.Attr) {
	base := []slog.Attr{slog.Bool("audit", true), slog.String("event", event), slog.String("trace_id", logging.RequestID(ctx))}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", append(base, attrs...)...)
}

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func userPtr(id uuid.UUID) *uuid.UUID { return &id }

func (s *Service) finished(ctx context.Context, a Agreement) {
	if s.onFinished != nil {
		s.onFinished(context.WithoutCancel(ctx), a)
	}
}

func (s *Service) dropObject(ctx context.Context, key string) {
	if key == "" {
		return
	}
	if err := s.files.Delete(context.WithoutCancel(ctx), key); err != nil {
		s.logger.WarnContext(ctx, "agreement object not deleted", slog.String("object_key", key), slog.String("error", err.Error()))
	}
}

// ---------------------------------------------------------------------------
// Who may
// ---------------------------------------------------------------------------

// AdminRoles read every agreement (read-only).
func AdminRoles() []string { return []string{auth.PlatformSuperAdmin, auth.PlatformSupport} }

// MaySend reports whether the caller may prepare and send agreements:
// whoever may manage listings, on a customer session (as leases).
func MaySend(p *auth.Principal) bool {
	return p != nil && p.SessionScope == auth.ScopeCustomer && p.ManagesListings()
}

func orgWide(p *auth.Principal, organisationID uuid.UUID) bool {
	return p.MayManageListings(organisationID) && p.MayAssignWork(organisationID)
}

func maySee(p *auth.Principal, a Agreement) bool {
	if !MaySend(p) {
		return false
	}
	if a.OrganisationID != nil {
		return orgWide(p, *a.OrganisationID) || (a.SenderID == p.UserID && p.MayManageListings(*a.OrganisationID))
	}
	return a.SenderID == p.UserID
}

func scopeOf(p *auth.Principal) Filter {
	f := Filter{Sender: userPtr(p.UserID), SenderDirect: true}
	for _, id := range p.ListingOrganisationIDs() {
		if orgWide(p, id) {
			f.Organisations = append(f.Organisations, id)
		} else {
			f.SenderOrganisations = append(f.SenderOrganisations, id)
		}
	}
	return f
}

// ---------------------------------------------------------------------------
// Tokens and codes
// ---------------------------------------------------------------------------

func newToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate signing token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func plausibleToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	for _, r := range token {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

type minted struct {
	recipient Recipient
	token     string
	expires   time.Time
}

// mint adds a link for the recipient to a and returns its token.
func mint(a *Agreement, r Recipient, now, expires time.Time) (minted, error) {
	token, hash, err := newToken()
	if err != nil {
		return minted{}, err
	}
	a.Links = append(a.Links, Link{ID: newID(), RecipientID: r.ID, TokenHash: hash, ExpiresAt: expires, CreatedAt: now})
	return minted{recipient: r, token: token, expires: expires}, nil
}

func codeHash(token string, recipientID uuid.UUID, code string) []byte {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("agreement-code:" + recipientID.String() + ":" + code))
	return m.Sum(nil)
}

func newCode() (string, error) {
	var b strings.Builder
	for range CodeLength {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteByte(byte('0' + n.Int64()))
	}
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func actionable(r Recipient) bool { return r.Role == RoleSigner || r.Role == RoleApprover }

// currentOrder is the lowest order with an actionable recipient who has not
// finished; 0 when everybody finished.
func currentOrder(a *Agreement) int {
	order := 0
	for _, r := range a.Recipients {
		if actionable(r) && !r.Done() && (order == 0 || r.Order < order) {
			order = r.Order
		}
	}
	return order
}

func allFinished(a *Agreement) bool {
	any := false
	for _, r := range a.Recipients {
		if actionable(r) {
			any = true
			if r.Status != RecipientSigned && r.Status != RecipientApproved {
				return false
			}
		}
	}
	return any
}

func systemEvent(a *Agreement, action string, at time.Time, r *Recipient, detail map[string]any) Event {
	e := Event{ID: newID(), AgreementID: a.ID, Action: action, ActorType: ActorSystem, ActorName: "RightProperty365", Detail: detail, At: at}
	if r != nil {
		id := r.ID
		e.RecipientID = &id
	}
	return e
}

// activate makes the recipients of the current order ACTIVE and gives each
// a link (emailed by the caller).
func activate(a *Agreement, now time.Time) ([]minted, []Event, error) {
	order := currentOrder(a)
	if order == 0 {
		return nil, nil, nil
	}
	var out []minted
	var events []Event
	for i := range a.Recipients {
		r := &a.Recipients[i]
		if !actionable(*r) || r.Order != order || r.Status != RecipientWaiting {
			continue
		}
		r.Status, r.ActivatedAt = RecipientActive, &now
		m, err := mint(a, *r, now, a.ExpiresAt)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, m)
		events = append(events, systemEvent(a, EventEmailed, now, r, map[string]any{"to": r.Email, "order": r.Order, "role": r.Role}))
	}
	return out, events, nil
}

func roleLabel(r Recipient) string {
	if r.Label != "" {
		return r.Label
	}
	return map[string]string{RoleSigner: "signer", RoleApprover: "approver", RoleCC: "copy recipient"}[r.Role]
}

func (s *Service) emailLinks(ctx context.Context, a Agreement, links []minted, reminder bool) {
	if s.mailer == nil {
		return
	}
	for _, m := range links {
		if err := s.mailer.SendSigning(ctx, m.recipient.Email, SigningMail{Token: m.token, Name: m.recipient.Name, RoleLabel: roleLabel(m.recipient),
			AgreementName: a.Name, Reference: a.Reference, SenderName: a.SenderName, Message: a.Message, ExpiresAt: m.expires, Reminder: reminder}); err != nil {
			s.logger.ErrorContext(ctx, "agreement signing email failed", slog.String("agreement_id", a.ID.String()),
				slog.String("recipient_id", m.recipient.ID.String()), slog.String("error", err.Error()))
		}
	}
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// RecipientView is a recipient as the sender sees them.
type RecipientView struct {
	ID            uuid.UUID  `json:"id"`
	Order         int        `json:"order"`
	Role          string     `json:"role"`
	Name          string     `json:"name"`
	Email         string     `json:"email"`
	Label         *string    `json:"label"`
	Status        string     `json:"status"`
	ActivatedAt   *time.Time `json:"activated_at"`
	FirstViewedAt *time.Time `json:"first_viewed_at"`
	CompletedAt   *time.Time `json:"completed_at"`
	DeclinedAt    *time.Time `json:"declined_at"`
	DeclineReason *string    `json:"decline_reason"`
	Verification  *string    `json:"verification"`
}

// FieldView is a field on the document.
type FieldView struct {
	ID          uuid.UUID `json:"id"`
	RecipientID uuid.UUID `json:"recipient_id"`
	Type        string    `json:"type"`
	Page        int       `json:"page"`
	X           float64   `json:"x"`
	Y           float64   `json:"y"`
	W           float64   `json:"w"`
	H           float64   `json:"h"`
	Required    bool      `json:"required"`
	Label       *string   `json:"label"`
	Value       *string   `json:"value"`
}

// DocumentView describes a stored PDF.
type DocumentView struct {
	SHA256    string     `json:"sha256"`
	SizeBytes int64      `json:"size_bytes"`
	FileName  string     `json:"file_name"`
	Pages     []PageSize `json:"pages,omitempty"`
	// ScanStatus is the malware scan status of an uploaded PDF (API
	// 0.41.0): PENDING while it is being checked (it cannot be opened or
	// sent yet), CLEAN, INFECTED (removed: upload another) or
	// LEGACY_UNSCANNED. Omitted when scanning is off and on the signed copy.
	ScanStatus string `json:"scan_status,omitempty"`
}

// View is an agreement as its sender (and their organisation) sees it.
type View struct {
	ID        uuid.UUID  `json:"id"`
	Reference string     `json:"reference"`
	Name      string     `json:"name"`
	Message   *string    `json:"message"`
	Status    string     `json:"status"`
	Source    string     `json:"source"`
	LeaseID   *uuid.UUID `json:"lease_id"`
	Sender    struct {
		UserID uuid.UUID `json:"user_id"`
		Name   string    `json:"name"`
		Email  string    `json:"email"`
	} `json:"sender"`
	Organisation *struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	} `json:"organisation"`
	ExpiresAt         time.Time       `json:"expires_at"`
	ReminderFrequency string          `json:"reminder_frequency"`
	Document          *DocumentView   `json:"document"`
	FinalDocument     *DocumentView   `json:"final_document"`
	Recipients        []RecipientView `json:"recipients"`
	Fields            []FieldView     `json:"fields"`
	WaitingForYou     bool            `json:"waiting_for_you"`
	CanEdit           bool            `json:"can_edit"`
	CanSend           bool            `json:"can_send"`
	CanCancel         bool            `json:"can_cancel"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	SentAt            *time.Time      `json:"sent_at"`
	CompletedAt       *time.Time      `json:"completed_at"`
	DeclinedAt        *time.Time      `json:"declined_at"`
	CancelledAt       *time.Time      `json:"cancelled_at"`
	ExpiredAt         *time.Time      `json:"expired_at"`
	CancelReason      *string         `json:"cancel_reason"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func fieldView(f Field) FieldView {
	return FieldView{ID: f.ID, RecipientID: f.RecipientID, Type: f.Type, Page: f.Page, X: f.X, Y: f.Y, W: f.W, H: f.H, Required: f.Required,
		Label: optional(f.Label), Value: optional(f.Value)}
}

func waitingFor(a Agreement, email string) bool {
	if a.Status != StatusOutForSignature {
		return false
	}
	for _, r := range a.Recipients {
		if r.Status == RecipientActive && r.Email == email {
			return true
		}
	}
	return false
}

func view(a Agreement, viewer string) View {
	v := View{ID: a.ID, Reference: a.Reference, Name: a.Name, Message: optional(a.Message), Status: a.Status, Source: a.Source,
		LeaseID: a.LeaseID, ExpiresAt: a.ExpiresAt, ReminderFrequency: a.ReminderFrequency, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
		SentAt: a.SentAt, CompletedAt: a.CompletedAt, DeclinedAt: a.DeclinedAt, CancelledAt: a.CancelledAt, ExpiredAt: a.ExpiredAt,
		CancelReason: optional(a.CancelReason), Recipients: []RecipientView{}, Fields: []FieldView{}}
	v.Sender.UserID, v.Sender.Name, v.Sender.Email = a.SenderID, a.SenderName, a.SenderEmail
	if a.OrganisationID != nil {
		v.Organisation = &struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
		}{*a.OrganisationID, a.OrganisationName}
	}
	if a.Original != nil {
		v.Document = &DocumentView{SHA256: a.Original.SHA256, SizeBytes: a.Original.Size, FileName: a.Original.FileName, Pages: a.Pages}
	}
	if a.Final != nil {
		v.FinalDocument = &DocumentView{SHA256: a.Final.SHA256, SizeBytes: a.Final.Size, FileName: a.Final.FileName}
	}
	for _, r := range a.Recipients {
		v.Recipients = append(v.Recipients, RecipientView{ID: r.ID, Order: r.Order, Role: r.Role, Name: r.Name, Email: r.Email, Label: optional(r.Label),
			Status: r.Status, ActivatedAt: r.ActivatedAt, FirstViewedAt: r.FirstViewedAt, CompletedAt: r.CompletedAt, DeclinedAt: r.DeclinedAt,
			DeclineReason: optional(r.DeclineReason), Verification: optional(r.Verification)})
	}
	for _, f := range a.Fields {
		v.Fields = append(v.Fields, fieldView(f))
	}
	v.WaitingForYou = waitingFor(a, viewer)
	v.CanEdit = a.Status == StatusDraft && a.Source == SourceUpload
	v.CanSend = a.Status == StatusDraft
	v.CanCancel = a.Status == StatusDraft || a.Status == StatusOutForSignature
	return v
}

// EventView is one record of an agreement's audit trail.
type EventView struct {
	ID          uuid.UUID      `json:"id"`
	Action      string         `json:"action"`
	ActorType   string         `json:"actor_type"`
	ActorUserID *uuid.UUID     `json:"actor_user_id"`
	ActorName   string         `json:"actor_name"`
	RecipientID *uuid.UUID     `json:"recipient_id"`
	IP          *string        `json:"ip"`
	UserAgent   *string        `json:"user_agent"`
	Detail      map[string]any `json:"detail"`
	At          time.Time      `json:"at"`
}

// Page is one page of agreements.
type Page struct {
	Items      []View `json:"items"`
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
	Total      int    `json:"total"`
	TotalPages int    `json:"total_pages"`
}

// ---------------------------------------------------------------------------
// Loading (and expiring on the way)
// ---------------------------------------------------------------------------

// fresh expires an agreement whose expiry passed (at most once).
func (s *Service) fresh(ctx context.Context, a Agreement) (Agreement, error) {
	now := s.clock()
	if a.Status != StatusOutForSignature || now.Before(a.ExpiresAt) {
		return a, nil
	}
	changed := false
	saved, err := s.store.ChangeAgreement(ctx, a.ID, func(cur *Agreement) ([]Event, error) {
		if cur.Status != StatusOutForSignature || now.Before(cur.ExpiresAt) {
			return nil, nil
		}
		changed = true
		cur.Status, cur.ExpiredAt, cur.UpdatedAt = StatusExpired, &now, now
		return []Event{systemEvent(cur, EventExpired, now, nil, nil)}, nil
	})
	if err != nil {
		return Agreement{}, err
	}
	if changed {
		s.audit(ctx, "agreement.expired", slog.String("agreement_id", a.ID.String()))
		s.finished(ctx, saved)
	}
	return saved, nil
}

func (s *Service) load(ctx context.Context, id uuid.UUID) (Agreement, error) {
	a, err := s.store.Agreement(ctx, id)
	if err != nil {
		return Agreement{}, err
	}
	return s.fresh(ctx, a)
}

func (s *Service) visible(ctx context.Context, p *auth.Principal, id uuid.UUID) (Agreement, error) {
	if !MaySend(p) {
		return Agreement{}, ErrForbidden
	}
	a, err := s.load(ctx, id)
	if err != nil {
		return Agreement{}, err
	}
	if !maySee(p, a) {
		return Agreement{}, ErrNotFound
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// Preparing
// ---------------------------------------------------------------------------

func (s *Service) organisation(p *auth.Principal, raw *string) (*uuid.UUID, string, error) {
	var id *uuid.UUID
	if raw != nil && strings.TrimSpace(*raw) != "" {
		v, err := uuid.Parse(strings.TrimSpace(*raw))
		if err != nil || !p.MayManageListings(v) {
			return nil, "", &ValidationError{Fields: map[string]string{"organisation_id": "must be an organisation whose listings you manage"}}
		}
		id = &v
	} else {
		orgs := p.ListingOrganisationIDs()
		switch {
		case len(orgs) == 1:
			id = &orgs[0]
		case len(orgs) > 1:
			return nil, "", &ValidationError{Fields: map[string]string{"organisation_id": "is required: you manage listings for more than one organisation"}}
		}
	}
	if id == nil {
		return nil, "", nil
	}
	for _, m := range p.Memberships {
		if m.OrganisationID == *id {
			return id, m.OrganisationName, nil
		}
	}
	return id, "", nil
}

// Create prepares a DRAFT agreement for a PDF the sender will upload.
func (s *Service) Create(ctx context.Context, p *auth.Principal, in Input) (View, error) {
	if !MaySend(p) {
		return View{}, ErrForbidden
	}
	now := s.clock()
	org, orgName, err := s.organisation(p, in.OrganisationID)
	if err != nil {
		return View{}, err
	}
	a := Agreement{ID: newID(), Status: StatusDraft, Source: SourceUpload, SenderID: p.UserID, SenderName: p.Name,
		SenderEmail: identity.NormalizeEmail(p.Email), OrganisationID: org, OrganisationName: orgName, ExpiresAt: now.Add(DefaultExpiry),
		ReminderFrequency: ReminderNone, CreatedAt: now, UpdatedAt: now}
	if err := applyInput(&a, in, true, now); err != nil {
		return View{}, err
	}
	for range 5 {
		if a.Reference, err = s.store.NextReference(ctx, now.Year()); err != nil {
			return View{}, err
		}
		err = s.store.CreateAgreement(ctx, a, []Event{userEvent(&a, EventCreated, p, now, map[string]any{"source": SourceUpload})})
		if !errors.Is(err, ErrReferenceTaken) {
			break
		}
	}
	if err != nil {
		return View{}, err
	}
	s.audit(ctx, "agreement.created", slog.String("agreement_id", a.ID.String()), slog.String("actor_user_id", p.UserID.String()))
	return view(a, a.SenderEmail), nil
}

func userEvent(a *Agreement, action string, p *auth.Principal, at time.Time, detail map[string]any) Event {
	return Event{ID: newID(), AgreementID: a.ID, Action: action, ActorType: ActorUser, ActorUserID: userPtr(p.UserID), ActorName: p.Name, Detail: detail, At: at}
}

func callerEmail(p *auth.Principal) string { return identity.NormalizeEmail(p.Email) }

// Get returns one agreement.
func (s *Service) Get(ctx context.Context, p *auth.Principal, id uuid.UUID) (View, error) {
	a, err := s.visible(ctx, p, id)
	if err != nil {
		return View{}, err
	}
	if a.Status == StatusOutForSignature && allFinished(&a) {
		if done, err := s.complete(ctx, a.ID); err == nil && done != nil {
			a = *done
		}
	}
	v := view(a, callerEmail(p))
	s.withScan(ctx, &v, a)
	return v, nil
}

// withScan sets the scan status of the agreement's uploaded document.
func (s *Service) withScan(ctx context.Context, v *View, a Agreement) {
	if v.Document == nil || a.Original == nil || s.files == nil {
		return
	}
	v.Document.ScanStatus = storage.ScanStatuses(ctx, s.files, a.Original.ObjectKey)[a.Original.ObjectKey]
}

// Patch changes a DRAFT: name, message, expiry, reminders and recipients
// (a field of a recipient who is no longer listed is dropped).
func (s *Service) Patch(ctx context.Context, p *auth.Principal, id uuid.UUID, in Input) (View, error) {
	if _, err := s.visible(ctx, p, id); err != nil {
		return View{}, err
	}
	if in.OrganisationID != nil {
		return View{}, &ValidationError{Fields: map[string]string{"organisation_id": "cannot change after the agreement was created"}}
	}
	now := s.clock()
	saved, err := s.store.ChangeAgreement(ctx, id, func(cur *Agreement) ([]Event, error) {
		if cur.Status != StatusDraft {
			return nil, ErrState
		}
		if err := applyInput(cur, in, false, now); err != nil {
			return nil, err
		}
		cur.UpdatedAt = now
		return nil, nil
	})
	if err != nil {
		return View{}, err
	}
	return view(saved, callerEmail(p)), nil
}

// AddDocument stores the PDF of an uploaded agreement (DRAFT); a new PDF
// drops the fields placed on the previous one.
func (s *Service) AddDocument(ctx context.Context, p *auth.Principal, id uuid.UUID, fileName string, content []byte) (View, error) {
	a, err := s.visible(ctx, p, id)
	if err != nil {
		return View{}, err
	}
	if a.Status != StatusDraft || a.Source != SourceUpload {
		return View{}, ErrState
	}
	pages, err := InspectPDF(content)
	if err != nil {
		return View{}, err
	}
	key, err := storage.NewKey(DocumentKeyPrefix)
	if err != nil {
		return View{}, err
	}
	// Quarantined until scanned when malware scanning is on (API 0.41.0):
	// it cannot be opened or sent for signature until it is CLEAN.
	scanStatus, err := storage.PutUpload(ctx, s.files, key, content, "application/pdf",
		storage.Upload{Kind: storage.UploadAgreementDocument, UploadedBy: p.UserID})
	if err != nil {
		return View{}, fmt.Errorf("store agreement document: %w", err)
	}
	name := cleanFileName(fileName)
	now := s.clock()
	var old string
	saved, err := s.store.ChangeAgreement(ctx, id, func(cur *Agreement) ([]Event, error) {
		if cur.Status != StatusDraft {
			return nil, ErrState
		}
		if cur.Original != nil {
			old = cur.Original.ObjectKey
		}
		cur.Original = &Document{ObjectKey: key, SHA256: Checksum(content), Size: int64(len(content)), FileName: name}
		cur.Pages, cur.Fields, cur.UpdatedAt = pages, nil, now
		return []Event{userEvent(cur, EventDocumentAdded, p, now, map[string]any{"sha256": cur.Original.SHA256, "pages": len(pages), "file_name": name})}, nil
	})
	if err != nil {
		s.dropObject(ctx, key)
		return View{}, err
	}
	s.dropObject(ctx, old)
	v := view(saved, callerEmail(p))
	if v.Document != nil {
		v.Document.ScanStatus = scanStatus
	}
	return v, nil
}

// PutFields replaces the fields of a DRAFT.
func (s *Service) PutFields(ctx context.Context, p *auth.Principal, id uuid.UUID, in []FieldInput) (View, error) {
	if _, err := s.visible(ctx, p, id); err != nil {
		return View{}, err
	}
	now := s.clock()
	saved, err := s.store.ChangeAgreement(ctx, id, func(cur *Agreement) ([]Event, error) {
		if cur.Status != StatusDraft {
			return nil, ErrState
		}
		if cur.Original == nil {
			return nil, ErrDocumentMissing
		}
		fields, err := checkFields(cur, in)
		if err != nil {
			return nil, err
		}
		cur.Fields, cur.UpdatedAt = fields, now
		return []Event{userEvent(cur, EventFieldsChanged, p, now, map[string]any{"fields": len(fields)})}, nil
	})
	if err != nil {
		return View{}, err
	}
	return view(saved, callerEmail(p)), nil
}

// Delete removes a DRAFT and its document.
func (s *Service) Delete(ctx context.Context, p *auth.Principal, id uuid.UUID) error {
	if _, err := s.visible(ctx, p, id); err != nil {
		return err
	}
	a, err := s.store.DeleteAgreement(ctx, id)
	if err != nil {
		return err
	}
	if a.Original != nil && a.Source == SourceUpload {
		s.dropObject(ctx, a.Original.ObjectKey)
	}
	return nil
}

// List returns one page of the agreements the caller sees, newest first.
// view: WAITING_FOR_YOU, WAITING_FOR_OTHERS, COMPLETED, DRAFTS or "".
func (s *Service) List(ctx context.Context, p *auth.Principal, status, listView string, page, size int) (Page, error) {
	if !MaySend(p) {
		return Page{}, ErrForbidden
	}
	f := scopeOf(p)
	f.Status = status
	return s.page(ctx, f, callerEmail(p), listView, page, size)
}

// AdminList returns one page of every agreement.
func (s *Service) AdminList(ctx context.Context, status string, page, size int) (Page, error) {
	return s.page(ctx, Filter{All: true, Status: status}, "", "", page, size)
}

func (s *Service) page(ctx context.Context, f Filter, email, listView string, page, size int) (Page, error) {
	all, err := s.store.Agreements(ctx, f)
	if err != nil {
		return Page{}, err
	}
	var kept []Agreement
	for _, a := range all {
		if a, err = s.fresh(ctx, a); err != nil {
			return Page{}, err
		}
		waiting := waitingFor(a, email)
		switch listView {
		case ViewWaitingForYou:
			if !waiting {
				continue
			}
		case ViewWaitingForOthers:
			if a.Status != StatusOutForSignature || waiting {
				continue
			}
		case ViewCompleted:
			if a.Status != StatusCompleted {
				continue
			}
		case ViewDrafts:
			if a.Status != StatusDraft {
				continue
			}
		}
		if f.Status == "" || a.Status == f.Status {
			kept = append(kept, a)
		}
	}
	out := Page{Items: []View{}, Page: page, PageSize: size, Total: len(kept)}
	if out.Total > 0 {
		out.TotalPages = (out.Total + size - 1) / size
	}
	start := min((page-1)*size, len(kept))
	for _, a := range kept[start:min(start+size, len(kept))] {
		out.Items = append(out.Items, view(a, email))
	}
	return out, nil
}

// Counts are the dashboard numbers.
type Counts struct {
	WaitingForYou    int
	WaitingForOthers int
}

// DashboardCounts counts what waits on the caller (as a recipient) and on
// others (agreements they sent).
func (s *Service) DashboardCounts(ctx context.Context, p *auth.Principal) (Counts, error) {
	var c Counts
	if p == nil || p.SessionScope != auth.ScopeCustomer {
		return c, nil
	}
	mine, err := s.SigningRequests(ctx, p)
	if err != nil {
		return c, err
	}
	for _, r := range mine {
		if r.CanSign {
			c.WaitingForYou++
		}
	}
	if MaySend(p) {
		list, err := s.store.Agreements(ctx, scopeOf(p))
		if err != nil {
			return c, err
		}
		for _, a := range list {
			if a.Status == StatusOutForSignature && !waitingFor(a, callerEmail(p)) && s.clock().Before(a.ExpiresAt) {
				c.WaitingForOthers++
			}
		}
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Sending, cancelling, replacing, reminding
// ---------------------------------------------------------------------------

func checkReady(a *Agreement) error {
	fields := map[string]string{}
	if a.Original == nil {
		return ErrDocumentMissing
	}
	if !slices.ContainsFunc(a.Recipients, actionable) {
		fields["recipients"] = "must include at least one SIGNER or APPROVER"
	}
	for i, r := range a.Recipients {
		if r.Role == RoleSigner && !slices.ContainsFunc(a.Fields, func(f Field) bool { return f.RecipientID == r.ID && f.Type == FieldSignature }) {
			fields[fmt.Sprintf("recipients[%d]", i)] = r.Name + " is a SIGNER and needs at least one SIGNATURE field"
		}
	}
	if _, err := checkFields(a, fieldInputs(a.Fields)); err != nil {
		return err
	}
	if len(fields) > 0 {
		return &ValidationError{Code: CodeFieldsIncomplete, Fields: fields}
	}
	return nil
}

// Send sends a DRAFT: the recipients of the first order are emailed their
// links; the others wait for their turn.
func (s *Service) Send(ctx context.Context, p *auth.Principal, id uuid.UUID) (View, error) {
	a, err := s.visible(ctx, p, id)
	if err != nil {
		return View{}, err
	}
	// An uploaded PDF still being scanned (or found infected) is not sent:
	// 409 FILE_SCAN_PENDING / FILE_INFECTED (API 0.41.0).
	if a.Original != nil && a.Source == SourceUpload {
		if err := storage.Servable(ctx, s.files, a.Original.ObjectKey); err != nil {
			return View{}, err
		}
	}
	return s.send(ctx, p, id)
}

func (s *Service) send(ctx context.Context, p *auth.Principal, id uuid.UUID) (View, error) {
	now := s.clock()
	var links []minted
	saved, err := s.store.ChangeAgreement(ctx, id, func(cur *Agreement) ([]Event, error) {
		if cur.Status != StatusDraft {
			return nil, ErrState
		}
		if err := checkReady(cur); err != nil {
			return nil, err
		}
		if !now.Before(cur.ExpiresAt) {
			return nil, &ValidationError{Fields: map[string]string{"expires_at": "has passed: choose a later date"}}
		}
		for i := range cur.Recipients {
			cur.Recipients[i].Status = RecipientWaiting
			if cur.Recipients[i].Role == RoleCC {
				cur.Recipients[i].Status = RecipientCC
			}
		}
		cur.Status, cur.SentAt, cur.UpdatedAt = StatusOutForSignature, &now, now
		var events []Event
		var err error
		links, events, err = activate(cur, now)
		if err != nil {
			return nil, err
		}
		return append([]Event{userEvent(cur, EventSent, p, now, map[string]any{"document_sha256": cur.Original.SHA256,
			"recipients": len(cur.Recipients)})}, events...), nil
	})
	if err != nil {
		return View{}, err
	}
	s.audit(ctx, "agreement.sent", slog.String("agreement_id", id.String()), slog.String("actor_user_id", p.UserID.String()))
	s.emailLinks(ctx, saved, links, false)
	return view(saved, callerEmail(p)), nil
}

// Cancel cancels a DRAFT or an agreement out for signature: every link stops.
func (s *Service) Cancel(ctx context.Context, p *auth.Principal, id uuid.UUID, reason string) (View, error) {
	if _, err := s.visible(ctx, p, id); err != nil {
		return View{}, err
	}
	clean, problem := cleanText(reason, 1000, true)
	if problem != "" {
		return View{}, &ValidationError{Fields: map[string]string{"reason": problem}}
	}
	now := s.clock()
	wasOut := false
	saved, err := s.store.ChangeAgreement(ctx, id, func(cur *Agreement) ([]Event, error) {
		if cur.Status != StatusDraft && cur.Status != StatusOutForSignature {
			return nil, ErrState
		}
		wasOut = cur.Status == StatusOutForSignature
		cur.Status, cur.CancelledAt, cur.CancelReason, cur.UpdatedAt = StatusCancelled, &now, clean, now
		for i := range cur.Links {
			if cur.Links[i].RevokedAt == nil {
				cur.Links[i].RevokedAt = &now
			}
		}
		return []Event{userEvent(cur, EventCancelled, p, now, map[string]any{"reason": clean})}, nil
	})
	if err != nil {
		return View{}, err
	}
	s.audit(ctx, "agreement.cancelled", slog.String("agreement_id", id.String()), slog.String("actor_user_id", p.UserID.String()))
	if wasOut {
		s.finished(ctx, saved)
	}
	return view(saved, callerEmail(p)), nil
}

// ReplaceRecipient changes the name or email address of a recipient who has
// not finished; their links stop working, and an ACTIVE recipient is
// emailed a new one at the new address.
func (s *Service) ReplaceRecipient(ctx context.Context, p *auth.Principal, id, recipientID uuid.UUID, in ReplaceInput) (View, error) {
	if _, err := s.visible(ctx, p, id); err != nil {
		return View{}, err
	}
	name, email, err := checkReplacement(in)
	if err != nil {
		return View{}, err
	}
	now := s.clock()
	var links []minted
	saved, err := s.store.ChangeAgreement(ctx, id, func(cur *Agreement) ([]Event, error) {
		r := cur.Recipient(recipientID)
		switch {
		case cur.Status != StatusOutForSignature:
			return nil, ErrState
		case r == nil:
			return nil, ErrRecipientNotFound
		case r.Done():
			return nil, ErrAlreadyDone
		}
		for _, o := range cur.Recipients {
			if o.ID != r.ID && o.Email == email {
				return nil, &ValidationError{Fields: map[string]string{"email": "is already a recipient of this agreement"}}
			}
		}
		before := r.Email
		if name != "" {
			r.Name = name
		}
		r.Email, r.CodeHash, r.CodeLinkID, r.CodeExpiresAt, r.CodeAttempts, r.CodeSendCount, r.CodeSentAt = email, nil, nil, nil, 0, 0, nil
		r.FirstViewedAt = nil
		for i := range cur.Links {
			if cur.Links[i].RecipientID == r.ID && cur.Links[i].RevokedAt == nil {
				cur.Links[i].RevokedAt = &now
			}
		}
		if r.Status == RecipientActive {
			m, err := mint(cur, *r, now, cur.ExpiresAt)
			if err != nil {
				return nil, err
			}
			links = append(links, m)
		}
		cur.UpdatedAt = now
		e := userEvent(cur, EventRecipientReplaced, p, now, map[string]any{"from": before, "to": email})
		e.RecipientID = &r.ID
		return []Event{e}, nil
	})
	if err != nil {
		return View{}, err
	}
	s.emailLinks(ctx, saved, links, false)
	return view(saved, callerEmail(p)), nil
}

// Remind emails an ACTIVE recipient a reminder with a new link (their
// earlier links keep working).
func (s *Service) Remind(ctx context.Context, p *auth.Principal, id, recipientID uuid.UUID) (View, error) {
	if _, err := s.visible(ctx, p, id); err != nil {
		return View{}, err
	}
	saved, links, err := s.remind(ctx, id, recipientID, &userRef{p.UserID, p.Name})
	if err != nil {
		return View{}, err
	}
	s.emailLinks(ctx, saved, links, true)
	return view(saved, callerEmail(p)), nil
}

type userRef struct {
	id   uuid.UUID
	name string
}

func (s *Service) remind(ctx context.Context, id, recipientID uuid.UUID, by *userRef) (Agreement, []minted, error) {
	now := s.clock()
	var links []minted
	saved, err := s.store.ChangeAgreement(ctx, id, func(cur *Agreement) ([]Event, error) {
		r := cur.Recipient(recipientID)
		switch {
		case cur.Status != StatusOutForSignature:
			return nil, ErrState
		case r == nil:
			return nil, ErrRecipientNotFound
		case r.Status != RecipientActive:
			return nil, ErrNotYourTurn
		}
		m, err := mint(cur, *r, now, cur.ExpiresAt)
		if err != nil {
			return nil, err
		}
		links = append(links, m)
		e := systemEvent(cur, EventReminded, now, r, map[string]any{"to": r.Email})
		if by != nil {
			e.ActorType, e.ActorUserID, e.ActorName = ActorUser, &by.id, by.name
		}
		return []Event{e}, nil
	})
	return saved, links, err
}

// reminderEvery is the interval of each reminder frequency.
var reminderEvery = map[string]time.Duration{ReminderDaily: 24 * time.Hour, ReminderEvery3Days: 72 * time.Hour, ReminderWeekly: 7 * 24 * time.Hour}

// Sweep expires the agreements whose expiry passed and sends the reminders
// that are due: one per recipient and interval since they were emailed,
// recorded under a job key so that it is sent once however often (and in
// however many processes) Sweep runs. The worker runs it periodically.
func (s *Service) Sweep(ctx context.Context) (expired, reminded int, err error) {
	list, err := s.store.Agreements(ctx, Filter{All: true, Status: StatusOutForSignature})
	if err != nil {
		return 0, 0, err
	}
	now := s.clock()
	for _, a := range list {
		fresh, err := s.fresh(ctx, a)
		if err != nil {
			return expired, reminded, err
		}
		if fresh.Status == StatusExpired {
			expired++
			continue
		}
		every, ok := reminderEvery[a.ReminderFrequency]
		if !ok {
			continue
		}
		for _, r := range a.Recipients {
			if r.Status != RecipientActive || r.ActivatedAt == nil {
				continue
			}
			slot := int(now.Sub(*r.ActivatedAt) / every)
			if slot < 1 {
				continue
			}
			key := fmt.Sprintf("reminder:%s:%s:%d", a.ID, r.ID, slot)
			first, err := s.store.ClaimJob(ctx, key, now)
			if err != nil {
				return expired, reminded, err
			}
			if !first {
				continue
			}
			saved, links, err := s.remind(ctx, a.ID, r.ID, nil)
			if err != nil {
				s.logger.WarnContext(ctx, "agreement reminder skipped", slog.String("agreement_id", a.ID.String()), slog.String("error", err.Error()))
				continue
			}
			s.emailLinks(ctx, saved, links, true)
			reminded++
		}
	}
	return expired, reminded, nil
}

// Events returns the audit trail, oldest first.
func (s *Service) Events(ctx context.Context, p *auth.Principal, id uuid.UUID) ([]EventView, error) {
	if _, err := s.visible(ctx, p, id); err != nil {
		return nil, err
	}
	events, err := s.store.Events(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]EventView, 0, len(events))
	for _, e := range events {
		detail := e.Detail
		if detail == nil {
			detail = map[string]any{}
		}
		out = append(out, EventView{ID: e.ID, Action: e.Action, ActorType: e.ActorType, ActorUserID: e.ActorUserID, ActorName: e.ActorName,
			RecipientID: e.RecipientID, IP: optional(e.IP), UserAgent: optional(e.UserAgent), Detail: detail, At: e.At})
	}
	return out, nil
}

// RawEvents returns the audit trail without a permission check (the lease
// module, which checked the lease).
func (s *Service) RawEvents(ctx context.Context, id uuid.UUID) ([]Event, error) {
	return s.store.Events(ctx, id)
}

// File is a PDF to stream.
type File struct {
	Content  io.ReadCloser
	Size     int64
	FileName string
}

func (s *Service) open(ctx context.Context, a Agreement) (File, error) {
	d := a.Original
	suffix := ""
	if a.Status == StatusCompleted && a.Final != nil {
		d, suffix = a.Final, "-signed"
	}
	if d == nil {
		return File{}, ErrDocumentMissing
	}
	rc, err := s.files.Open(ctx, d.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		return File{}, ErrDocumentMissing
	}
	if err != nil {
		return File{}, err
	}
	return File{Content: rc, Size: d.Size, FileName: a.Reference + suffix + ".pdf"}, nil
}

// PDF opens the agreement's document: the original, or the completed one.
func (s *Service) PDF(ctx context.Context, p *auth.Principal, id uuid.UUID) (File, error) {
	a, err := s.visible(ctx, p, id)
	if err != nil {
		return File{}, err
	}
	return s.open(ctx, a)
}
