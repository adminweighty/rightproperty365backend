package agreements

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/go-pdf/fpdf"
	"github.com/go-pdf/fpdf/contrib/gofpdi"
	realgofpdi "github.com/phpdave11/gofpdi"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gobolditalic"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/gomediumitalic"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/sfnt"
)

// Documents are read and written with pure-Go libraries: the original
// pages are imported with github.com/phpdave11/gofpdi (MIT) into
// github.com/go-pdf/fpdf (MIT), the field values are drawn on top, and the
// audit report is appended. Text fonts are the Go fonts (BSD-3-Clause,
// embedded); typed signatures use the script fonts below (OFL-1.1).
//
// Typed signatures: three embedded script fonts (SignatureFonts), vendored in
// fonts/ with their SIL Open Font License 1.1 texts: Great Vibes, Kaushan
// Script and Allura (Google Fonts). The style keys stored with a signature
// predate the fonts and stay as they are. A name with a letter a script font
// does not have (Cyrillic, Greek, ...) is drawn in Go Italic instead, so no
// letter is ever left out.

//go:embed fonts/GreatVibes-Regular.ttf
var greatVibesTTF []byte

//go:embed fonts/KaushanScript-Regular.ttf
var kaushanScriptTTF []byte

//go:embed fonts/Allura-Regular.ttf
var alluraTTF []byte

// FontInfo is one typed-signature style.
type FontInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type signatureFont struct {
	FontInfo
	ttf      []byte
	fallback []byte
	face     *sfnt.Font
}

var signatureFonts = []signatureFont{
	{FontInfo: FontInfo{"FLOWING", "Flowing (Great Vibes)"}, ttf: greatVibesTTF, fallback: goitalic.TTF},
	{FontInfo: FontInfo{"BOLD", "Bold (Kaushan Script)"}, ttf: kaushanScriptTTF, fallback: gobolditalic.TTF},
	{FontInfo: FontInfo{"CLASSIC", "Classic (Allura)"}, ttf: alluraTTF, fallback: gomediumitalic.TTF},
}

func init() {
	for i := range signatureFonts {
		face, err := sfnt.Parse(signatureFonts[i].ttf)
		if err != nil {
			panic(fmt.Sprintf("signature font %s: %v", signatureFonts[i].Key, err))
		}
		signatureFonts[i].face = face
	}
}

// covers reports whether the font has a glyph for every letter of text.
func (sf signatureFont) covers(text string) bool {
	var buf sfnt.Buffer
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		if g, err := sf.face.GlyphIndex(&buf, r); err != nil || g == 0 {
			return false
		}
	}
	return true
}

// fontFor returns the fpdf font name a typed signature is drawn with.
func fontFor(key, text string) string {
	for _, sf := range signatureFonts {
		if sf.Key == key {
			name := "sig" + strings.ToLower(sf.Key)
			if !sf.covers(text) {
				name += "fb"
			}
			return name
		}
	}
	return fontFor(signatureFonts[0].Key, text)
}

// SignatureFonts lists the styles a typed signature can use.
func SignatureFonts() []FontInfo {
	out := make([]FontInfo, 0, len(signatureFonts))
	for _, f := range signatureFonts {
		out = append(out, f.FontInfo)
	}
	return out
}

func knownFont(key string) bool {
	for _, f := range signatureFonts {
		if f.Key == key {
			return true
		}
	}
	return false
}

