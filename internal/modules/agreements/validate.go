package agreements

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg" // uploaded signatures may be JPEG
	"image/png"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
)

var emailRe = regexp.MustCompile(`^[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>".]+$`)

func cleanText(raw string, max int, multiline bool) (string, string) {
	v := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	switch {
	case !utf8.ValidString(v):
		return "", "contains characters that are not allowed"
	case utf8.RuneCountInString(v) > max:
		return "", fmt.Sprintf("must be at most %d characters", max)
	case strings.ContainsAny(v, "<>"):
		return "", "must be plain text (no < or >)"
	}
	for _, r := range v {
		if (r == '\n' || r == '\t') && multiline {
			continue
		}
		if unicode.IsControl(r) {
			return "", "contains characters that are not allowed"
		}
	}
	return v, ""
}

func cleanFileName(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(" ._-()", r) {
			b.WriteRune(r)
		}
	}
	name := strings.TrimSpace(b.String())
	if r := []rune(name); len(r) > 120 {
		name = string(r[:120])
	}
	if name == "" {
		name = "document.pdf"
	}
	return name
}

// RecipientInput is one recipient of a create or PATCH body. ID keeps an
// existing recipient (and their fields).
type RecipientInput struct {
	ID    *string `json:"id"`
	Name  string  `json:"name"`
	Email string  `json:"email"`
	Role  string  `json:"role"`
	Order int     `json:"order"`
}

// Input is the body of POST and PATCH /v1/me/agreements. Nil fields stay.
type Input struct {
	Name              *string           `json:"name"`
	Message           *string           `json:"message"`
	ExpiresAt         *time.Time        `json:"expires_at"`
	ReminderFrequency *string           `json:"reminder_frequency"`
	Recipients        *[]RecipientInput `json:"recipients"`
	OrganisationID    *string           `json:"organisation_id"`
}

func reminderFrequencies() []string {
	return []string{ReminderNone, ReminderDaily, ReminderEvery3Days, ReminderWeekly}
}

