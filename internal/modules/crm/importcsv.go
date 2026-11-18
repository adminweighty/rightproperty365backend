package crm

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Import statuses, per row: a dry run says CREATE, DUPLICATE or ERROR; a
// real run CREATED, SKIPPED (a duplicate: nothing is overwritten) or ERROR.
const (
	OutcomeCreate    = "CREATE"
	OutcomeDuplicate = "DUPLICATE"
	OutcomeError     = "ERROR"
	OutcomeCreated   = "CREATED"
	OutcomeSkipped   = "SKIPPED"
)

// Import errors.
var (
	// ErrImportTooManyRows: more than MaxImportRows data rows.
	ErrImportTooManyRows = errors.New("too many rows")
	// ErrImportTooLarge: the file is larger than MaxImportBytes.
	ErrImportTooLarge = errors.New("file too large")
)

// ImportRow is the outcome of one data row. Row is the line in the file
// (the header is line 1).
type ImportRow struct {
	Row    int    `json:"row"`
	Status string `json:"status"`
	Name   string `json:"name"`
	// Email and Phone are the first (normalised) ones; Emails and Phones all.
	Email  *string  `json:"email"`
	Phone  *string  `json:"phone"`
	Emails []string `json:"emails"`
	Phones []string `json:"phones"`
	// DuplicateOf are the existing contacts the row duplicates;
	// DuplicateOfRow the earlier row of the file it duplicates.
	DuplicateOf    []uuid.UUID       `json:"duplicate_of,omitempty"`
	DuplicateOfRow *int              `json:"duplicate_of_row,omitempty"`
	Errors         map[string]string `json:"errors,omitempty"`
	// ContactID is the contact created (real runs only).
	ContactID *uuid.UUID `json:"contact_id,omitempty"`
}

// ImportSummary counts the rows by outcome. Created is set on a real run.
type ImportSummary struct {
	Create    int  `json:"create"`
	Duplicate int  `json:"duplicate"`
	Error     int  `json:"error"`
	Created   *int `json:"created,omitempty"`
}

// ImportReport is the answer of POST .../contacts/import.
type ImportReport struct {
	DryRun    bool          `json:"dry_run"`
	TotalRows int           `json:"total_rows"`
	Summary   ImportSummary `json:"summary"`
	Rows      []ImportRow   `json:"rows"`
}

// ImportColumns are the columns read (any order; other columns, such as the
// export's id, are ignored). Several emails, phones or tags in one cell are
// separated by ";" or "|". Only name is required; type defaults to other,
// source to import and stage to NEW.
func ImportColumns() []string {
	return []string{"name", "type", "stage", "emails", "phones", "tags", "source", "owner_user_id", "marketing_granted",
		"marketing_source", "last_contact_at", "notes"}
}

var columnAliases = map[string]string{
	"email": "emails", "phone": "phones", "tag": "tags", "note": "notes", "owner": "owner_user_id",
	"marketing_permission": "marketing_granted",
}

// unformula undoes the export's protection of a cell against spreadsheet
// formulas (a leading apostrophe before = + - @).
func unformula(s string) string {
	if len(s) > 1 && s[0] == '\'' && strings.ContainsRune("=+-@\t\r", rune(s[1])) {
		return s[1:]
	}
	return s
}

func splitMulti(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '|' })
}

type importLine struct {
	row     int
	contact Contact
	note    string
	errs    fields
}