// Checksum returns the hex SHA-256 of content.
func Checksum(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// InspectPDF checks an uploaded PDF and returns its pages' sizes: it must
// start like a PDF, open without a password and import page by page.
func InspectPDF(content []byte) (pages []PageSize, err error) {
	if len(content) > MaxPDFBytes {
		return nil, ErrFileTooLarge
	}
	if !bytes.HasPrefix(bytes.TrimLeft(content, "\x00\t\r\n "), []byte("%PDF-")) {
		return nil, ErrDocumentInvalid
	}
	defer func() {
		if recover() != nil {
			pages, err = nil, ErrDocumentInvalid
		}
	}()
	imp := realgofpdi.NewImporter()
	rs := io.ReadSeeker(bytes.NewReader(content))
	imp.SetSourceStream(&rs)
	n := imp.GetNumPages()
	if n < 1 {
		return nil, ErrDocumentInvalid
	}
	if n > MaxPDFPages {
		return nil, &ValidationError{Code: CodeDocumentInvalid, Fields: map[string]string{"file": fmt.Sprintf("must have at most %d pages", MaxPDFPages)}}
	}
	sizes := imp.GetPageSizes()
	for i := 1; i <= n; i++ {
		box := sizes[i]["/MediaBox"]
		w, h := box["w"], box["h"]
		if w < 72 || h < 72 || w > 14400 || h > 14400 {
			return nil, ErrDocumentInvalid
		}
		pages = append(pages, PageSize{Width: round2(w), Height: round2(h)})
	}
	// Import every page once, as completion will.
	if _, err := drawPages(content, pages, func(*fpdf.Fpdf, int) {}); err != nil {
		return nil, ErrDocumentInvalid
	}
	return pages, nil
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

func newPDF() *fpdf.Fpdf {
	f := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 595.28, Ht: 841.89}})
	f.SetAutoPageBreak(false, 0)
	f.SetMargins(0, 0, 0)
	f.AddUTF8FontFromBytes("gotext", "", goregular.TTF)
	f.AddUTF8FontFromBytes("gotext", "B", gobold.TTF)
	for _, sf := range signatureFonts {
		f.AddUTF8FontFromBytes("sig"+strings.ToLower(sf.Key), "", sf.ttf)
		f.AddUTF8FontFromBytes("sig"+strings.ToLower(sf.Key)+"fb", "", sf.fallback)
	}
	f.SetProducer("RightProperty365", false)
	f.SetCreator("RightProperty365", false)
	return f
}

// drawPages imports every original page into a new PDF and calls overlay
// on each.
func drawPages(original []byte, pages []PageSize, overlay func(f *fpdf.Fpdf, page int)) (f *fpdf.Fpdf, err error) {
	defer func() {
		if r := recover(); r != nil {
			f, err = nil, fmt.Errorf("import the document's pages: %v", r)
		}
	}()
	f = newPDF()
	imp := gofpdi.NewImporter()
	for i, p := range pages {
		f.AddPageFormat("P", fpdf.SizeType{Wd: p.Width, Ht: p.Height})
		rs := io.ReadSeeker(bytes.NewReader(original))
		tpl := imp.ImportPageFromStream(f, &rs, i+1, "/MediaBox")
		imp.UseImportedTemplate(f, tpl, 0, 0, p.Width, p.Height)
		overlay(f, i+1)
	}
	return f, f.Error()
}

// AuditLine is one event of the audit report.
type AuditLine struct {
	At     time.Time
	Text   string
	Detail string
}

// AuditReport is the page appended to a completed agreement.
type AuditReport struct {
	Name, Reference         string
	SenderName, SenderEmail string
	CreatedAt, SentAt       time.Time
	CompletedAt             time.Time
	OriginalSHA256          string
	Pages                   int
	VerifyURL               string
	Lines                   []AuditLine
}

// FinalInput is everything the completed document is drawn from.
type FinalInput struct {
	Original   []byte
	Pages      []PageSize
	Fields     []Field
	Recipients map[string]Recipient
	// Images are the adopted signature images by object key (PNG); the
	// stamps rendered while drawing are kept here as "stamp:<recipient ID>".
	Images map[string][]byte
	Audit  AuditReport
}

// Statement is printed on every completed document (never "legally executed").
const Statement = "Signed electronically on RightProperty365. Whether this document is enforceable, and whether it must also be " +
	"witnessed, stamped or registered, depends on the law of the country concerned; RightProperty365 does not witness, stamp or " +
	"register documents and does not give legal advice."

// SealNote says that no digital seal is applied (no platform certificate).
const SealNote = "Digital seal: none. No platform certificate is configured, so this file carries no PDF digital signature; " +
	"its integrity can be checked against the SHA-256 recorded by RightProperty365."

func formatUTC(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2 January 2006, 15:04:05") + " UTC"
}

