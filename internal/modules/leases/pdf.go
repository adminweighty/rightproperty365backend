package leases

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/goregular"
)

// The PDFs are made with github.com/go-pdf/fpdf (MIT licence): pure Go, no
// CGO, no external program. Text is set in the Go fonts (golang.org/x/image
// /font/gofont, BSD-3-Clause licence, embedded TTF): they cover the Latin,
// Greek and Cyrillic alphabets, so names such as "Zoë Müller-Śmiałek" are
// printed as typed. The fixed ASCII parts (reference, page numbers, the
// DRAFT watermark) use the PDF core font Helvetica.
//
// Output is deterministic: the same snapshot gives the same bytes (sorted
// catalogue, fixed dates).
//
// Every page leaves a row above the footer for each party's initials, and
// the last section has a signature block per party. RenderPDF reports where
// they are (Layout), so that the signing workflow can place its fields.

// Render modes.
const (
	// ModeDraft: every page carries a diagonal DRAFT watermark.
	ModeDraft = "DRAFT"
	// ModeSigning: the frozen copy sent for signing; no watermark.
	ModeSigning = "SIGNING"
)

// Field types the layout reports (the agreements module's field types).
const (
	FieldSignature  = "SIGNATURE"
	FieldInitials   = "INITIALS"
	FieldName       = "NAME"
	FieldDateSigned = "DATE_SIGNED"
	FieldStamp      = "STAMP"
)

// SnapshotTemplate is the template as a document was rendered from it.
type SnapshotTemplate struct {
	ID         string    `json:"id"`
	Version    int       `json:"version"`
	Name       string    `json:"name"`
	LeaseKind  string    `json:"lease_kind"`
	MarketCode string    `json:"market_code"`
	Sections   []Section `json:"sections"`
	Clauses    []Clause  `json:"optional_clauses"`
}

// Snapshot is everything a lease PDF is rendered from (the documents'
// input_snapshot). Rendering the same snapshot gives the same PDF.
type Snapshot struct {
	Reference        string           `json:"reference"`
	AgreementDate    string           `json:"agreement_date"`
	GeneratedAt      time.Time        `json:"generated_at"`
	CountryName      string           `json:"country_name"`
	OrganisationName string           `json:"organisation_name,omitempty"`
	PreparedBy       string           `json:"prepared_by"`
	Template         SnapshotTemplate `json:"template"`
	Input            Input            `json:"input"`
}

