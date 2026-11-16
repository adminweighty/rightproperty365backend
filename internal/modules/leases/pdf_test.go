package leases

import (
	"bytes"
	"compress/zlib"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
)

// PDFText returns the decompressed content streams of a PDF, where the
// text set in the core fonts (reference, page numbers, the watermark) is
// readable as "(...) Tj".
func PDFText(t *testing.T, content []byte) string {
	t.Helper()
	var out strings.Builder
	re := regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	for _, m := range re.FindAllSubmatch(content, -1) {
		r, err := zlib.NewReader(bytes.NewReader(m[1]))
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(r)
		out.Write(raw)
	}
	return out.String()
}

func sampleSnapshot() Snapshot {
	return Snapshot{
		Reference: "RPL-2026-000123", AgreementDate: "2026-11-14", GeneratedAt: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC),
		CountryName: "Zimbabwe", OrganisationName: "Sample Estates", PreparedBy: "Tendai Ncube",
		Template: SnapshotTemplate{ID: "0195c0de-0000-7000-8000-00000000e301", Version: 2, Name: "Résidential lease", LeaseKind: KindResidential,
			MarketCode: "ZW", Sections: []Section{
				{Heading: "Parties", Text: "Between {{landlord_names}} and {{tenant_names}} for {{property_address}}."},
				{Heading: "Rent", Text: strings.Repeat("The tenant pays {{rent_currency}} {{rent_amount}} per {{rent_period}} on {{payment_day}}. ", 60)},
			}, Clauses: []Clause{{Code: "NO_SMOKING", Title: "No smoking", Text: "Nobody smokes inside."}}},
		Input: Input{PropertyAddress: "12 Sample Road, Avondale, Harare", StartDate: "2026-12-01", EndDate: "2027-11-30",
			Landlords:  []Party{{Name: "Zoë Müller-Śmiałek", Email: "zoe@example.test"}},
			Tenants:    []Party{{Name: "François Ñúñez", Email: "francois@example.test"}, {Name: "Tariro Tenant", Email: "tenant@example.test"}},
			RentAmount: "1250.5", RentCurrency: "USD", RentPeriod: PeriodMonthly, PaymentDay: 1, NoticePeriodDays: 30, OptionalClauses: []string{"NO_SMOKING"}},
	}
}

func TestRenderPDF(t *testing.T) {
	snap := sampleSnapshot()
	draft, layout, err := RenderPDF(snap, ModeDraft)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(draft, []byte("%PDF-")) {
		t.Fatal("not a PDF")
	}
	text := PDFText(t, draft)
	pages := len(layout.Pages)
	if pages < 2 {
		t.Fatalf("expected a document of several pages, got %d", pages)
	}
	if n := strings.Count(text, "(DRAFT) Tj"); n != pages {
		t.Fatalf("the DRAFT watermark is on %d of %d pages", n, pages)
	}
	if !strings.Contains(text, "(RPL-2026-000123)") || !strings.Contains(text, "(Page 1 of ") {
		t.Fatal("the footer must carry the reference and page x of y")
	}

	signing, signLayout, err := RenderPDF(snap, ModeSigning)
	if err != nil {
		t.Fatal(err)
	}
	if text := PDFText(t, signing); strings.Contains(text, "(DRAFT)") || !strings.Contains(text, "(RPL-2026-000123)") {
		t.Fatal("the signing copy has no watermark and keeps the reference")
	}
	again, _, _ := RenderPDF(snap, ModeSigning)
	if Checksum(again) != Checksum(signing) {
		t.Fatal("rendering must be deterministic")
	}
	// Every party initials every page and has a signature block.
	parties := 3
	count := map[string]int{}
	for _, f := range signLayout.Fields {
		count[f.Type]++
		page := signLayout.Pages[f.Page-1]
		if f.X < 0 || f.Y < 0 || f.X+f.W > page.Width || f.Y+f.H > page.Height {
			t.Fatalf("field off the page: %+v", f)
		}
	}
	if count[FieldInitials] != parties*len(signLayout.Pages) || count[FieldSignature] != parties || count[FieldName] != parties || count[FieldDateSigned] != parties {
		t.Fatalf("fields = %v for %d pages", count, len(signLayout.Pages))
	}
	if p := signLayout.Pages[0]; p.Width != 595.28 || p.Height != 841.89 {
		t.Fatalf("A4 in points, got %+v", p)
	}
}

func TestPlaceholdersAndValues(t *testing.T) {
	unknown, malformed := UnknownPlaceholders("Rent {{rent_amount}} {{ tenant_names }} {{landlord_pet}} {{")
	if len(unknown) != 1 || unknown[0] != "landlord_pet" || !malformed {
		t.Fatalf("unknown=%v malformed=%v", unknown, malformed)
	}
	v := Values(sampleSnapshot().Input, "RPL-1", "Zimbabwe", KindResidential, "2026-11-14")
	if v["rent_amount"] != "1,250.50" || v["tenant_names"] != "François Ñúñez and Tariro Tenant" || v["payment_day"] != "the 1st day of each month" ||
		v["deposit_amount"] != "no deposit" || v["term"] != "from 1 December 2026 to 30 November 2027" {
		t.Fatalf("values = %v", v)
	}
	if FormatAmount("1234567") != "1,234,567.00" || PaymentDayText(PeriodWeekly, 5) != "Friday of each week" {
		t.Fatal("formatting")
	}
	if Fill("{{reference}} / {{unknown}}", v) != "RPL-1 / {{unknown}}" {
		t.Fatal("fill")
	}
}

// A lease asking for the landlords' stamps leaves a 40 mm STAMP field next
// to each landlord's signature block, inside the page, not over the name.
func TestRenderPDFLandlordStamp(t *testing.T) {
	snap := sampleSnapshot()
	_, plain, err := RenderPDF(snap, ModeSigning)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range plain.Fields {
		if f.Type == FieldStamp {
			t.Fatal("no stamp unless the lease asks for one")
		}
	}
	snap.Input.LandlordStamp = true
	snap.Input.Landlords = append(snap.Input.Landlords, Party{Name: "Second Landlord", Email: "second@example.test"})
	_, layout, err := RenderPDF(snap, ModeSigning)
	if err != nil {
		t.Fatal(err)
	}
	var stamps []PlacedField
	names := map[int]PlacedField{}
	for _, f := range layout.Fields {
		switch f.Type {
		case FieldStamp:
			stamps = append(stamps, f)
		case FieldName:
			if f.Role == RoleLandlord {
				names[f.Position] = f
			}
		}
	}
	if len(stamps) != 2 {
		t.Fatalf("%d STAMP fields, want one per landlord", len(stamps))
	}
	for _, s := range stamps {
		page := layout.Pages[s.Page-1]
		name := names[s.Position]
		if s.Role != RoleLandlord || s.W < 113 || s.H < 113 || s.X+s.W > page.Width || s.Y+s.H > page.Height ||
			(name.Page == s.Page && name.X+name.W > s.X) {
			t.Fatalf("stamp %+v (name %+v, page %+v)", s, name, page)
		}
	}
}