func fitText(f *fpdf.Fpdf, font, text string, w, h, max float64) float64 {
	size := min(h*0.72, max)
	f.SetFont(font, "", size)
	for size > 5 && f.GetStringWidth(text) > w*0.96 {
		size -= 0.5
		f.SetFont(font, "", size)
	}
	return size
}

func (in FinalInput) drawAdopted(f *fpdf.Fpdf, a *Adopted, fd Field) {
	if a == nil {
		return
	}
	if a.Kind == AdoptTyped {
		font := fontFor(a.Font, a.Text)
		size := fitText(f, font, a.Text, fd.W, fd.H, 40)
		f.SetTextColor(20, 30, 120)
		f.Text(fd.X+2, fd.Y+fd.H/2+size*0.35, a.Text)
		f.SetTextColor(0, 0, 0)
		return
	}
	img := in.Images[a.ImageKey]
	if len(img) == 0 {
		return
	}
	name := "img" + Checksum(img)[:16]
	info := f.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(img))
	if info == nil || f.Error() != nil {
		return
	}
	drawImage(f, name, info, fd)
}

// drawImage draws a registered image inside the field, keeping its aspect
// ratio, centred.
func drawImage(f *fpdf.Fpdf, name string, info *fpdf.ImageInfoType, fd Field) {
	w, h := fd.W, fd.W*info.Height()/info.Width()
	if h > fd.H {
		h, w = fd.H, fd.H*info.Width()/info.Height()
	}
	f.ImageOptions(name, fd.X+(fd.W-w)/2, fd.Y+(fd.H-h)/2, w, h, false, fpdf.ImageOptions{ImageType: "PNG"}, 0, "")
}

// drawStamp draws the recipient's generated stamp, dated the day they
// signed, inside a STAMP field (a transparent PNG over the page).
func (in FinalInput) drawStamp(f *fpdf.Fpdf, r Recipient, fd Field) {
	if r.Stamp == nil {
		return
	}
	key := "stamp:" + r.ID.String()
	img := in.Images[key]
	if len(img) == 0 {
		day := in.Audit.CompletedAt
		if r.CompletedAt != nil {
			day = *r.CompletedAt
		}
		var err error
		if img, err = RenderStamp(*r.Stamp, day.UTC()); err != nil {
			return
		}
		if in.Images != nil {
			in.Images[key] = img
		}
	}
	name := "stamp" + Checksum(img)[:16]
	info := f.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(img))
	if info == nil || f.Error() != nil {
		return
	}
	drawImage(f, name, info, fd)
}

func (in FinalInput) drawField(f *fpdf.Fpdf, fd Field) {
	r, ok := in.Recipients[fd.RecipientID.String()]
	if !ok || (r.Status != RecipientSigned && r.Status != RecipientApproved) {
		return
	}
	switch fd.Type {
	case FieldSignature:
		in.drawAdopted(f, r.Signature, fd)
	case FieldInitials:
		in.drawAdopted(f, r.Initials, fd)
	case FieldStamp:
		in.drawStamp(f, r, fd)
	case FieldName, FieldDateSigned, FieldText:
		value := fd.Value
		switch fd.Type {
		case FieldName:
			value = r.Name
		case FieldDateSigned:
			if r.CompletedAt != nil {
				value = r.CompletedAt.UTC().Format("2 January 2006")
			}
		}
		if value == "" {
			return
		}
		size := fitText(f, "gotext", value, fd.W, fd.H, 11)
		f.Text(fd.X+2, fd.Y+fd.H/2+size*0.35, value)
	case FieldCheckbox:
		if fd.Value == "true" {
			f.SetLineWidth(1.4)
			f.SetDrawColor(20, 30, 120)
			f.Line(fd.X+fd.W*0.2, fd.Y+fd.H*0.55, fd.X+fd.W*0.42, fd.Y+fd.H*0.8)
			f.Line(fd.X+fd.W*0.42, fd.Y+fd.H*0.8, fd.X+fd.W*0.82, fd.Y+fd.H*0.2)
			f.SetLineWidth(0.5)
		}
	}
}

