package agreements

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/storage"
)

type signing struct {
	a    Agreement
	r    Recipient
	link Link
}

// byToken resolves a signing link: ErrLinkNotFound, ErrLinkNoLongerValid
// (replaced, or the agreement was cancelled), ErrLinkExpired.
func (s *Service) byToken(ctx context.Context, token string) (signing, error) {
	if !plausibleToken(token) {
		return signing{}, ErrLinkNotFound
	}
	link, id, err := s.store.LinkByToken(ctx, hashToken(token))
	if err != nil {
		return signing{}, err
	}
	a, err := s.load(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return signing{}, ErrLinkNotFound
	}
	if err != nil {
		return signing{}, err
	}
	r := a.Recipient(link.RecipientID)
	switch {
	case r == nil:
		return signing{}, ErrLinkNotFound
	case link.RevokedAt != nil || a.Status == StatusCancelled:
		return signing{}, ErrLinkNoLongerValid
	case a.Status == StatusExpired || !s.clock().Before(link.ExpiresAt):
		return signing{}, ErrLinkExpired
	}
	return signing{a: a, r: *r, link: link}, nil
}

// OtherRecipient is another recipient as a signing page shows them.
type OtherRecipient struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Order  int    `json:"order"`
	Status string `json:"status"`
}

// SigningView is what a signing link shows.
type SigningView struct {
	Agreement struct {
		Reference        string     `json:"reference"`
		Name             string     `json:"name"`
		Message          *string    `json:"message"`
		Status           string     `json:"status"`
		SenderName       string     `json:"sender_name"`
		OrganisationName *string    `json:"organisation_name"`
		ExpiresAt        time.Time  `json:"expires_at"`
		DocumentSHA256   string     `json:"document_sha256"`
		FinalSHA256      *string    `json:"final_sha256"`
		Pages            []PageSize `json:"pages"`
		CompletedAt      *time.Time `json:"completed_at"`
	} `json:"agreement"`
	Recipient struct {
		ID          uuid.UUID  `json:"id"`
		Name        string     `json:"name"`
		Role        string     `json:"role"`
		Label       *string    `json:"label"`
		Status      string     `json:"status"`
		EmailMasked string     `json:"email_masked"`
		CompletedAt *time.Time `json:"completed_at"`
	} `json:"recipient"`
	Fields          []FieldView      `json:"fields"`
	OtherRecipients []OtherRecipient `json:"other_recipients"`
	CanAct          bool             `json:"can_act"`
	ReadOnly        bool             `json:"read_only"`
	CodeLength      int              `json:"code_length"`
	ConsentText     string           `json:"consent_text"`
	Statement       string           `json:"statement"`
	SignatureFonts  []FontInfo       `json:"signature_fonts"`
	LinkExpiresAt   time.Time        `json:"link_expires_at"`
}

func signingView(sg signing) SigningView {
	a, r := sg.a, sg.r
	var v SigningView
	v.Agreement.Reference, v.Agreement.Name, v.Agreement.Message, v.Agreement.Status = a.Reference, a.Name, optional(a.Message), a.Status
	v.Agreement.SenderName, v.Agreement.OrganisationName, v.Agreement.ExpiresAt = a.SenderName, optional(a.OrganisationName), a.ExpiresAt
	if a.Original != nil {
		v.Agreement.DocumentSHA256 = a.Original.SHA256
	}
	if a.Final != nil {
		v.Agreement.FinalSHA256 = &a.Final.SHA256
	}
	v.Agreement.Pages, v.Agreement.CompletedAt = a.Pages, a.CompletedAt
	v.Recipient.ID, v.Recipient.Name, v.Recipient.Role, v.Recipient.Label, v.Recipient.Status = r.ID, r.Name, r.Role, optional(r.Label), r.Status
	v.Recipient.EmailMasked, v.Recipient.CompletedAt = identity.MaskEmail(r.Email), r.CompletedAt
	v.Fields, v.OtherRecipients = []FieldView{}, []OtherRecipient{}
	for _, f := range a.Fields {
		if f.RecipientID == r.ID {
			v.Fields = append(v.Fields, fieldView(f))
		}
	}
	for _, o := range a.Recipients {
		if o.ID != r.ID {
			v.OtherRecipients = append(v.OtherRecipients, OtherRecipient{Name: o.Name, Role: o.Role, Order: o.Order, Status: o.Status})
		}
	}
	v.CanAct = a.Status == StatusOutForSignature && r.Status == RecipientActive
	v.ReadOnly = !v.CanAct
	v.CodeLength, v.ConsentText, v.Statement, v.SignatureFonts, v.LinkExpiresAt = CodeLength, ConsentText, Statement, SignatureFonts(), sg.link.ExpiresAt
	return v
}

