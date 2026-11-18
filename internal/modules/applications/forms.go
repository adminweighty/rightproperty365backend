package applications

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Form statuses (lease templates' pattern: editing a PUBLISHED version
// creates the family's next DRAFT).
const (
	FormDraft     = "DRAFT"
	FormPublished = "PUBLISHED"
	FormRetired   = "RETIRED"
)

// Field types.
const (
	TypeShortText      = "short_text"
	TypeLongText       = "long_text"
	TypeNumber         = "number"
	TypeDate           = "date"
	TypeSingleChoice   = "single_choice"
	TypeMultipleChoice = "multiple_choice"
	TypeFile           = "file"
)

// FieldTypes returns every field type.
func FieldTypes() []string {
	return []string{TypeShortText, TypeLongText, TypeNumber, TypeDate, TypeSingleChoice, TypeMultipleChoice, TypeFile}
}

// The platform file allowlist (FR-018) and limits.
const (
	FilePDF  = "pdf"
	FileJPG  = "jpg"
	FilePNG  = "png"
	FileWEBP = "webp"
	FileHEIC = "heic"
	FileDOCX = "docx"

	// MaxFileBytes is the size cap of one file; MaxFilesPerField the most
	// files a file field may take.
	MaxFileBytes     = 10 << 20
	MaxFilesPerField = 5
	// MaxUploadBodyBytes is the request body limit of an upload (the file
	// and the multipart framing).
	MaxUploadBodyBytes = MaxFileBytes + 64<<10
)

// FileKinds returns the platform allowlist.
func FileKinds() []string { return []string{FilePDF, FileJPG, FilePNG, FileWEBP, FileHEIC, FileDOCX} }

// Definition limits.
const (
	MaxNameLength          = 120
	MaxPurposeLength       = 2000
	MaxPrivacyNoticeLength = 5000
	MaxSections            = 20
	MaxFields              = 100
	MaxOptions             = 50
	MaxLabelLength         = 200
	MaxHelpLength          = 500
	MaxDescriptionLength   = 1000
	MaxOptionLength        = 120
	MinRetentionDays       = 30
	MaxRetentionDays       = 3650

	DefaultShortTextMax = 200
	CapShortTextMax     = 500
	DefaultLongTextMax  = 5000
	CapLongTextMax      = 10000
)

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// Option is one choice of a choice field.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// VisibleIf shows a field only when an earlier choice field has one of the values.
type VisibleIf struct {
	Field  string   `json:"field"`
	Equals *string  `json:"equals,omitempty"`
	In     []string `json:"in,omitempty"`
}

// values returns the values that make the field visible.
func (v VisibleIf) values() []string {
	if v.Equals != nil {
		return []string{*v.Equals}
	}
	return v.In
}

// Field is one question.
type Field struct {
	Key       string     `json:"key"`
	Type      string     `json:"type"`
	Label     string     `json:"label"`
	Help      *string    `json:"help,omitempty"`
	Required  bool       `json:"required"`
	Options   []Option   `json:"options,omitempty"`
	Min       *float64   `json:"min,omitempty"`
	Max       *float64   `json:"max,omitempty"`
	MinLength *int       `json:"min_length,omitempty"`
	MaxLength *int       `json:"max_length,omitempty"`
	MinDate   *string    `json:"min_date,omitempty"`
	MaxDate   *string    `json:"max_date,omitempty"`
	Accept    []string   `json:"accept,omitempty"`
	MaxFiles  *int       `json:"max_files,omitempty"`
	VisibleIf *VisibleIf `json:"visible_if,omitempty"`
}

// Section groups fields.
type Section struct {
	Key         string  `json:"key"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	Fields      []Field `json:"fields"`
}

// Form is one stored version of a questionnaire.
type Form struct {
	ID             uuid.UUID
	FamilyID       uuid.UUID
	Version        int
	Status         string
	OrganisationID *uuid.UUID
	OwnerUserID    *uuid.UUID
	MarketCode     string
	Name           string
	Purpose        string
	PrivacyNotice  string
	RetentionDays  int
	ExpiresAt      *time.Time
	Sections       []Section
	CreatedBy      *uuid.UUID
	CreatedByName  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	PublishedAt    *time.Time
	RetiredAt      *time.Time
	// Submissions is how many applications were submitted on this version
	// (read with the form; never written).
	Submissions int
}

// Book returns whose form it is.
func (f Form) Book() Book {
	if f.OrganisationID != nil {
		return OrganisationBook(*f.OrganisationID)
	}
	return PersonalBook(*f.OwnerUserID)
}

// Expired reports whether the form is past its expiry.
func (f Form) Expired(now time.Time) bool { return f.ExpiresAt != nil && !now.Before(*f.ExpiresAt) }

// Fields returns every field in order.
func (f Form) Fields() []Field {
	var out []Field
	for _, s := range f.Sections {
		out = append(out, s.Fields...)
	}
	return out
}

// field returns a field by key.
func (f Form) field(key string) (Field, bool) {
	for _, s := range f.Sections {
		for _, x := range s.Fields {
			if x.Key == key {
				return x, true
			}
		}
	}
	return Field{}, false
}

// Book is whose forms and applications: an organisation's, or an approved
// direct owner's own (addressed with their user ID, as contact books are).
type Book struct {
	OrganisationID *uuid.UUID
	UserID         *uuid.UUID
}

// OrganisationBook is an organisation's book.
func OrganisationBook(id uuid.UUID) Book { return Book{OrganisationID: &id} }

// PersonalBook is a direct owner's book.
func PersonalBook(id uuid.UUID) Book { return Book{UserID: &id} }

// ID is the organisation or user ID.
func (b Book) ID() uuid.UUID {
	if b.OrganisationID != nil {
		return *b.OrganisationID
	}
	if b.UserID != nil {
		return *b.UserID
	}
	return uuid.Nil
}

// Same reports whether two books are the same.
func (b Book) Same(o Book) bool {
	return eqID(b.OrganisationID, o.OrganisationID) && eqID(b.UserID, o.UserID)
}

func eqID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Warning is a sensitive-data warning (FR-018): never a block.
type Warning struct {
	Code    string `json:"code"`
	Section string `json:"section"`
	Field   string `json:"field"`
	Match   string `json:"match"`
	Message string `json:"message"`
}

// WarningSensitiveField is the code of a sensitive-data warning.
const WarningSensitiveField = "SENSITIVE_FIELD"

// sensitivePatterns are matched against a field's key and label (lower
// case, with "_" and "-" read as spaces). Each pattern is what the warning
// names.
var sensitivePatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"passport", regexp.MustCompile(`\bpassports?\b`)},
	{"national ID", regexp.MustCompile(`\bnational (id|identity|identification|insurance)\b|\bnin\b`)},
	{"ID number", regexp.MustCompile(`\bid (number|no|card|document)\b|\bidentity (number|card|document)\b|\bomang\b`)},
	{"social security", regexp.MustCompile(`\bsocial security\b|\bssn\b|\bsocial insurance\b`)},
	{"bank account", regexp.MustCompile(`\bbank (account|details|statement)s?\b|\baccount number\b|\biban\b|\bsort code\b|\brouting number\b`)},
	{"BVN", regexp.MustCompile(`\bbvn\b|\bbank verification number\b`)},
	{"tax number", regexp.MustCompile(`\btax (number|id|identification|reference)\b|\btin\b|\butr\b|\bvat number\b`)},
	{"driving licence", regexp.MustCompile(`\bdriv(er'?s|ing) licen[cs]e\b`)},
	{"health", regexp.MustCompile(`\b(medical|health) (history|condition|record)s?\b|\bdisabilit(y|ies)\b`)},
	{"religion or ethnicity", regexp.MustCompile(`\breligion\b|\bethnicity\b|\brace\b|\btribe\b`)},
	{"card number", regexp.MustCompile(`\b(credit|debit) card\b|\bcard number\b|\bcvv\b`)},
}

const sensitiveMessage = "This looks like a government ID or other sensitive data. Collect it only when it is necessary for this application, " +
	"say why in the purpose, and keep the retention period as short as possible."

// Warnings returns the sensitive-data warnings of a form's fields.
func Warnings(sections []Section) []Warning {
	out := []Warning{}
	for _, s := range sections {
		for _, f := range s.Fields {
			text := " " + strings.NewReplacer("_", " ", "-", " ").Replace(strings.ToLower(f.Key+" "+f.Label)) + " "
			for _, p := range sensitivePatterns {
				if p.re.MatchString(text) {
					out = append(out, Warning{Code: WarningSensitiveField, Section: s.Key, Field: f.Key, Match: p.name, Message: sensitiveMessage})
					break
				}
			}
		}
	}
	return out
}

// fieldErrors collects validation messages by path.
type fieldErrors map[string]string

func (e fieldErrors) add(path, msg string) {
	if _, ok := e[path]; !ok {
		e[path] = msg
	}
}

func (e fieldErrors) err() error {
	if len(e) == 0 {
		return nil
	}
	return &ValidationError{Fields: e}
}

func runes(s string) int { return utf8.RuneCountInString(s) }

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// ValidateSections checks a form definition. complete is set on publish:
// at least one section with at least one field. Every rule of the contract
// is checked here: unique keys, visible_if pointing only at an EARLIER
// choice field and its options, choice fields with 2 or more options,
// accept within the platform allowlist, and the limits.
func ValidateSections(sections []Section, complete bool) error {
	errs := fieldErrors{}
	validateSections(errs, sections, complete)
	return errs.err()
}

func validateSections(errs fieldErrors, sections []Section, complete bool) {
	if len(sections) > MaxSections {
		errs.add("sections", fmt.Sprintf("at most %d sections", MaxSections))
		return
	}
	keys := map[string]bool{}
	// earlier holds the fields seen so far (visible_if may only look back).
	earlier := map[string]Field{}
	total := 0
	for i, s := range sections {
		sp := fmt.Sprintf("sections[%d]", i)
		switch {
		case !keyRe.MatchString(s.Key):
			errs.add(sp+".key", "must be 1 to 40 lower-case letters, digits or _ and start with a letter")
		case keys[s.Key]:
			errs.add(sp+".key", "is used twice; keys must be unique across the form")
		}
		keys[s.Key] = true
		if t := strings.TrimSpace(s.Title); t == "" {
			errs.add(sp+".title", "is required")
		} else if runes(t) > MaxLabelLength {
			errs.add(sp+".title", fmt.Sprintf("must be at most %d characters", MaxLabelLength))
		}
		if s.Description != nil && runes(*s.Description) > MaxDescriptionLength {
			errs.add(sp+".description", fmt.Sprintf("must be at most %d characters", MaxDescriptionLength))
		}
		if complete && len(s.Fields) == 0 {
			errs.add(sp+".fields", "a section needs at least one field")
		}
		for j, f := range s.Fields {
			total++
			fp := fmt.Sprintf("%s.fields[%d]", sp, j)
			validateField(errs, fp, f, keys, earlier)
			keys[f.Key] = true
			earlier[f.Key] = f
		}
	}
	if total > MaxFields {
		errs.add("sections", fmt.Sprintf("at most %d fields in all", MaxFields))
	}
	if complete && (len(sections) == 0 || total == 0) {
		errs.add("sections", "a form needs at least one section with at least one field")
	}
}

func validateField(errs fieldErrors, fp string, f Field, keys map[string]bool, earlier map[string]Field) {
	switch {
	case !keyRe.MatchString(f.Key):
		errs.add(fp+".key", "must be 1 to 40 lower-case letters, digits or _ and start with a letter")
	case keys[f.Key]:
		errs.add(fp+".key", "is used twice; keys must be unique across the form")
	}
	if !slices.Contains(FieldTypes(), f.Type) {
		errs.add(fp+".type", "must be one of "+strings.Join(FieldTypes(), ", "))
		return
	}
	if t := strings.TrimSpace(f.Label); t == "" {
		errs.add(fp+".label", "is required")
	} else if runes(t) > MaxLabelLength {
		errs.add(fp+".label", fmt.Sprintf("must be at most %d characters", MaxLabelLength))
	}
	if f.Help != nil && runes(*f.Help) > MaxHelpLength {
		errs.add(fp+".help", fmt.Sprintf("must be at most %d characters", MaxHelpLength))
	}
	choice := f.Type == TypeSingleChoice || f.Type == TypeMultipleChoice
	if choice {
		switch {
		case len(f.Options) < 2:
			errs.add(fp+".options", "a choice field needs at least 2 options")
		case len(f.Options) > MaxOptions:
			errs.add(fp+".options", fmt.Sprintf("at most %d options", MaxOptions))
		default:
			seen := map[string]bool{}
			for k, o := range f.Options {
				op := fmt.Sprintf("%s.options[%d]", fp, k)
				switch {
				case strings.TrimSpace(o.Value) == "" || runes(o.Value) > MaxOptionLength:
					errs.add(op+".value", fmt.Sprintf("is required, at most %d characters", MaxOptionLength))
				case seen[o.Value]:
					errs.add(op+".value", "is used twice in this field")
				}
				seen[o.Value] = true
				if strings.TrimSpace(o.Label) == "" || runes(o.Label) > MaxOptionLength {
					errs.add(op+".label", fmt.Sprintf("is required, at most %d characters", MaxOptionLength))
				}
			}
		}
	} else if len(f.Options) > 0 {
		errs.add(fp+".options", "only choice fields have options")
	}
	// min / max: the value of a number, the number of selections of a
	// multiple choice.
	if f.Min != nil || f.Max != nil {
		switch f.Type {
		case TypeNumber:
		case TypeMultipleChoice:
			for name, v := range map[string]*float64{"min": f.Min, "max": f.Max} {
				if v != nil && (*v < 0 || *v != math.Trunc(*v) || int(*v) > len(f.Options)) {
					errs.add(fp+"."+name, "must be a whole number of selections, at most the number of options")
				}
			}
		default:
			errs.add(fp+".min", "min and max apply to number and multiple_choice fields")
		}
		if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
			errs.add(fp+".max", "must not be less than min")
		}
	}
	if f.MinLength != nil || f.MaxLength != nil {
		limit := map[string]int{TypeShortText: CapShortTextMax, TypeLongText: CapLongTextMax}[f.Type]
		if limit == 0 {
			errs.add(fp+".max_length", "min_length and max_length apply to text fields")
		} else {
			if f.MinLength != nil && (*f.MinLength < 0 || *f.MinLength > limit) {
				errs.add(fp+".min_length", fmt.Sprintf("must be between 0 and %d", limit))
			}
			if f.MaxLength != nil && (*f.MaxLength < 1 || *f.MaxLength > limit) {
				errs.add(fp+".max_length", fmt.Sprintf("must be between 1 and %d", limit))
			}
			if f.MinLength != nil && f.MaxLength != nil && *f.MinLength > *f.MaxLength {
				errs.add(fp+".max_length", "must not be less than min_length")
			}
		}
	}
	if f.MinDate != nil || f.MaxDate != nil {
		if f.Type != TypeDate {
			errs.add(fp+".min_date", "min_date and max_date apply to date fields")
		} else {
			if f.MinDate != nil && !validDate(*f.MinDate) {
				errs.add(fp+".min_date", "must be a date (YYYY-MM-DD)")
			}
			if f.MaxDate != nil && !validDate(*f.MaxDate) {
				errs.add(fp+".max_date", "must be a date (YYYY-MM-DD)")
			}
			if f.MinDate != nil && f.MaxDate != nil && *f.MinDate > *f.MaxDate {
				errs.add(fp+".max_date", "must not be before min_date")
			}
		}
	}
	if f.Type == TypeFile {
		for k, a := range f.Accept {
			if !slices.Contains(FileKinds(), a) {
				errs.add(fmt.Sprintf("%s.accept[%d]", fp, k), "must be one of "+strings.Join(FileKinds(), ", "))
			}
		}
		if f.MaxFiles != nil && (*f.MaxFiles < 1 || *f.MaxFiles > MaxFilesPerField) {
			errs.add(fp+".max_files", fmt.Sprintf("must be between 1 and %d", MaxFilesPerField))
		}
	} else {
		if len(f.Accept) > 0 {
			errs.add(fp+".accept", "only file fields have accept")
		}
		if f.MaxFiles != nil {
			errs.add(fp+".max_files", "only file fields have max_files")
		}
	}
	if v := f.VisibleIf; v != nil {
		vp := fp + ".visible_if"
		target, ok := earlier[v.Field]
		switch {
		case v.Field == f.Key:
			errs.add(vp+".field", "a field cannot depend on itself")
		case !ok && keys[v.Field]:
			errs.add(vp+".field", "must refer to an EARLIER field (this one comes later or is a section)")
		case !ok:
			errs.add(vp+".field", "must refer to an earlier field of the form")
		case target.Type != TypeSingleChoice && target.Type != TypeMultipleChoice:
			errs.add(vp+".field", "must refer to a single_choice or multiple_choice field")
		case (v.Equals == nil) == (len(v.In) == 0):
			errs.add(vp, "set exactly one of equals or in")
		default:
			for _, val := range v.values() {
				if !slices.ContainsFunc(target.Options, func(o Option) bool { return o.Value == val }) {
					errs.add(vp, fmt.Sprintf("%q is not an option of %s", val, target.Key))
					break
				}
			}
		}
	}
}

// accepts returns the file kinds a file field accepts (all by default).
func (f Field) accepts() []string {
	if len(f.Accept) == 0 {
		return FileKinds()
	}
	return f.Accept
}

// maxFiles returns how many files a file field takes (1 by default).
func (f Field) maxFiles() int {
	if f.MaxFiles == nil {
		return 1
	}
	return *f.MaxFiles
}

// textMax returns the length cap of a text answer.
func (f Field) textMax() int {
	if f.MaxLength != nil {
		return *f.MaxLength
	}
	if f.Type == TypeLongText {
		return DefaultLongTextMax
	}
	return DefaultShortTextMax
}