func applyInput(a *Agreement, in Input, creating bool, now time.Time) error {
	fields := map[string]string{}
	if in.Name != nil || creating {
		raw := ""
		if in.Name != nil {
			raw = *in.Name
		}
		v, problem := cleanText(raw, MaxNameRunes, false)
		switch {
		case problem != "":
			fields["name"] = problem
		case v == "":
			fields["name"] = "is required"
		}
		a.Name = v
	}
	if in.Message != nil {
		v, problem := cleanText(*in.Message, MaxMessageRunes, true)
		if problem != "" {
			fields["message"] = problem
		}
		a.Message = v
	}
	if in.ExpiresAt != nil {
		at := in.ExpiresAt.UTC()
		switch {
		case !at.After(now.Add(time.Hour)):
			fields["expires_at"] = "must be at least an hour from now"
		case at.After(now.Add(MaxExpiry)):
			fields["expires_at"] = "must be within a year"
		default:
			a.ExpiresAt = at.Truncate(time.Second)
		}
	}
	if in.ReminderFrequency != nil {
		if !slices.Contains(reminderFrequencies(), *in.ReminderFrequency) {
			fields["reminder_frequency"] = "must be one of " + strings.Join(reminderFrequencies(), ", ")
		} else {
			a.ReminderFrequency = *in.ReminderFrequency
		}
	}
	if in.Recipients != nil {
		recipients, problems := checkRecipients(a, *in.Recipients)
		for k, v := range problems {
			fields[k] = v
		}
		if len(problems) == 0 {
			a.Recipients = recipients
			kept := a.Fields[:0]
			for _, f := range a.Fields {
				if r := a.Recipient(f.RecipientID); r != nil && r.Role != RoleCC {
					kept = append(kept, f)
				}
			}
			a.Fields = kept
		}
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

func checkRecipients(a *Agreement, in []RecipientInput) ([]Recipient, map[string]string) {
	fields := map[string]string{}
	if len(in) > MaxRecipients {
		fields["recipients"] = fmt.Sprintf("must have at most %d recipients", MaxRecipients)
	}
	seen := map[string]bool{}
	out := make([]Recipient, 0, len(in))
	for i, r := range in {
		k := "recipients[" + strconv.Itoa(i) + "]"
		name, problem := cleanText(r.Name, 120, false)
		if problem != "" || name == "" {
			fields[k+".name"] = map[bool]string{true: "is required", false: problem}[problem == ""]
		}
		email := identity.NormalizeEmail(r.Email)
		switch {
		case !emailRe.MatchString(email) || len(email) > 254:
			fields[k+".email"] = "must be a valid email address"
		case seen[email]:
			fields[k+".email"] = "is already a recipient: every recipient needs their own address"
		}
		seen[email] = true
		if r.Role != RoleSigner && r.Role != RoleApprover && r.Role != RoleCC {
			fields[k+".role"] = "must be SIGNER, APPROVER or CC"
		}
		if r.Order < 1 || r.Order > MaxRecipients {
			fields[k+".order"] = fmt.Sprintf("must be from 1 to %d", MaxRecipients)
		}
		rec := Recipient{ID: newID(), Order: r.Order, Role: r.Role, Name: name, Email: email, Status: RecipientWaiting}
		if r.ID != nil {
			id, err := uuid.Parse(*r.ID)
			old := a.Recipient(id)
			if err != nil || old == nil {
				fields[k+".id"] = "is not a recipient of this agreement"
			} else {
				rec.ID, rec.Label = old.ID, old.Label
			}
		}
		out = append(out, rec)
	}
	return out, fields
}

// FieldInput is one field of PUT /v1/me/agreements/{id}/fields.
type FieldInput struct {
	RecipientID string  `json:"recipient_id"`
	Type        string  `json:"type"`
	Page        int     `json:"page"`
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	W           float64 `json:"w"`
	H           float64 `json:"h"`
	Required    *bool   `json:"required"`
	Label       string  `json:"label"`
}

func fieldTypes() []string {
	return []string{FieldSignature, FieldInitials, FieldDateSigned, FieldName, FieldText, FieldCheckbox}
}

func fieldInputs(fields []Field) []FieldInput {
	out := make([]FieldInput, 0, len(fields))
	for _, f := range fields {
		req := f.Required
		out = append(out, FieldInput{RecipientID: f.RecipientID.String(), Type: f.Type, Page: f.Page, X: f.X, Y: f.Y, W: f.W, H: f.H, Required: &req, Label: f.Label})
	}
	return out
}

// checkFields validates fields against the agreement's pages and
// recipients: on a page, inside it, for a SIGNER or APPROVER (never a CC;
// an APPROVER has no SIGNATURE or INITIALS), and every SIGNER has at least
// one SIGNATURE field. SIGNATURE, INITIALS, NAME and DATE_SIGNED are always
// required; TEXT and CHECKBOX as sent.
func checkFields(a *Agreement, in []FieldInput) ([]Field, error) {
	problems := map[string]string{}
	if len(in) > MaxFields {
		problems["fields"] = fmt.Sprintf("must have at most %d fields", MaxFields)
	}
	out := make([]Field, 0, len(in))
	for i, f := range in {
		k := "fields[" + strconv.Itoa(i) + "]"
		rid, err := uuid.Parse(f.RecipientID)
		r := a.Recipient(rid)
		switch {
		case err != nil || r == nil:
			problems[k+".recipient_id"] = "is not a recipient of this agreement"
		case r.Role == RoleCC:
			problems[k+".recipient_id"] = "is a CC: a copy recipient fills in nothing"
		case r.Role == RoleApprover && (f.Type == FieldSignature || f.Type == FieldInitials):
			problems[k+".type"] = "an APPROVER approves without signing: no SIGNATURE or INITIALS fields"
		}
		if !slices.Contains(fieldTypes(), f.Type) {
			problems[k+".type"] = "must be one of " + strings.Join(fieldTypes(), ", ")
		}
		if f.Page < 1 || f.Page > len(a.Pages) {
			problems[k+".page"] = fmt.Sprintf("must be a page from 1 to %d", len(a.Pages))
		} else {
			p := a.Pages[f.Page-1]
			if f.W < 6 || f.H < 6 || f.X < 0 || f.Y < 0 || f.X+f.W > p.Width+0.01 || f.Y+f.H > p.Height+0.01 {
				problems[k] = fmt.Sprintf("must lie on the page (%.2f x %.2f points, from the top-left) and be at least 6 x 6 points", p.Width, p.Height)
			}
		}
		label, problem := cleanText(f.Label, 80, false)
		if problem != "" {
			problems[k+".label"] = problem
		}
		req := true
		if (f.Type == FieldText || f.Type == FieldCheckbox) && f.Required != nil {
			req = *f.Required
		}
		out = append(out, Field{ID: newID(), RecipientID: rid, Type: f.Type, Page: f.Page, X: f.X, Y: f.Y, W: f.W, H: f.H, Required: req, Label: label})
	}
	for i, r := range a.Recipients {
		if r.Role == RoleSigner && !slices.ContainsFunc(out, func(f Field) bool { return f.RecipientID == r.ID && f.Type == FieldSignature }) {
			problems[fmt.Sprintf("recipients[%d]", i)] = r.Name + " is a SIGNER and needs at least one SIGNATURE field"
		}
	}
	if len(problems) > 0 {
		return nil, &ValidationError{Code: CodeFieldsIncomplete, Fields: problems}
	}
	return out, nil
}

// ReplaceInput is the body of POST .../recipients/{rid}/replace.
type ReplaceInput struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func checkReplacement(in ReplaceInput) (string, string, error) {
	fields := map[string]string{}
	name, problem := cleanText(in.Name, 120, false)
	if problem != "" {
		fields["name"] = problem
	}
	email := identity.NormalizeEmail(in.Email)
	if !emailRe.MatchString(email) || len(email) > 254 {
		fields["email"] = "must be a valid email address"
	}
	if len(fields) > 0 {
		return "", "", &ValidationError{Fields: fields}
	}
	return name, email, nil
}

// AdoptInput is an adopted signature or initials as sent.
type AdoptInput struct {
	Kind        string `json:"kind"`
	Text        string `json:"text"`
	Font        string `json:"font"`
	ImageBase64 string `json:"image_base64"`
}

// adoptedImage decodes, checks and re-encodes a signature image as a PNG
// flattened onto white (dropping metadata). DRAWN: PNG only, 200 KB;
// UPLOADED: PNG or JPEG, 500 KB.
func adoptedImage(kind, raw string) ([]byte, string) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, ";base64,"); strings.HasPrefix(raw, "data:") && i > 0 {
		raw = raw[i+len(";base64,"):]
	}
	limit := MaxDrawnBytes
	if kind == AdoptUploaded {
		limit = MaxUploadedBytes
	}
	if raw == "" {
		return nil, "is required for a " + kind + " signature"
	}
	if len(raw) > base64.StdEncoding.EncodedLen(limit) {
		return nil, fmt.Sprintf("must be at most %d KB", limit>>10)
	}
	content, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, "must be base64"
	}
	if len(content) > limit {
		return nil, fmt.Sprintf("must be at most %d KB", limit>>10)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || (format != "png" && !(kind == AdoptUploaded && format == "jpeg")) {
		if kind == AdoptUploaded {
			return nil, "must be a PNG or JPEG image"
		}
		return nil, "must be a PNG image"
	}
	if cfg.Width < 20 || cfg.Height < 10 || cfg.Width > 2000 || cfg.Height > 1000 {
		return nil, "must be from 20 x 10 to 2000 x 1000 pixels"
	}
	img, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return nil, "could not be read"
	}
	flat := image.NewRGBA(img.Bounds())
	draw.Draw(flat, flat.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
	var out bytes.Buffer
	if err := png.Encode(&out, flat); err != nil {
		return nil, "could not be read"
	}
	return out.Bytes(), ""
}