// parseImport reads the CSV into lines (validated, not yet checked for duplicates).
func (s *Service) parseImport(ctx context.Context, a access, raw []byte, now time.Time) ([]importLine, error) {
	if len(raw) > MaxImportBytes {
		return nil, ErrImportTooLarge
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return nil, &ValidationError{Fields: map[string]string{"file": "must be a UTF-8 CSV file"}}
	}
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, &ValidationError{Fields: map[string]string{"file": "is empty"}}
	}
	if err != nil {
		return nil, &ValidationError{Fields: map[string]string{"file": "is not a valid CSV file: " + err.Error()}}
	}
	col := map[string]int{}
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(h))
		if alias, ok := columnAliases[h]; ok {
			h = alias
		}
		if _, seen := col[h]; !seen && slices.Contains(ImportColumns(), h) {
			col[h] = i
		}
	}
	if _, ok := col["name"]; !ok {
		return nil, &ValidationError{Fields: map[string]string{"file": "must have a header row with a name column"}}
	}
	types, err := s.vocab.Labels(ctx, SetContactTypes)
	if err != nil {
		return nil, err
	}
	sources, err := s.vocab.Labels(ctx, SetContactSources)
	if err != nil {
		return nil, err
	}
	owners := map[uuid.UUID]bool{}
	actor, _ := a.actor()
	var lines []importLine
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &ValidationError{Fields: map[string]string{"file": fmt.Sprintf("is not a valid CSV file (line %d): %v", line, err)}}
		}
		get := func(name string) string {
			i, ok := col[name]
			if !ok || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(unformula(rec[i]))
		}
		blank := true
		for _, v := range rec {
			if strings.TrimSpace(v) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		if len(lines) == MaxImportRows {
			return nil, ErrImportTooManyRows
		}
		f := fields{}
		c := Contact{ID: newID(), Book: a.book, Stage: StageNew, Type: TypeOther, Source: SourceImport, CreatedBy: actor, CreatedAt: now, UpdatedAt: now}
		c.Name = cleanName(get("name"), f, "name")
		c.Emails = cleanEmails(splitMulti(get("emails")), f, "emails")
		c.Phones = cleanPhones(splitMulti(get("phones")), a.market, f, "phones")
		c.Tags = cleanTags(splitMulti(get("tags")), f, "tags")
		if v := strings.ToLower(get("type")); v != "" {
			if _, ok := types[v]; !ok {
				f["type"] = "must be one of the codes in GET /v1/reference/contact_types"
			}
			c.Type = v
		}
		if _, ok := types[c.Type]; !ok && f["type"] == "" {
			f["type"] = "is required (the type other is not available)"
		}
		if v := strings.ToLower(get("source")); v != "" {
			if _, ok := sources[v]; !ok {
				f["source"] = "must be one of the codes in GET /v1/reference/contact_sources"
			}
			c.Source = v
		} else if _, ok := sources[c.Source]; !ok {
			c.Source = ""
		}
		if v := strings.ToUpper(get("stage")); v != "" {
			if !validStage(v) {
				f["stage"] = "must be one of " + strings.Join(Stages(), ", ")
			}
			c.Stage = v
		}
		if v := get("owner_user_id"); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				f["owner_user_id"] = "must be a user ID"
			} else {
				ok, seen := owners[id]
				if !seen {
					if a.personal {
						ok = id == a.p.UserID
					} else if _, ok, err = s.people.Assignable(ctx, a.book.ID(), id); err != nil {
						return nil, err
					}
					owners[id] = ok
				}
				if !ok {
					f["owner_user_id"] = "must be an active OWNER, MANAGER or AGENT of the organisation"
				}
				c.OwnerUserID = &id
			}
		}
		if v := strings.ToLower(get("marketing_granted")); v != "" {
			var granted bool
			switch v {
			case "true", "yes", "y", "1":
				granted = true
			case "false", "no", "n", "0":
			default:
				f["marketing_granted"] = "must be true or false"
			}
			src := get("marketing_source")
			if src == "" {
				src = "Import"
			}
			if utf8.RuneCountInString(src) > MaxMarketingSource {
				f["marketing_source"] = fmt.Sprintf("must be at most %d characters", MaxMarketingSource)
			}
			c.Marketing = &Marketing{Granted: granted, GrantedAt: now, Source: src, RecordedBy: actor}
		}
		if v := get("last_contact_at"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				if d, derr := time.Parse("2006-01-02", v); derr == nil {
					t, err = d, nil
				}
			}
			switch {
			case err != nil:
				f["last_contact_at"] = "must be a date (2026-11-18) or an RFC 3339 time"
			case t.After(now.Add(5 * time.Minute)):
				f["last_contact_at"] = "cannot be in the future"
			default:
				t = t.UTC().Truncate(time.Microsecond)
				c.LastContactAt = &t
			}
		}
		note := get("notes")
		if utf8.RuneCountInString(note) > MaxNoteLength {
			f["notes"] = fmt.Sprintf("must be at most %d characters", MaxNoteLength)
		}
		lines = append(lines, importLine{row: line, contact: c, note: note, errs: f})
	}
	return lines, nil
}