func recipientEvent(a *Agreement, r Recipient, action, ip, ua string, at time.Time, detail map[string]any) Event {
	id := r.ID
	return Event{ID: newID(), AgreementID: a.ID, Action: action, ActorType: ActorRecipient, ActorName: r.Name, RecipientID: &id,
		IP: ip, UserAgent: truncate(ua, 500), Detail: detail, At: at}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// Signing returns what a signing link shows; the first view is audited.
func (s *Service) Signing(ctx context.Context, token, ip, ua string) (SigningView, error) {
	sg, err := s.byToken(ctx, token)
	if err != nil {
		return SigningView{}, err
	}
	if sg.r.FirstViewedAt == nil {
		now := s.clock()
		saved, err := s.store.ChangeAgreement(ctx, sg.a.ID, func(cur *Agreement) ([]Event, error) {
			r := cur.Recipient(sg.r.ID)
			if r == nil || r.FirstViewedAt != nil {
				return nil, nil
			}
			r.FirstViewedAt = &now
			return []Event{recipientEvent(cur, *r, EventViewed, ip, ua, now, nil)}, nil
		})
		if err != nil {
			return SigningView{}, err
		}
		sg.a, sg.r = saved, *saved.Recipient(sg.r.ID)
	}
	if sg.a.Status == StatusOutForSignature && allFinished(&sg.a) {
		if done, err := s.complete(ctx, sg.a.ID); err == nil && done != nil {
			sg.a = *done
		}
	}
	return signingView(sg), nil
}

// SigningPDF opens the document behind a link (the completed one once
// everybody finished).
func (s *Service) SigningPDF(ctx context.Context, token string) (File, error) {
	sg, err := s.byToken(ctx, token)
	if err != nil {
		return File{}, err
	}
	return s.open(ctx, sg.a)
}

func canAct(a Agreement, r Recipient) error {
	switch {
	case r.Done():
		return ErrAlreadyDone
	case a.Status != StatusOutForSignature || r.Status != RecipientActive:
		return ErrNotYourTurn
	}
	return nil
}

// CodeSent is the answer of POST /v1/signing/{token}/code.
type CodeSent struct {
	SentTo     string    `json:"sent_to"`
	ExpiresAt  time.Time `json:"expires_at"`
	CodeLength int       `json:"code_length"`
}

// SendCode emails the recipient a six-digit code (10 minutes, 5 tries;
// at most one every 30 seconds and 10 per recipient).
func (s *Service) SendCode(ctx context.Context, token, ip, ua string) (CodeSent, error) {
	sg, err := s.byToken(ctx, token)
	if err != nil {
		return CodeSent{}, err
	}
	if err := canAct(sg.a, sg.r); err != nil {
		return CodeSent{}, err
	}
	code, err := newCode()
	if err != nil {
		return CodeSent{}, err
	}
	now := s.clock()
	expires := now.Add(CodeTTL)
	linkID := sg.link.ID
	_, err = s.store.ChangeAgreement(ctx, sg.a.ID, func(cur *Agreement) ([]Event, error) {
		r := cur.Recipient(sg.r.ID)
		if r == nil {
			return nil, ErrLinkNotFound
		}
		if err := canAct(*cur, *r); err != nil {
			return nil, err
		}
		if r.CodeSendCount >= CodeMaxSends {
			return nil, &ResendLaterError{Exhausted: true, After: CodeTTL}
		}
		if r.CodeSentAt != nil {
			if wait := r.CodeSentAt.Add(CodeResendInterval).Sub(now); wait > 0 {
				return nil, &ResendLaterError{After: wait}
			}
		}
		r.CodeHash, r.CodeLinkID, r.CodeExpiresAt, r.CodeAttempts, r.CodeSentAt = codeHash(token, r.ID, code), &linkID, &expires, 0, &now
		r.CodeSendCount++
		return []Event{recipientEvent(cur, *r, EventCodeSent, ip, ua, now, map[string]any{"sent_to": identity.MaskEmail(r.Email)})}, nil
	})
	if err != nil {
		return CodeSent{}, err
	}
	if s.mailer != nil {
		if err := s.mailer.SendCode(ctx, sg.r.Email, CodeMail{Name: sg.r.Name, Code: code, Reference: sg.a.Reference}); err != nil {
			s.logger.ErrorContext(ctx, "agreement code email failed", slog.String("recipient_id", sg.r.ID.String()), slog.String("error", err.Error()))
		}
	}
	return CodeSent{SentTo: identity.MaskEmail(sg.r.Email), ExpiresAt: expires, CodeLength: CodeLength}, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// SubmitResult is the answer of POST /v1/signing/{token}/submit.
type SubmitResult struct {
	RecipientStatus string     `json:"recipient_status"`
	AgreementStatus string     `json:"agreement_status"`
	CompletedAt     *time.Time `json:"completed_at"`
}

// Submit signs (a SIGNER) or approves (an APPROVER): the emailed code (or,
// in development and staging, AUTH_DEMO_CODE) and the consent are required,
// and every required field of the recipient must be completed. SIGNATURE
// and INITIALS fields take the adopted signature and initials (typed
// initials default to the initials of a typed signature); NAME and
// DATE_SIGNED are filled in. The next order is then emailed, or, after the
// last recipient, the agreement is completed.
func (s *Service) Submit(ctx context.Context, token, code string, in SubmitInput, ip, ua string) (SubmitResult, error) {
	problems := map[string]string{}
	if !allDigits(code) {
		problems["code"] = "Enter the code we sent you."
	}
	if !in.Consent {
		problems["consent"] = "must be true: \"" + ConsentText + "\""
	}
	sg, err := s.byToken(ctx, token)
	if err != nil {
		return SubmitResult{}, err
	}
	if err := canAct(sg.a, sg.r); err != nil {
		return SubmitResult{}, err
	}
	r := sg.r
	mine := map[uuid.UUID]Field{}
	needs := map[string]bool{}
	for _, f := range sg.a.Fields {
		if f.RecipientID == r.ID {
			mine[f.ID] = f
			needs[f.Type] = true
		}
	}
	values := map[uuid.UUID]string{}
	for i, fv := range in.FieldValues {
		id, err := uuid.Parse(fv.FieldID)
		f, ok := mine[id]
		key := fmt.Sprintf("field_values[%d]", i)
		switch {
		case err != nil || !ok:
			problems[key+".field_id"] = "is not one of your fields"
		case f.Type == FieldText:
			v, problem := cleanText(fv.Value, MaxTextValueRunes, true)
			if problem != "" {
				problems[key+".value"] = problem
			}
			values[id] = v
		case f.Type == FieldCheckbox:
			if fv.Value != "true" && fv.Value != "false" {
				problems[key+".value"] = "must be \"true\" or \"false\""
			}
			values[id] = fv.Value
		}
	}
	for id, f := range mine {
		switch {
		case f.Type == FieldText && f.Required && values[id] == "":
			problems["fields."+id.String()] = labelOr(f, "This text field") + " is required"
		case f.Type == FieldCheckbox && f.Required && values[id] != "true":
			problems["fields."+id.String()] = labelOr(f, "This box") + " must be ticked"
		}
	}
	var sig, ini *Adopted
	var sigImg, iniImg []byte
	if r.Role == RoleSigner {
		if in.Signature == nil {
			if needs[FieldSignature] {
				problems["signature"] = "is required: adopt a signature (typed, drawn or uploaded)"
			}
		} else {
			sig, sigImg = checkAdopt("signature", *in.Signature, problems)
		}
		if in.Initials != nil {
			ini, iniImg = checkAdopt("initials", *in.Initials, problems)
		} else if needs[FieldInitials] {
			if sig != nil && sig.Kind == AdoptTyped && sig.Text != "" {
				ini = &Adopted{Kind: AdoptTyped, Text: initialsOf(sig.Text), Font: sig.Font}
			} else {
				problems["initials"] = "is required: adopt initials"
			}
		}
	}
	if len(problems) > 0 {
		return SubmitResult{}, &ValidationError{Fields: problems}
	}
	now := s.clock()
	if r.CodeHash == nil || r.CodeExpiresAt == nil || !now.Before(*r.CodeExpiresAt) || r.CodeAttempts >= CodeMaxAttempts ||
		r.CodeLinkID == nil || *r.CodeLinkID != sg.link.ID {
		return SubmitResult{}, ErrCodeExpired
	}
	verification := VerifiedByEmailCode
	demo := s.demoCode != "" && subtle.ConstantTimeCompare([]byte(code), []byte(s.demoCode)) == 1
	if demo {
		verification = VerifiedByDemoCode
	} else if len(code) != CodeLength {
		return SubmitResult{}, &ValidationError{Fields: map[string]string{"code": "Enter the code we sent you."}}
	} else if !hmac.Equal(r.CodeHash, codeHash(token, r.ID, code)) {
		attempts := 0
		if _, err := s.store.ChangeAgreement(ctx, sg.a.ID, func(cur *Agreement) ([]Event, error) {
			if x := cur.Recipient(r.ID); x != nil {
				x.CodeAttempts++
				attempts = x.CodeAttempts
			}
			return nil, nil
		}); err != nil {
			return SubmitResult{}, err
		}
		if attempts >= CodeMaxAttempts {
			return SubmitResult{}, ErrCodeExpired
		}
		return SubmitResult{}, &ValidationError{Fields: map[string]string{"code": "That code is not right."}}
	}
	var stored []string
	for _, pair := range []struct {
		a   *Adopted
		img []byte
	}{{sig, sigImg}, {ini, iniImg}} {
		if pair.a == nil || pair.img == nil {
			continue
		}
		key, err := storage.NewKey(SignatureKeyPrefix)
		if err == nil {
			err = s.files.Put(ctx, key, bytes.NewReader(pair.img), int64(len(pair.img)), "image/png")
		}
		if err != nil {
			for _, k := range stored {
				s.dropObject(ctx, k)
			}
			return SubmitResult{}, fmt.Errorf("store adopted signature: %w", err)
		}
		pair.a.ImageKey = key
		stored = append(stored, key)
	}
	var links []minted
	saved, err := s.store.ChangeAgreement(ctx, sg.a.ID, func(cur *Agreement) ([]Event, error) {
		x := cur.Recipient(r.ID)
		if x == nil {
			return nil, ErrLinkNotFound
		}
		if err := canAct(*cur, *x); err != nil {
			return nil, err
		}
		if !hmac.Equal(x.CodeHash, r.CodeHash) || x.CodeAttempts >= CodeMaxAttempts {
			return nil, ErrCodeExpired
		}
		action, status := EventSigned, RecipientSigned
		if x.Role == RoleApprover {
			action, status = EventApproved, RecipientApproved
		}
		x.Status, x.CompletedAt, x.IP, x.UserAgent, x.Verification = status, &now, ip, truncate(ua, 500), verification
		x.Signature, x.Initials, x.CodeHash, x.CodeLinkID, x.CodeExpiresAt = sig, ini, nil, nil, nil
		for i := range cur.Fields {
			f := &cur.Fields[i]
			if f.RecipientID == x.ID {
				if v, ok := values[f.ID]; ok {
					f.Value = v
				}
			}
		}
		cur.UpdatedAt = now
		events := []Event{
			recipientEvent(cur, *x, EventEmailVerified, ip, ua, now, map[string]any{"method": verification, "email": identity.MaskEmail(x.Email)}),
			recipientEvent(cur, *x, action, ip, ua, now, map[string]any{"document_sha256": cur.Original.SHA256}),
		}
		var more []Event
		var err error
		if links, more, err = activate(cur, now); err != nil {
			return nil, err
		}
		return append(events, more...), nil
	})
	if err != nil {
		for _, k := range stored {
			s.dropObject(ctx, k)
		}
		return SubmitResult{}, err
	}
	s.audit(ctx, "agreement.submitted", slog.String("agreement_id", saved.ID.String()), slog.String("recipient_id", r.ID.String()),
		slog.String("ip", ip), slog.Bool("demo_code", demo))
	s.emailLinks(ctx, saved, links, false)
	out := SubmitResult{RecipientStatus: saved.Recipient(r.ID).Status, AgreementStatus: saved.Status}
	if allFinished(&saved) {
		done, err := s.complete(ctx, saved.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "agreement completion failed", slog.String("agreement_id", saved.ID.String()), slog.String("error", err.Error()))
		} else if done != nil {
			out.AgreementStatus, out.CompletedAt = done.Status, done.CompletedAt
		}
	}
	return out, nil
}

func labelOr(f Field, fallback string) string {
	if f.Label != "" {
		return "\"" + f.Label + "\""
	}
	return fallback
}

// Decline records that the recipient will not sign or approve: the
// agreement becomes DECLINED and the sender is told.
func (s *Service) Decline(ctx context.Context, token, reason, ip, ua string) (SigningView, error) {
	clean, problem := cleanText(reason, 1000, true)
	switch {
	case problem != "":
		return SigningView{}, &ValidationError{Fields: map[string]string{"reason": problem}}
	case clean == "":
		return SigningView{}, &ValidationError{Fields: map[string]string{"reason": "is required: tell the sender why"}}
	}
	sg, err := s.byToken(ctx, token)
	if err != nil {
		return SigningView{}, err
	}
	if err := canAct(sg.a, sg.r); err != nil {
		return SigningView{}, err
	}
	now := s.clock()
	saved, err := s.store.ChangeAgreement(ctx, sg.a.ID, func(cur *Agreement) ([]Event, error) {
		x := cur.Recipient(sg.r.ID)
		if x == nil {
			return nil, ErrLinkNotFound
		}
		if err := canAct(*cur, *x); err != nil {
			return nil, err
		}
		x.Status, x.DeclinedAt, x.DeclineReason, x.CodeHash = RecipientDeclined, &now, clean, nil
		cur.Status, cur.DeclinedAt, cur.UpdatedAt = StatusDeclined, &now, now
		return []Event{recipientEvent(cur, *x, EventDeclined, ip, ua, now, map[string]any{"reason": clean})}, nil
	})
	if err != nil {
		return SigningView{}, err
	}
	s.audit(ctx, "agreement.declined", slog.String("agreement_id", saved.ID.String()), slog.String("recipient_id", sg.r.ID.String()))
	if s.mailer != nil && saved.SenderEmail != "" {
		if err := s.mailer.SendDeclined(ctx, saved.SenderEmail, DeclinedMail{Name: saved.SenderName, AgreementName: saved.Name, Reference: saved.Reference,
			RecipientName: sg.r.Name, Reason: clean, AgreementID: saved.ID}); err != nil {
			s.logger.ErrorContext(ctx, "agreement declined email failed", slog.String("agreement_id", saved.ID.String()), slog.String("error", err.Error()))
		}
	}
	s.finished(ctx, saved)
	sg.a, sg.r = saved, *saved.Recipient(sg.r.ID)
	return signingView(sg), nil
}

// ---------------------------------------------------------------------------
// Completion
// ---------------------------------------------------------------------------

func (s *Service) readObject(ctx context.Context, key string, limit int64) ([]byte, error) {
	rc, err := s.files.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}

var eventTexts = map[string]string{
	EventCreated: "Created by %s", EventDocumentAdded: "Document added by %s", EventSent: "Sent for signature by %s",
	EventEmailed: "Emailed to %s for signature", EventViewed: "Viewed by %s", EventCodeSent: "Verification code emailed to %s",
	EventEmailVerified: "Email address verified by %s", EventSigned: "Signed electronically by %s", EventApproved: "Approved by %s",
	EventReminded: "Reminder emailed to %s", EventRecipientReplaced: "Recipient replaced by %s", EventCompleted: "Agreement completed",
	EventCopySent: "Completed copy emailed to %s",
}

func auditLines(a Agreement, events []Event) []AuditLine {
	var out []AuditLine
	for _, e := range events {
		tmpl, ok := eventTexts[e.Action]
		if !ok || e.Action == EventFieldsChanged {
			continue
		}
		who := e.ActorName
		if e.RecipientID != nil {
			if r := a.Recipient(*e.RecipientID); r != nil {
				who = r.Name + " (" + r.Email + ")"
			}
		}
		text := tmpl
		if strings.Contains(tmpl, "%s") {
			text = fmt.Sprintf(tmpl, who)
		}
		var detail []string
		if e.IP != "" {
			detail = append(detail, "IP "+e.IP)
		}
		if e.UserAgent != "" && (e.Action == EventSigned || e.Action == EventApproved || e.Action == EventViewed) {
			detail = append(detail, "device "+truncate(e.UserAgent, 140))
		}
		if m, ok := e.Detail["method"].(string); ok && e.Action == EventEmailVerified {
			if m == VerifiedByDemoCode {
				detail = append(detail, "with the demonstration code of a test environment (no email): not a real verification")
			} else {
				detail = append(detail, "with a six-digit code sent to that address")
			}
		}
		out = append(out, AuditLine{At: e.At, Text: text, Detail: strings.Join(detail, "; ")})
	}
	return out
}

// complete produces the completed document of an agreement whose every
// signer and approver finished, and makes it COMPLETED; nil (no error)
// when there is nothing to do.
func (s *Service) complete(ctx context.Context, id uuid.UUID) (*Agreement, error) {
	a, err := s.store.Agreement(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Status != StatusOutForSignature || !allFinished(&a) || a.Original == nil {
		return nil, nil
	}
	original, err := s.readObject(ctx, a.Original.ObjectKey, MaxPDFBytes+1)
	if err != nil {
		return nil, fmt.Errorf("read the original document: %w", err)
	}
	images := map[string][]byte{}
	recipients := map[string]Recipient{}
	for _, r := range a.Recipients {
		recipients[r.ID.String()] = r
		for _, ad := range []*Adopted{r.Signature, r.Initials} {
			if ad != nil && ad.ImageKey != "" {
				if img, err := s.readObject(ctx, ad.ImageKey, MaxUploadedBytes*8); err == nil {
					images[ad.ImageKey] = img
				}
			}
		}
	}
	events, err := s.store.Events(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	lines := append(auditLines(a, events), AuditLine{At: now, Text: "Agreement completed"})
	report := AuditReport{Name: a.Name, Reference: a.Reference, SenderName: a.SenderName, SenderEmail: a.SenderEmail, CreatedAt: a.CreatedAt,
		CompletedAt: now, OriginalSHA256: a.Original.SHA256, Pages: len(a.Pages), VerifyURL: s.publicURL + "/verify", Lines: lines}
	if a.SentAt != nil {
		report.SentAt = *a.SentAt
	}
	content, err := RenderFinal(FinalInput{Original: original, Pages: a.Pages, Fields: a.Fields, Recipients: recipients, Images: images, Audit: report})
	if err != nil {
		return nil, err
	}
	key, err := storage.NewKey(DocumentKeyPrefix)
	if err != nil {
		return nil, err
	}
	if err := s.files.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/pdf"); err != nil {
		return nil, fmt.Errorf("store the completed document: %w", err)
	}
	final := &Document{ObjectKey: key, SHA256: Checksum(content), Size: int64(len(content)), FileName: a.Reference + "-signed.pdf"}
	until := now.Add(ReadOnlyTTL)
	var links []minted
	done := false
	saved, err := s.store.ChangeAgreement(ctx, id, func(cur *Agreement) ([]Event, error) {
		if cur.Status != StatusOutForSignature || !allFinished(cur) {
			return nil, nil
		}
		done = true
		cur.Status, cur.CompletedAt, cur.UpdatedAt, cur.Final = StatusCompleted, &now, now, final
		for i := range cur.Links {
			if cur.Links[i].RevokedAt == nil {
				cur.Links[i].ExpiresAt = until
			}
		}
		events := []Event{systemEvent(cur, EventCompleted, now, nil, map[string]any{"original_sha256": cur.Original.SHA256, "final_sha256": final.SHA256})}
		for _, r := range cur.Recipients {
			m, err := mint(cur, r, now, until)
			if err != nil {
				return nil, err
			}
			links = append(links, m)
			events = append(events, systemEvent(cur, EventCopySent, now, &r, map[string]any{"to": r.Email}))
		}
		return events, nil
	})
	if err != nil || !done {
		s.dropObject(ctx, key)
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
	s.audit(ctx, "agreement.completed", slog.String("agreement_id", id.String()), slog.String("final_sha256", final.SHA256))
	if s.mailer != nil {
		for _, m := range links {
			if err := s.mailer.SendCompleted(ctx, m.recipient.Email, CompletedMail{Name: m.recipient.Name, AgreementName: saved.Name, Reference: saved.Reference,
				Token: m.token, CompletedAt: now, AvailableUntil: until}); err != nil {
				s.logger.ErrorContext(ctx, "agreement completed email failed", slog.String("recipient_id", m.recipient.ID.String()), slog.String("error", err.Error()))
			}
		}
		if saved.SenderEmail != "" && !slices.ContainsFunc(saved.Recipients, func(r Recipient) bool { return r.Email == saved.SenderEmail }) {
			if err := s.mailer.SendCompleted(ctx, saved.SenderEmail, CompletedMail{Name: saved.SenderName, AgreementName: saved.Name, Reference: saved.Reference,
				AgreementID: saved.ID, CompletedAt: now}); err != nil {
				s.logger.ErrorContext(ctx, "agreement completed email failed", slog.String("agreement_id", saved.ID.String()), slog.String("error", err.Error()))
			}
		}
	}
	s.finished(ctx, saved)
	return &saved, nil
}

// ---------------------------------------------------------------------------
// Signed-in recipients
// ---------------------------------------------------------------------------

// SigningRequest is an agreement in which the signed-in person is a recipient.
type SigningRequest struct {
	AgreementID     uuid.UUID  `json:"agreement_id"`
	RecipientID     uuid.UUID  `json:"recipient_id"`
	Reference       string     `json:"reference"`
	Name            string     `json:"name"`
	AgreementStatus string     `json:"agreement_status"`
	Role            string     `json:"role"`
	RecipientStatus string     `json:"recipient_status"`
	SenderName      string     `json:"sender_name"`
	SentAt          *time.Time `json:"sent_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	CompletedAt     *time.Time `json:"completed_at"`
	// CanSign: it waits for this person now; open it with POST .../link.
	CanSign bool `json:"can_sign"`
}

// SigningRequests lists the agreements (not drafts or cancelled ones) in
// which a recipient has the signed-in account's email address, which
// signing in proved.
func (s *Service) SigningRequests(ctx context.Context, p *auth.Principal) ([]SigningRequest, error) {
	if p == nil || p.SessionScope != auth.ScopeCustomer {
		return nil, ErrForbidden
	}
	email := callerEmail(p)
	list, err := s.store.RecipientAgreements(ctx, email)
	if err != nil {
		return nil, err
	}
	out := []SigningRequest{}
	for _, a := range list {
		if a, err = s.fresh(ctx, a); err != nil {
			return nil, err
		}
		if a.Status == StatusCancelled || a.Status == StatusDraft {
			continue
		}
		for _, r := range a.Recipients {
			if r.Email != email {
				continue
			}
			out = append(out, SigningRequest{AgreementID: a.ID, RecipientID: r.ID, Reference: a.Reference, Name: a.Name, AgreementStatus: a.Status,
				Role: r.Role, RecipientStatus: r.Status, SenderName: a.SenderName, SentAt: a.SentAt, ExpiresAt: a.ExpiresAt, CompletedAt: a.CompletedAt,
				CanSign: a.Status == StatusOutForSignature && r.Status == RecipientActive})
		}
	}
	slices.SortStableFunc(out, func(a, b SigningRequest) int {
		if a.SentAt == nil || b.SentAt == nil {
			return 0
		}
		return b.SentAt.Compare(*a.SentAt)
	})
	return out, nil
}

// OwnLink is a signing link a signed-in recipient asked for.
type OwnLink struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// OwnLink gives a signed-in recipient a new link to their signing page
// (their emailed links keep working): while it is their turn, or read-only
// once the agreement is completed or declined. A recipient whose turn has
// not come has no link yet.
func (s *Service) OwnLink(ctx context.Context, p *auth.Principal, recipientID uuid.UUID) (OwnLink, error) {
	list, err := s.SigningRequests(ctx, p)
	if err != nil {
		return OwnLink{}, err
	}
	idx := slices.IndexFunc(list, func(r SigningRequest) bool { return r.RecipientID == recipientID })
	if idx < 0 {
		return OwnLink{}, ErrRecipientNotFound
	}
	now := s.clock()
	var m minted
	_, err = s.store.ChangeAgreement(ctx, list[idx].AgreementID, func(cur *Agreement) ([]Event, error) {
		r := cur.Recipient(recipientID)
		if r == nil {
			return nil, ErrRecipientNotFound
		}
		var expires time.Time
		switch {
		case cur.Status == StatusOutForSignature && (r.Status == RecipientActive || r.Done()):
			expires = cur.ExpiresAt
		case cur.Status == StatusCompleted && cur.CompletedAt != nil:
			expires = cur.CompletedAt.Add(ReadOnlyTTL)
		case cur.Status == StatusDeclined && cur.DeclinedAt != nil && r.Status != RecipientWaiting:
			expires = cur.DeclinedAt.Add(ReadOnlyTTL)
		default:
			return nil, ErrNotYourTurn
		}
		if !now.Before(expires) {
			return nil, ErrLinkExpired
		}
		var err error
		m, err = mint(cur, *r, now, expires)
		return nil, err
	})
	if err != nil {
		return OwnLink{}, err
	}
	return OwnLink{Token: m.token, ExpiresAt: m.expires}, nil
}

// SavedView is a saved signature or initials, with its image.
type SavedView struct {
	Kind        string  `json:"kind"`
	Text        *string `json:"text"`
	Font        *string `json:"font"`
	ImageBase64 *string `json:"image_base64"`
}

// SavedSignatureView is the body of GET /v1/me/signature.
type SavedSignatureView struct {
	Signature *SavedView `json:"signature"`
	Initials  *SavedView `json:"initials"`
	Fonts     []FontInfo `json:"signature_fonts"`
}

func (s *Service) savedView(ctx context.Context, a *Adopted) *SavedView {
	if a == nil {
		return nil
	}
	v := &SavedView{Kind: a.Kind, Text: optional(a.Text), Font: optional(a.Font)}
	if a.ImageKey != "" {
		if img, err := s.readObject(ctx, a.ImageKey, MaxUploadedBytes*8); err == nil {
			enc := base64.StdEncoding.EncodeToString(img)
			v.ImageBase64 = &enc
		}
	}
	return v
}

// MySignature returns the signed-in user's saved signature and initials.
func (s *Service) MySignature(ctx context.Context, p *auth.Principal) (SavedSignatureView, error) {
	if p == nil || p.SessionScope != auth.ScopeCustomer {
		return SavedSignatureView{}, ErrForbidden
	}
	saved, _, err := s.store.SavedSignature(ctx, p.UserID)
	if err != nil {
		return SavedSignatureView{}, err
	}
	return SavedSignatureView{Signature: s.savedView(ctx, saved.Signature), Initials: s.savedView(ctx, saved.Initials), Fonts: SignatureFonts()}, nil
}

// SaveSignatureInput is the body of PUT /v1/me/signature (null clears one).
type SaveSignatureInput struct {
	Signature *AdoptInput `json:"signature"`
	Initials  *AdoptInput `json:"initials"`
}

// SaveMySignature replaces the signed-in user's saved signature and
// initials. The web sends them with a submit; nothing is applied
// automatically.
func (s *Service) SaveMySignature(ctx context.Context, p *auth.Principal, in SaveSignatureInput) (SavedSignatureView, error) {
	if p == nil || p.SessionScope != auth.ScopeCustomer {
		return SavedSignatureView{}, ErrForbidden
	}
	problems := map[string]string{}
	var next SavedSignature
	var imgs [][]byte
	var adopted []*Adopted
	if in.Signature != nil {
		a, img := checkAdopt("signature", *in.Signature, problems)
		next.Signature, imgs, adopted = a, append(imgs, img), append(adopted, a)
	}
	if in.Initials != nil {
		a, img := checkAdopt("initials", *in.Initials, problems)
		next.Initials, imgs, adopted = a, append(imgs, img), append(adopted, a)
	}
	if len(problems) > 0 {
		return SavedSignatureView{}, &ValidationError{Fields: problems}
	}
	for i, img := range imgs {
		if img == nil {
			continue
		}
		key, err := storage.NewKey(SavedSignatureKeyPrefix)
		if err == nil {
			err = s.files.Put(ctx, key, bytes.NewReader(img), int64(len(img)), "image/png")
		}
		if err != nil {
			return SavedSignatureView{}, err
		}
		adopted[i].ImageKey = key
	}
	old, _, err := s.store.SavedSignature(ctx, p.UserID)
	if err != nil {
		return SavedSignatureView{}, err
	}
	next.UpdatedAt = s.clock()
	if err := s.store.PutSavedSignature(ctx, p.UserID, next); err != nil {
		return SavedSignatureView{}, err
	}
	for _, a := range []*Adopted{old.Signature, old.Initials} {
		if a != nil && a.ImageKey != "" {
			s.dropObject(ctx, a.ImageKey)
		}
	}
	return s.MySignature(ctx, p)
}

// ---------------------------------------------------------------------------
// Verification (public)
// ---------------------------------------------------------------------------

// Verification is what the public verify endpoints say about a completed
// agreement: never a name, an email address or the document itself.
type Verification struct {
	Match          *bool      `json:"match,omitempty"`
	Document       *string    `json:"document,omitempty"`
	Reference      string     `json:"reference"`
	Status         string     `json:"status"`
	CompletedAt    *time.Time `json:"completed_at"`
	OriginalSHA256 string     `json:"original_sha256"`
	FinalSHA256    string     `json:"final_sha256"`
	Signatures     int        `json:"signatures"`
	Statement      string     `json:"statement"`
}

func verification(a Agreement) Verification {
	v := Verification{Reference: a.Reference, Status: a.Status, CompletedAt: a.CompletedAt, Statement: Statement}
	if a.Original != nil {
		v.OriginalSHA256 = a.Original.SHA256
	}
	if a.Final != nil {
		v.FinalSHA256 = a.Final.SHA256
	}
	for _, r := range a.Recipients {
		if r.Status == RecipientSigned || r.Status == RecipientApproved {
			v.Signatures++
		}
	}
	return v
}

// VerifyReference returns the fingerprints of a completed agreement.
func (s *Service) VerifyReference(ctx context.Context, reference string) (Verification, error) {
	a, err := s.store.ByReference(ctx, strings.ToUpper(strings.TrimSpace(reference)))
	if err != nil {
		return Verification{}, err
	}
	if a.Status != StatusCompleted {
		return Verification{}, ErrNotFound
	}
	return verification(a), nil
}

// VerifyFile says whether a PDF is, byte for byte, the original or the
// completed document of a completed agreement.
func (s *Service) VerifyFile(ctx context.Context, content []byte) (Verification, error) {
	sum := Checksum(content)
	a, kind, err := s.store.BySHA256(ctx, sum)
	no := false
	if errors.Is(err, ErrNotFound) {
		return Verification{Match: &no, FinalSHA256: "", OriginalSHA256: "", Statement: Statement}, nil
	}
	if err != nil {
		return Verification{}, err
	}
	yes := true
	v := verification(a)
	v.Match, v.Document = &yes, &kind
	return v, nil
}

// ---------------------------------------------------------------------------
// Generated documents (leases)
// ---------------------------------------------------------------------------

// PreparedRecipient is a recipient of a generated document. Key ties its
// fields to it.
type PreparedRecipient struct {
	Key   string
	Label string
	Name  string
	Email string
	Role  string
	Order int
}

// PreparedField is a field placed by the generator.
type PreparedField struct {
	RecipientKey string
	Type         string
	Page         int
	X, Y, W, H   float64
}

// Prepared is a generated document to send at once.
type Prepared struct {
	LeaseID           *uuid.UUID
	Name              string
	Message           string
	ExpiresAt         *time.Time
	ReminderFrequency string
	OrganisationID    *uuid.UUID
	OrganisationName  string
	Document          Document
	Pages             []PageSize
	Recipients        []PreparedRecipient
	Fields            []PreparedField
}

// CreateAndSend creates an agreement for a generated document (its PDF is
// already stored) with its recipients and fields, and sends it.
func (s *Service) CreateAndSend(ctx context.Context, p *auth.Principal, in Prepared) (uuid.UUID, error) {
	if !MaySend(p) {
		return uuid.Nil, ErrForbidden
	}
	now := s.clock()
	a := Agreement{ID: newID(), Status: StatusDraft, Source: SourceLease, LeaseID: in.LeaseID, SenderID: p.UserID, SenderName: p.Name,
		SenderEmail: callerEmail(p), OrganisationID: in.OrganisationID, OrganisationName: in.OrganisationName, ExpiresAt: now.Add(DefaultExpiry),
		ReminderFrequency: ReminderNone, CreatedAt: now, UpdatedAt: now, Pages: in.Pages}
	doc := in.Document
	a.Original = &doc
	name, msg, freq := in.Name, in.Message, in.ReminderFrequency
	input := Input{Name: &name, Message: &msg, ExpiresAt: in.ExpiresAt}
	if freq != "" {
		input.ReminderFrequency = &freq
	}
	if err := applyInput(&a, input, true, now); err != nil {
		return uuid.Nil, err
	}
	keys := map[string]uuid.UUID{}
	for _, r := range in.Recipients {
		rec := Recipient{ID: newID(), Order: r.Order, Role: r.Role, Name: r.Name, Email: identity.NormalizeEmail(r.Email), Label: r.Label, Status: RecipientWaiting}
		keys[r.Key] = rec.ID
		a.Recipients = append(a.Recipients, rec)
	}
	for _, f := range in.Fields {
		a.Fields = append(a.Fields, Field{ID: newID(), RecipientID: keys[f.RecipientKey], Type: f.Type, Page: f.Page, X: f.X, Y: f.Y, W: f.W, H: f.H, Required: true})
	}
	var err error
	for range 5 {
		if a.Reference, err = s.store.NextReference(ctx, now.Year()); err != nil {
			return uuid.Nil, err
		}
		err = s.store.CreateAgreement(ctx, a, []Event{userEvent(&a, EventCreated, p, now, map[string]any{"source": SourceLease})})
		if !errors.Is(err, ErrReferenceTaken) {
			break
		}
	}
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := s.send(ctx, p, a.ID); err != nil {
		_, _ = s.store.DeleteAgreement(context.WithoutCancel(ctx), a.ID)
		return uuid.Nil, err
	}
	return a.ID, nil
}
