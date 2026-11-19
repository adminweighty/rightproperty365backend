package inspections

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"golang.org/x/image/draw"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"

	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/auth"
)

// Report is everything the PDF is drawn from.
type Report struct {
	Inspection Inspection
	ManagedBy  string
	Property   string
	Address    string
	Unit       string
	Tenants    []string
	// Baseline is the move-in a move-out is compared with (nil otherwise).
	Baseline *Inspection
	// PhotoCounts is the number of photos per item.
	PhotoCounts map[uuid.UUID]int
	// Thumbnails are the embedded photos (JPEG) per item, in order, at most
	// MaxReportThumbnails in all.
	Thumbnails  map[uuid.UUID][][]byte
	GeneratedAt time.Time
}

// ReportFileName is the download name of the report.
func ReportFileName(in Inspection) string {
	return "inspection-" + in.Reference + ".pdf"
}

const reportFont = "GoText"

func utc(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// RenderReport draws the report with gofpdf and the Go fonts (as the leases
// and the rent statements): the header, the property and unit, the type,
// the dates, the inspector, one table per area with its photo thumbnails,
// the summary, the sign-off and a footer with the reference.
func RenderReport(r Report) ([]byte, error) {
	in := r.Inspection
	f := fpdf.New("P", "mm", "A4", "")
	f.SetCatalogSort(true)
	at := r.GeneratedAt.UTC()
	f.SetCreationDate(at)
	f.SetModificationDate(at)
	f.SetProducer("RightProperty365", false)
	f.SetCreator("RightProperty365", false)
	f.SetTitle("Inspection report "+in.Reference, true)
	f.AddUTF8FontFromBytes(reportFont, "", goregular.TTF)
	f.AddUTF8FontFromBytes(reportFont, "B", gobold.TTF)
	f.SetMargins(15, 15, 15)
	f.SetAutoPageBreak(true, 18)
	f.SetFooterFunc(func() {
		f.SetY(-12)
		f.SetFont(reportFont, "", 8)
		f.SetTextColor(110, 110, 110)
		f.CellFormat(0, 5, fmt.Sprintf("%s - generated %s - page %d", in.Reference, at.Format("2006-01-02 15:04 UTC"), f.PageNo()),
			"", 0, "R", false, 0, "")
		f.SetTextColor(0, 0, 0)
	})
	f.AddPage()
	_, pageH := f.GetPageSize()
	bottom := pageH - 18
	ensure := func(h float64) bool {
		if f.GetY()+h > bottom {
			f.AddPage()
			return true
		}
		return false
	}

	f.SetFont(reportFont, "B", 16)
	f.CellFormat(0, 9, "Inspection report", "", 1, "L", false, 0, "")
	f.SetFont(reportFont, "B", 11)
	title := in.Reference + " - " + TypeLabel(in.Type)
	switch in.Status {
	case StatusCompleted:
	case StatusCancelled:
		title += " (Cancelled)"
	default:
		title += " (Draft: " + strings.ToLower(StatusLabel(in.Status)) + ")"
	}
	f.CellFormat(0, 7, title, "", 1, "L", false, 0, "")
	f.Ln(2)
	rows := [][2]string{
		{"Managed by", r.ManagedBy},
		{"Property", r.Property},
		{"Address", r.Address},
	}
	if r.Unit != "" {
		rows = append(rows, [2]string{"Unit", r.Unit})
	}
	if len(r.Tenants) > 0 {
		rows = append(rows, [2]string{"Tenants", strings.Join(r.Tenants, ", ")})
	}
	rows = append(rows,
		[2]string{"Type", TypeLabel(in.Type)},
		[2]string{"Scheduled", utc(&in.ScheduledFor)},
		[2]string{"Started", utc(in.StartedAt)},
		[2]string{"Completed", utc(in.CompletedAt)},
		[2]string{"Inspector", in.InspectorName},
		[2]string{"Checklist", in.TemplateLabel},
	)
	if r.Baseline != nil {
		rows = append(rows, [2]string{"Compared with", r.Baseline.Reference + " (move-in, completed " + utc(r.Baseline.CompletedAt) + ")"})
	}
	if in.Status == StatusCancelled && in.CancelReason != "" {
		rows = append(rows, [2]string{"Cancelled", utc(in.CancelledAt) + ": " + in.CancelReason})
	}
	for _, row := range rows {
		f.SetFont(reportFont, "B", 10)
		f.CellFormat(34, 6, row[0], "", 0, "L", false, 0, "")
		f.SetFont(reportFont, "", 10)
		v := row[1]
		if v == "" {
			v = "-"
		}
		f.MultiCell(0, 6, v, "", "L", false)
	}
	pr := in.Checklist.Progress()
	f.Ln(2)
	f.SetFont(reportFont, "", 10)
	f.MultiCell(0, 6, fmt.Sprintf("%d items: %d OK, %d need attention, %d damaged, %d not applicable, %d without a finding.",
		pr.Items, pr.OK, pr.Attention, pr.Damaged, pr.NA, pr.Items-pr.Done), "", "L", false)

	var baselines map[uuid.UUID]Item
	compare := r.Baseline != nil
	if compare {
		baselines = Baselines(in.Checklist, r.Baseline.Checklist)
	}
	heads := []string{"Item", "Status", "Note", "Photos"}
	widths := []float64{55, 30, 80, 15}
	if compare {
		heads = []string{"Item", "Status", "Move-in", "Note", "Photos"}
		widths = []float64{46, 26, 26, 68, 14}
	}
	noteCol := len(widths) - 2
	header := func() {
		f.SetFont(reportFont, "B", 9)
		f.SetFillColor(235, 235, 235)
		for i, h := range heads {
			align := "L"
			if i == len(heads)-1 {
				align = "R"
			}
			f.CellFormat(widths[i], 6, h, "B", 0, align, true, 0, "")
		}
		f.Ln(-1)
		f.SetFont(reportFont, "", 9)
	}
	statusText := func(s string) string {
		if s == "" {
			return "-"
		}
		return FindingLabel(s)
	}
	left, _, _, _ := f.GetMargins()
	for _, a := range in.Checklist.Areas {
		ensure(20)
		f.Ln(4)
		f.SetFont(reportFont, "B", 12)
		f.CellFormat(0, 7, a.Label, "", 1, "L", false, 0, "")
		header()
		for _, it := range a.Items {
			f.SetFont(reportFont, "", 9)
			note := strings.ReplaceAll(it.Note, "\r", "")
			var lines []string
			if note != "" {
				lines = f.SplitText(note, widths[noteCol]-2)
			}
			h := float64(max(1, len(lines))) * 5
			if ensure(h) {
				header()
			}
			y := f.GetY()
			cells := []string{truncate(f, it.Label, widths[0]), statusText(it.Status)}
			if compare {
				b, ok := baselines[it.ID]
				if ok {
					cells = append(cells, statusText(b.Status))
				} else {
					cells = append(cells, "-")
				}
			}
			cells = append(cells, "")
			count := ""
			if n := r.PhotoCounts[it.ID]; n > 0 {
				count = fmt.Sprintf("%d", n)
			}
			cells = append(cells, count)
			x := left
			for i, c := range cells {
				align := "L"
				if i == len(cells)-1 {
					align = "R"
				}
				f.SetXY(x, y)
				f.CellFormat(widths[i], h, c, "B", 0, align, false, 0, "")
				x += widths[i]
			}
			if note != "" {
				nx := left
				for i := 0; i < noteCol; i++ {
					nx += widths[i]
				}
				f.SetXY(nx, y)
				f.MultiCell(widths[noteCol], 5, note, "", "L", false)
			}
			f.SetXY(left, y+h)
		}
		// The area's photos.
		var thumbs []struct {
			label string
			data  []byte
		}
		for _, it := range a.Items {
			for _, d := range r.Thumbnails[it.ID] {
				thumbs = append(thumbs, struct {
					label string
					data  []byte
				}{it.Label, d})
			}
		}
		const tw, th, gap, perRow = 32.0, 24.0, 5.0, 5
		for i, t := range thumbs {
			if i%perRow == 0 {
				if i == 0 {
					f.Ln(2)
				}
				ensure(th + 7)
			}
			x := left + float64(i%perRow)*(tw+gap)
			y := f.GetY()
			name := fmt.Sprintf("thumb-%s-%d", a.ID, i)
			info := f.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: "JPG"}, bytes.NewReader(t.data))
			if info != nil && info.Width() > 0 && info.Height() > 0 {
				w, hh := tw, tw*info.Height()/info.Width()
				if hh > th {
					hh, w = th, th*info.Width()/info.Height()
				}
				f.ImageOptions(name, x+(tw-w)/2, y, w, hh, false, fpdf.ImageOptions{ImageType: "JPG"}, 0, "")
			}
			f.SetFont(reportFont, "", 7)
			f.SetXY(x, y+th+0.5)
			f.CellFormat(tw, 4, truncate(f, t.label, tw), "", 0, "C", false, 0, "")
			if i%perRow == perRow-1 || i == len(thumbs)-1 {
				f.SetXY(left, y+th+6)
			} else {
				f.SetXY(x+tw+gap, y)
			}
		}
	}

	ensure(30)
	f.Ln(6)
	f.SetFont(reportFont, "B", 12)
	f.CellFormat(0, 7, "Summary", "", 1, "L", false, 0, "")
	f.SetFont(reportFont, "", 10)
	summary := in.Summary
	if summary == "" {
		summary = "No summary."
	}
	f.MultiCell(0, 5.5, summary, "", "L", false)
	ensure(30)
	f.Ln(4)
	f.SetFont(reportFont, "B", 12)
	f.CellFormat(0, 7, "Sign-off", "", 1, "L", false, 0, "")
	for _, row := range [][2]string{{"Tenant", in.TenantSignOff}, {"Agent", in.AgentSignOff}, {"Completed", utc(in.CompletedAt)}} {
		f.SetFont(reportFont, "B", 10)
		f.CellFormat(34, 6, row[0], "", 0, "L", false, 0, "")
		f.SetFont(reportFont, "", 10)
		v := row[1]
		if v == "" {
			v = "-"
		}
		f.MultiCell(0, 6, v, "", "L", false)
	}
	f.Ln(2)
	f.SetFont(reportFont, "", 8)
	f.SetTextColor(110, 110, 110)
	f.MultiCell(0, 4.5, "The sign-off names were typed when the inspection was completed; they are not signatures.", "", "L", false)
	f.SetTextColor(0, 0, 0)

	if err := f.Error(); err != nil {
		return nil, fmt.Errorf("render inspection report: %w", err)
	}
	var buf bytes.Buffer
	if err := f.Output(&buf); err != nil {
		return nil, fmt.Errorf("render inspection report: %w", err)
	}
	return buf.Bytes(), nil
}