// Import serves POST .../contacts/import: OWNER and MANAGER only. Every row
// is validated and checked for duplicates (of contacts of the book, and of
// earlier rows of the file). A dry run only reports; a real run creates the
// rows whose outcome is CREATE, never changes an existing contact, and is
// written to the organisation's audit trail.
func (s *Service) Import(ctx context.Context, p *auth.Principal, bookID uuid.UUID, raw []byte, dryRun bool) (ImportReport, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return ImportReport{}, err
	}
	if !a.manage || a.book.OrganisationID == nil {
		return ImportReport{}, ErrForbidden
	}
	now := s.clock()
	lines, err := s.parseImport(ctx, a, raw, now)
	if err != nil {
		return ImportReport{}, err
	}
	var allEmails, allPhones []string
	for _, l := range lines {
		allEmails = append(allEmails, l.contact.Emails...)
		allPhones = append(allPhones, l.contact.Phones...)
	}
	existing, err := s.store.Matching(ctx, a.book, allEmails, allPhones)
	if err != nil {
		return ImportReport{}, err
	}
	report := ImportReport{DryRun: dryRun, TotalRows: len(lines), Rows: make([]ImportRow, 0, len(lines))}
	firstRow := map[string]int{}
	var created []Contact
	ch := Change{}
	actor, name := a.actor()
	for _, l := range lines {
		c := l.contact
		row := ImportRow{Row: l.row, Name: c.Name, Emails: nonNil(c.Emails), Phones: nonNil(c.Phones)}
		if len(c.Emails) > 0 {
			row.Email = &c.Emails[0]
		}
		if len(c.Phones) > 0 {
			row.Phone = &c.Phones[0]
		}
		if len(l.errs) > 0 {
			row.Errors = map[string]string(l.errs)
		}
		switch {
		case len(l.errs) > 0:
			row.Status = OutcomeError
			report.Summary.Error++
		default:
			for _, e := range existing {
				if (shares(e.Emails, c.Emails) || shares(e.Phones, c.Phones)) && !slices.Contains(row.DuplicateOf, e.ID) {
					row.DuplicateOf = append(row.DuplicateOf, e.ID)
				}
			}
			for _, key := range append(prefixed("e:", c.Emails), prefixed("p:", c.Phones)...) {
				if n, ok := firstRow[key]; ok && row.DuplicateOfRow == nil {
					row.DuplicateOfRow = &n
				}
			}
			if len(row.DuplicateOf) > 0 || row.DuplicateOfRow != nil {
				row.Status = OutcomeDuplicate
				if !dryRun {
					row.Status = OutcomeSkipped
				}
				report.Summary.Duplicate++
				break
			}
			row.Status = OutcomeCreate
			report.Summary.Create++
			for _, key := range append(prefixed("e:", c.Emails), prefixed("p:", c.Phones)...) {
				firstRow[key] = l.row
			}
			if !dryRun {
				id := c.ID
				row.ContactID, row.Status = &id, OutcomeCreated
				created = append(created, c)
				ch.Contacts = append(ch.Contacts, c)
				ch.Events = append(ch.Events, Event{ID: newID(), ContactID: c.ID, Kind: EventImported, ActorUserID: actor, ActorName: name,
					Data: map[string]any{"row": l.row}, At: now})
				if l.note != "" {
					ch.Notes = append(ch.Notes, Note{ID: newID(), ContactID: c.ID, Text: l.note, AuthorUserID: actor, AuthorName: name, CreatedAt: now})
				}
			}
		}
		report.Rows = append(report.Rows, row)
	}
	if dryRun {
		return report, nil
	}
	if len(created) > 0 {
		var emails, phones []string
		for _, c := range created {
			emails, phones = append(emails, c.Emails...), append(phones, c.Phones...)
		}
		items, err := s.matchItems(ctx, a.book, a.market, emails, phones)
		if err != nil {
			return ImportReport{}, err
		}
		for _, c := range created {
			var mine []Item
			for _, it := range items {
				ie, ip := identifiers(it, a.market)
				if shares(ie, c.Emails) || shares(ip, c.Phones) {
					mine = append(mine, it)
				}
			}
			linkChange(&ch, c.ID, mine, nil, nil, "", now)
		}
	}
	n := len(created)
	report.Summary.Created = &n
	ch.Audit = []orgaudit.Event{orgaudit.New(a.book.ID(), p.UserID, p.Name, orgaudit.ActionContactsImported, orgaudit.TargetContactBook,
		a.book.ID(), "Contact import", nil, map[string]any{"total_rows": report.TotalRows, "created": n,
			"duplicates": report.Summary.Duplicate, "errors": report.Summary.Error}, now)}
	if err := s.store.Apply(ctx, ch); err != nil {
		return ImportReport{}, err
	}
	return report, nil
}