// PageSize is a page's size in PDF points.
type PageSize struct {
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// PlacedField is a place left for a party: in PDF points from the page's
// top-left corner. Role and Position name the party (Position counts from 1
// within the role).
type PlacedField struct {
	Role     string
	Position int
	Type     string
	Page     int
	X, Y     float64
	W, H     float64
}

// Layout is where the parties' fields are, and the pages' sizes.
type Layout struct {
	Pages  []PageSize
	Fields []PlacedField
}

const (
	fontText   = "gotext"
	pageMargin = 20.0
	// bottomSpace keeps the body clear of the initials row and the footer.
	bottomSpace = 34.0
	mmToPt      = 72.0 / 25.4
	pageWidthMM = 210.0
	initialsW   = 12.0
	initialsH   = 8.0
	initialsGap = 2.0
)

type renderer struct {
	snap   Snapshot
	mode   string
	total  int // pages of the document
	f      *fpdf.Fpdf
	layout Layout
}

// RenderPDF renders a lease and reports its layout.
func RenderPDF(snap Snapshot, mode string) ([]byte, Layout, error) {
	// First pass: count the pages, for "page x of y".
	first := &renderer{snap: snap, mode: mode}
	if _, err := first.render(); err != nil {
		return nil, Layout{}, err
	}
	second := &renderer{snap: snap, mode: mode, total: first.total}
	content, err := second.render()
	if err != nil {
		return nil, Layout{}, err
	}
	return content, second.layout, nil
}

func (r *renderer) parties() []PlacedField {
	var out []PlacedField
	for i := range r.snap.Input.Landlords {
		out = append(out, PlacedField{Role: RoleLandlord, Position: i + 1})
	}
	for i := range r.snap.Input.Tenants {
		out = append(out, PlacedField{Role: RoleTenant, Position: i + 1})
	}
	return out
}

func (r *renderer) place(p PlacedField, kind string, x, y, w, h float64) {
	p.Type, p.Page = kind, r.f.PageNo()
	p.X, p.Y, p.W, p.H = x*mmToPt, y*mmToPt, w*mmToPt, h*mmToPt
	r.layout.Fields = append(r.layout.Fields, p)
}

func (r *renderer) render() ([]byte, error) {
	f := fpdf.New("P", "mm", "A4", "")
	r.f = f
	f.SetCatalogSort(true)
	at := r.snap.GeneratedAt.UTC()
	f.SetCreationDate(at)
	f.SetModificationDate(at)
	f.SetProducer("RightProperty365", false)
	f.SetCreator("RightProperty365", false)
	f.SetTitle("Lease "+r.snap.Reference, true)
	f.SetSubject(map[string]string{ModeDraft: "Draft lease", ModeSigning: "Lease for signature"}[r.mode], true)
	f.SetKeywords(r.snap.Reference, true)
	f.AddUTF8FontFromBytes(fontText, "", goregular.TTF)
	f.AddUTF8FontFromBytes(fontText, "B", gobold.TTF)
	f.AddUTF8FontFromBytes(fontText, "I", goitalic.TTF)
	f.SetMargins(pageMargin, pageMargin, pageMargin)
	f.SetAutoPageBreak(true, bottomSpace)
	f.SetHeaderFunc(r.header)
	f.SetFooterFunc(r.footer)

	f.AddPage()
	r.body()
	r.total = f.PageNo()
	if err := f.Error(); err != nil {
		return nil, fmt.Errorf("render lease: %w", err)
	}
	var buf bytes.Buffer
	if err := f.Output(&buf); err != nil {
		return nil, fmt.Errorf("render lease: %w", err)
	}
	w, h := f.GetPageSize()
	for range r.total {
		r.layout.Pages = append(r.layout.Pages, PageSize{Width: round2(w * mmToPt), Height: round2(h * mmToPt)})
	}
	return buf.Bytes(), nil
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

// header draws the watermark of a draft, under everything else on the page.
func (r *renderer) header() {
	if r.mode != ModeDraft {
		return
	}
	f := r.f
	w, h := f.GetPageSize()
	f.SetFont("Helvetica", "B", 110)
	f.SetTextColor(225, 225, 225)
	f.TransformBegin()
	f.TransformRotate(45, w/2, h/2)
	text := "DRAFT"
	f.Text(w/2-f.GetStringWidth(text)/2, h/2+15, text)
	f.TransformEnd()
	f.SetTextColor(0, 0, 0)
	f.SetXY(pageMargin, pageMargin)
}

func (r *renderer) footer() {
	f := r.f
	_, h := f.GetPageSize()
	// The initials row: one labelled box per party.
	y := h - 27
	f.SetFont("Helvetica", "", 7)
	f.SetTextColor(110, 110, 110)
	f.Text(pageMargin, y+initialsH-2.5, "Initials")
	f.SetDrawColor(170, 170, 170)
	x := pageMargin + 12
	for _, p := range r.parties() {
		f.Rect(x, y, initialsW, initialsH, "D")
		f.Text(x, y+initialsH+2.6, fmt.Sprintf("%s%d", p.Role[:1], p.Position))
		r.place(p, FieldInitials, x, y, initialsW, initialsH)
		x += initialsW + initialsGap
	}
	f.SetY(-15)
	f.SetDrawColor(200, 200, 200)
	f.Line(pageMargin, f.GetY(), pageWidthMM-pageMargin, f.GetY())
	f.SetY(-13)
	f.SetTextColor(90, 90, 90)
	f.SetFont("Helvetica", "", 8)
	f.CellFormat(45, 5, r.snap.Reference, "", 0, "L", false, 0, "")
	f.SetFont(fontText, "", 8)
	f.CellFormat(80, 5, fmt.Sprintf("%s (template version %d)", r.snap.Template.Name, r.snap.Template.Version), "", 0, "C", false, 0, "")
	f.SetFont("Helvetica", "", 8)
	right := fmt.Sprintf("Page %d", f.PageNo())
	if r.total > 0 {
		right = fmt.Sprintf("Page %d of %d", f.PageNo(), r.total)
	}
	f.CellFormat(0, 5, right, "", 0, "R", false, 0, "")
	f.SetTextColor(0, 0, 0)
}

func (r *renderer) heading(text string, size float64) {
	r.f.SetFont(fontText, "B", size)
	r.f.MultiCell(0, size*0.5, text, "", "L", false)
	r.f.Ln(1.5)
}

func (r *renderer) para(text string) {
	r.f.SetFont(fontText, "", 10.5)
	r.f.MultiCell(0, 5.2, text, "", "L", false)
	r.f.Ln(2.5)
}

func (r *renderer) fact(label, value string) {
	f := r.f
	f.SetFont(fontText, "B", 10)
	y := f.GetY()
	f.MultiCell(45, 5.2, label, "", "L", false)
	after := f.GetY()
	f.SetXY(pageMargin+45, y)
	f.SetFont(fontText, "", 10)
	f.MultiCell(0, 5.2, value, "", "L", false)
	if f.GetY() < after {
		f.SetY(after)
	}
	f.Ln(0.8)
}

// RoleLabel is "Landlord" / "Tenant", plural for n > 1.
func RoleLabel(role string, n int) string {
	label := map[string]string{RoleLandlord: "Landlord", RoleTenant: "Tenant"}[role]
	if n > 1 {
		label += "s"
	}
	return label
}

func (r *renderer) body() {
	s, in := r.snap, r.snap.Input
	values := Values(in, s.Reference, s.CountryName, s.Template.LeaseKind, s.AgreementDate)

	r.heading(s.Template.Name, 17)
	r.f.SetFont(fontText, "", 10)
	kind := map[string]string{KindResidential: "Residential lease", KindCommercial: "Commercial lease"}[s.Template.LeaseKind]
	r.f.MultiCell(0, 5, fmt.Sprintf("%s · %s · Reference %s", kind, s.CountryName, s.Reference), "", "L", false)
	prepared := "Prepared on " + LongDate(s.AgreementDate) + " by " + s.PreparedBy
	if s.OrganisationName != "" {
		prepared += ", " + s.OrganisationName
	}
	r.f.MultiCell(0, 5, prepared+".", "", "L", false)
	r.f.Ln(4)

	r.heading("Parties and key terms", 12)
	r.fact(RoleLabel(RoleLandlord, len(in.Landlords)), values["landlord_details"])
	r.fact(RoleLabel(RoleTenant, len(in.Tenants)), values["tenant_details"])
	r.fact("Property", in.PropertyAddress)
	r.fact("Term", strings.ToUpper(values["term"][:1])+values["term"][1:])
	r.fact("Rent", fmt.Sprintf("%s %s per %s", in.RentCurrency, values["rent_amount"], values["rent_period"]))
	r.fact("Rent is due", values["payment_day"])
	r.fact("Deposit", values["deposit_amount"])
	r.fact("Notice period", values["notice_period_days"]+" days")
	r.f.Ln(3)

	n := 0
	usesSpecial := false
	for _, sec := range s.Template.Sections {
		n++
		r.heading(strconv.Itoa(n)+". "+Fill(sec.Heading, values), 11.5)
		r.para(Fill(sec.Text, values))
		usesSpecial = usesSpecial || strings.Contains(sec.Text, "special_conditions")
	}
	for _, code := range in.OptionalClauses {
		for _, c := range s.Template.Clauses {
			if c.Code == code {
				n++
				r.heading(strconv.Itoa(n)+". "+c.Title, 11.5)
				r.para(Fill(c.Text, values))
			}
		}
	}
	if !usesSpecial && strings.TrimSpace(in.SpecialConditions) != "" {
		n++
		r.heading(strconv.Itoa(n)+". Special conditions", 11.5)
		r.para(in.SpecialConditions)
	}

	n++
	r.heading(strconv.Itoa(n)+". Signatures", 11.5)
	r.para("This lease is signed electronically on RightProperty365 by each landlord and each tenant named above, " +
		"who also initial every page. The audit report added to the signed document records who signed, when, " +
		"how their email address was confirmed and the SHA-256 fingerprints of the document.")
	if in.LandlordStamp {
		r.para("Each landlord also applies their stamp, generated by RightProperty365 from the text they enter.")
	}
	if r.mode == ModeDraft {
		r.para("This is a draft. It cannot be signed until it is sent for signing.")
	}
	r.signatureBlocks()
}

// Stamp box of a landlord who stamps the lease: 40 mm square, to the right
// of the name and date.
const (
	stampSize   = 40.0
	stampBlockH = 52.0
)

// signatureBlocks draws one block per party: signature, name and date, and
// for each landlord a stamp box when the lease asks for landlords' stamps.
func (r *renderer) signatureBlocks() {
	f := r.f
	_, h := f.GetPageSize()
	const blockH = 34.0
	for _, p := range r.parties() {
		stamp := p.Role == RoleLandlord && r.snap.Input.LandlordStamp
		need := blockH
		if stamp {
			need = stampBlockH
		}
		if f.GetY()+need > h-bottomSpace {
			f.AddPage()
		}
		y := f.GetY()
		name := r.snap.Input.Landlords
		if p.Role == RoleTenant {
			name = r.snap.Input.Tenants
		}
		f.SetFont(fontText, "B", 10)
		f.Text(pageMargin, y+4, fmt.Sprintf("%s %d: %s", RoleLabel(p.Role, 1), p.Position, name[p.Position-1].Name))
		f.SetDrawColor(170, 170, 170)
		f.SetFont("Helvetica", "", 7)
		f.SetTextColor(110, 110, 110)
		f.Rect(pageMargin, y+7, 75, 18, "D")
		f.Text(pageMargin, y+28, "Signature")
		r.place(p, FieldSignature, pageMargin, y+7, 75, 18)
		nameW := 65.0
		if stamp {
			nameW = 40
		}
		f.Rect(pageMargin+85, y+7, nameW, 7, "D")
		f.Text(pageMargin+85, y+16.5, "Name")
		r.place(p, FieldName, pageMargin+85, y+7, nameW, 7)
		f.Rect(pageMargin+85, y+18, 40, 7, "D")
		f.Text(pageMargin+85, y+27.5, "Date signed")
		r.place(p, FieldDateSigned, pageMargin+85, y+18, 40, 7)
		if stamp {
			x := pageWidthMM - pageMargin - stampSize
			f.Rect(x, y+7, stampSize, stampSize, "D")
			f.Text(x, y+stampSize+10, "Stamp")
			r.place(p, FieldStamp, x, y+7, stampSize, stampSize)
		}
		f.SetTextColor(0, 0, 0)
		f.SetY(y + need)
	}
}

// Checksum returns the hex SHA-256 of content.
func Checksum(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