// checkAdopt validates an adoption; for an image it returns the PNG to store.
func checkAdopt(key string, in AdoptInput, fields map[string]string) (*Adopted, []byte) {
	switch in.Kind {
	case AdoptTyped:
		text, problem := cleanText(in.Text, 120, false)
		if problem != "" || text == "" {
			fields[key+".text"] = map[bool]string{true: "is required", false: problem}[problem == ""]
		}
		font := in.Font
		if font == "" {
			font = signatureFonts[0].Key
		}
		if !knownFont(font) {
			fields[key+".font"] = "must be one of the signature fonts"
		}
		return &Adopted{Kind: AdoptTyped, Text: text, Font: font}, nil
	case AdoptDrawn, AdoptUploaded:
		img, problem := adoptedImage(in.Kind, in.ImageBase64)
		if problem != "" {
			fields[key+".image_base64"] = problem
		}
		return &Adopted{Kind: in.Kind}, img
	}
	fields[key+".kind"] = "must be TYPED, DRAWN or UPLOADED"
	return nil, nil
}

// initialsOf is "Tariro Tenant" -> "TT".
func initialsOf(name string) string {
	var b strings.Builder
	for _, w := range strings.Fields(name) {
		r, _ := utf8.DecodeRuneInString(w)
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// FieldValue is one value of a submit.
type FieldValue struct {
	FieldID string `json:"field_id"`
	Value   string `json:"value"`
}

// SubmitInput is the body of POST /v1/signing/{token}/submit.
type SubmitInput struct {
	Code        json.RawMessage `json:"code"`
	Consent     bool            `json:"consent"`
	FieldValues []FieldValue    `json:"field_values"`
	Signature   *AdoptInput     `json:"signature"`
	Initials    *AdoptInput     `json:"initials"`
}