// RenderFinal draws every field value onto the original pages and appends
// the audit report.
func RenderFinal(in FinalInput) ([]byte, error) {
	f, err := drawPages(in.Original, in.Pages, func(f *fpdf.Fpdf, page int) {
		for _, fd := range in.Fields {
			if fd.Page == page {
				in.drawField(f, fd)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	a := in.Audit
	f.SetTitle(a.Name, true)
	f.SetSubject("Completed agreement "+a.Reference, true)
	f.SetKeywords(a.Reference, true)
	f.SetCreationDate(a.CompletedAt.UTC())
	f.SetModificationDate(a.CompletedAt.UTC())
	const margin = 42.0
	f.SetMargins(margin, margin, margin)
	f.SetAutoPageBreak(true, margin+14)
	// The footer is set before the audit report's first page, so AddPage
	// would also draw it on the last original page while closing it: it is
	// drawn only once the audit report has started, numbered within it.
	auditStarted, firstAuditPage := false, 0
	f.SetFooterFunc(func() {
		if !auditStarted {
			return
		}
		f.SetY(-margin + 6)
		f.SetFont("Helvetica", "", 8)
		f.SetTextColor(90, 90, 90)
		f.CellFormat(0, 10, fmt.Sprintf("Audit report  %s  page %d", a.Reference, f.PageNo()-firstAuditPage+1), "", 0, "L", false, 0, "")
		f.SetTextColor(0, 0, 0)
	})
	f.AddPageFormat("P", fpdf.SizeType{Wd: 595.28, Ht: 841.89})
	auditStarted, firstAuditPage = true, f.PageNo()
	f.SetFont("gotext", "B", 18)
	f.MultiCell(0, 22, "Audit report", "", "L", false)
	f.Ln(4)
	fact := func(label, value string) {
		y := f.GetY()
		f.SetFont("gotext", "B", 9.5)
		f.MultiCell(150, 13, label, "", "L", false)
		after := f.GetY()
		f.SetXY(margin+150, y)
		f.SetFont("gotext", "", 9.5)
		f.MultiCell(0, 13, value, "", "L", false)
		if f.GetY() < after {
			f.SetY(after)
		}
	}
	fact("Document", a.Name)
	fact("Reference", a.Reference)
	fact("Created by", a.SenderName+" ("+a.SenderEmail+")")
	fact("Created", formatUTC(a.CreatedAt))
	fact("Sent for signature", formatUTC(a.SentAt))
	fact("Completed", formatUTC(a.CompletedAt))
	fact("Status", "Signed electronically on RightProperty365")
	fact("Pages", fmt.Sprintf("%d, before this report", a.Pages))
	fact("Original document SHA-256", a.OriginalSHA256)
	fact("Final document SHA-256", "Recorded by RightProperty365 when this file was produced (a file cannot contain its own "+
		"fingerprint). Check a copy at "+a.VerifyURL+" with the reference above.")
	f.Ln(8)
	f.SetFont("gotext", "B", 12)
	f.MultiCell(0, 16, "History", "", "L", false)
	f.Ln(2)
	for _, l := range a.Lines {
		y := f.GetY()
		f.SetFont("gotext", "", 8.5)
		f.SetTextColor(80, 80, 80)
		f.MultiCell(150, 11.5, formatUTC(l.At), "", "L", false)
		after := f.GetY()
		f.SetXY(margin+150, y)
		f.SetTextColor(0, 0, 0)
		f.SetFont("gotext", "B", 9)
		f.MultiCell(0, 12, l.Text, "", "L", false)
		if l.Detail != "" {
			f.SetX(margin + 150)
			f.SetFont("gotext", "", 8)
			f.MultiCell(0, 10.5, l.Detail, "", "L", false)
		}
		if f.GetY() < after {
			f.SetY(after)
		}
		f.Ln(4)
	}
	f.Ln(6)
	f.SetFont("gotext", "", 9)
	f.MultiCell(0, 12, SealNote, "", "L", false)
	f.Ln(4)
	f.MultiCell(0, 12, Statement, "", "L", false)
	if err := f.Error(); err != nil {
		return nil, fmt.Errorf("render the completed document: %w", err)
	}
	var buf bytes.Buffer
	if err := f.Output(&buf); err != nil {
		return nil, fmt.Errorf("render the completed document: %w", err)
	}
	return buf.Bytes(), nil
}