// truncate shortens text to fit a cell width.
func truncate(f *fpdf.Fpdf, s string, width float64) string {
	if f.GetStringWidth(s) <= width-1 {
		return s
	}
	r := []rune(s)
	for len(r) > 1 && f.GetStringWidth(string(r)+"...") > width-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "..."
}

// thumbnail turns a stored JPEG into a small one for the PDF.
func thumbnail(rc io.Reader) ([]byte, error) {
	src, err := jpeg.Decode(rc)
	if err != nil {
		return nil, err
	}
	const side = 360
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if longer := max(w, h); longer > side {
		w, h = max(1, w*side/longer), max(1, h*side/longer)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 70}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// report gathers what the PDF needs (the thumbnails read from the storage;
// a photo that cannot be read is only counted).
func (s *Service) report(ctx context.Context, in Inspection, managedBy string) ([]byte, error) {
	props, err := s.tenancies.Properties(ctx, []uuid.UUID{in.PropertyID})
	if err != nil {
		return nil, err
	}
	prop := props[in.PropertyID]
	r := Report{Inspection: in, ManagedBy: managedBy, Property: prop.Name, Address: prop.Address, PhotoCounts: map[uuid.UUID]int{},
		Thumbnails: map[uuid.UUID][][]byte{}, GeneratedAt: s.clock()}
	if in.UnitID != nil {
		u, _ := prop.Unit(*in.UnitID)
		r.Unit = u.Label
	}
	if in.TenancyID != nil {
		t, err := s.tenancies.TenancyByID(ctx, *in.TenancyID)
		if err == nil && t.Book.Same(in.Book) {
			for _, party := range t.Parties {
				if party.Role == tenancies.RoleTenant {
					r.Tenants = append(r.Tenants, party.Name)
				}
			}
		} else if err != nil && err != tenancies.ErrTenancyNotFound {
			return nil, err
		}
	}
	if r.Baseline, err = s.baseline(ctx, in); err != nil {
		return nil, err
	}
	photos, err := s.store.Photos(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	embedded := 0
	for _, id := range in.Checklist.ItemIDs() {
		for _, p := range photos {
			if p.ItemID != id {
				continue
			}
			r.PhotoCounts[id]++
			if embedded >= MaxReportThumbnails || s.files == nil {
				continue
			}
			rc, err := s.files.Open(ctx, p.ObjectKey)
			if err != nil {
				s.logger.WarnContext(ctx, "an inspection photo could not be read for the report", "error", err.Error())
				continue
			}
			thumb, err := thumbnail(rc)
			_ = rc.Close()
			if err != nil {
				s.logger.WarnContext(ctx, "an inspection photo could not be shrunk for the report", "error", err.Error())
				continue
			}
			r.Thumbnails[id] = append(r.Thumbnails[id], thumb)
			embedded++
		}
	}
	return RenderReport(r)
}

// Report serves GET .../inspections/{inspectionId}/report.pdf.
func (s *Service) Report(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) ([]byte, string, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return nil, "", err
	}
	in, err := s.inspection(ctx, a, id)
	if err != nil {
		return nil, "", err
	}
	pdf, err := s.report(ctx, in, a.name)
	if err != nil {
		return nil, "", err
	}
	return pdf, ReportFileName(in), nil
}