func prefixed(prefix string, values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, prefix+v)
	}
	return out
}

// ExportHeader is the header row of the export.
func ExportHeader() []string {
	return []string{"id", "name", "type", "stage", "emails", "phones", "tags", "owner_user_id", "owner_name", "source",
		"marketing_granted", "marketing_granted_at", "marketing_source", "last_contact_at", "created_at"}
}

// Export serves GET .../contacts/export.csv: OWNER and MANAGER only. The
// export is written to the organisation's audit trail before any row is
// sent; rows are the live contacts, newest first (at most MaxExportRows).
func (s *Service) Export(ctx context.Context, p *auth.Principal, bookID uuid.UUID) ([][]string, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return nil, err
	}
	if !a.manage || a.book.OrganisationID == nil {
		return nil, ErrForbidden
	}
	rows, total, err := s.store.List(ctx, a.book, ListFilter{Sort: SortCreated, Limit: MaxExportRows})
	if err != nil {
		return nil, err
	}
	now := s.clock()
	audit := orgaudit.New(a.book.ID(), p.UserID, p.Name, orgaudit.ActionContactsExported, orgaudit.TargetContactBook, a.book.ID(),
		"Contact export", nil, map[string]any{"rows": len(rows), "total": total, "truncated": total > len(rows)}, now)
	if err := s.store.Apply(ctx, Change{Audit: []orgaudit.Event{audit}}); err != nil {
		return nil, err
	}
	names := map[uuid.UUID]string{}
	out := make([][]string, 0, len(rows))
	for _, c := range rows {
		owner, ownerName := "", ""
		if c.OwnerUserID != nil {
			owner = c.OwnerUserID.String()
			n, ok := names[*c.OwnerUserID]
			if !ok {
				if n, err = s.people.Name(ctx, *c.OwnerUserID); err != nil {
					return nil, err
				}
				names[*c.OwnerUserID] = n
			}
			ownerName = n
		}
		granted, grantedAt, src := "", "", ""
		if m := c.Marketing; m != nil {
			granted, grantedAt, src = strconv.FormatBool(m.Granted), m.GrantedAt.UTC().Format(time.RFC3339), m.Source
		}
		last := ""
		if c.LastContactAt != nil {
			last = c.LastContactAt.UTC().Format(time.RFC3339)
		}
		out = append(out, []string{c.ID.String(), c.Name, c.Type, c.Stage, strings.Join(c.Emails, "; "), strings.Join(c.Phones, "; "),
			strings.Join(c.Tags, "; "), owner, ownerName, c.Source, granted, grantedAt, src, last, c.CreatedAt.UTC().Format(time.RFC3339)})
	}
	return out, nil
}
