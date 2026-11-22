package tenancies

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/money"
)

// Statement formats.
const (
	FormatJSON = "json"
	FormatCSV  = "csv"
	FormatPDF  = "pdf"
)

var lineOrder = map[string]int{LineCharge: 0, LinePayment: 1, LinePaymentReversed: 2, LineWaiver: 3}

// rawLine is a statement line before the running balance.
type rawLine struct {
	StatementLine
	debit, credit *big.Rat
	key           string
}

// statementLines returns every line of a tenancy's history, in order.
func statementLines(t Tenancy, dues []Due, recs []Receipt) []rawLine {
	var out []rawLine
	add := func(l StatementLine, debit, credit *big.Rat, key string) {
		l.Debit, l.Credit = money.Format(debit, l.Currency), money.Format(credit, l.Currency)
		out = append(out, rawLine{StatementLine: l, debit: debit, credit: credit, key: key})
	}
	for _, d := range dues {
		id := d.ID
		add(StatementLine{Date: d.DueDate, Type: LineCharge, Description: fmt.Sprintf("Rent %s to %s", d.DueDate, PeriodEnd(t, d.Sequence, d.DueDate)),
			Currency: d.Currency, DueID: &id}, money.Rat(d.Amount), new(big.Rat), id.String())
		if d.WaivedAt != nil {
			if w := d.state(forever).waived; w.Sign() > 0 {
				add(StatementLine{Date: clockDay(*d.WaivedAt), Type: LineWaiver, Description: "Rent of " + d.DueDate + " waived", Currency: d.Currency,
					DueID: &id}, new(big.Rat), w, id.String())
			}
		}
	}
	for _, r := range recs {
		id := r.ID
		add(StatementLine{Date: r.ReceivedOn, Type: LinePayment, Description: "Payment (" + methodLabel(r.Method) + ")", Reference: r.Reference,
			Currency: r.Currency, ReceiptID: &id}, new(big.Rat), money.Rat(r.Amount), id.String())
		if r.Reversed() {
			add(StatementLine{Date: r.ReversedOn, Type: LinePaymentReversed, Description: "Payment of " + r.ReceivedOn + " reversed",
				Reference: r.Reference, Currency: r.Currency, ReceiptID: &id}, money.Rat(r.Amount), new(big.Rat), id.String())
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if lineOrder[a.Type] != lineOrder[b.Type] {
			return lineOrder[a.Type] < lineOrder[b.Type]
		}
		return a.key < b.key
	})
	return out
}

// buildStatement assembles a tenancy's statement from from to to (both
// inclusive), per currency.
func buildStatement(t Tenancy, st StatementTenancy, dues []Due, recs []Receipt, from, to string, now time.Time) Statement {
	out := Statement{Tenancy: st, From: from, To: to, GeneratedAt: now, Currencies: []StatementCurrency{}}
	lines := statementLines(t, dues, recs)
	var codes []string
	per := map[string][]rawLine{}
	for _, l := range lines {
		if _, ok := per[l.Currency]; !ok {
			codes = append(codes, l.Currency)
		}
		per[l.Currency] = append(per[l.Currency], l)
	}
	sort.Strings(codes)
	for _, cur := range codes {
		opening := new(big.Rat)
		charged, paid, reversed, waived := new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat)
		sc := StatementCurrency{Currency: cur, MinorUnit: money.MinorUnit(cur), Lines: []StatementLine{}}
		for _, l := range per[cur] {
			if l.Date < from {
				opening.Add(opening, l.debit)
				opening.Sub(opening, l.credit)
			}
		}
		balance := new(big.Rat).Set(opening)
		for _, l := range per[cur] {
			if l.Date < from || l.Date > to {
				continue
			}
			balance.Add(balance, l.debit)
			balance.Sub(balance, l.credit)
			switch l.Type {
			case LineCharge:
				charged.Add(charged, l.debit)
			case LinePayment:
				paid.Add(paid, l.credit)
			case LinePaymentReversed:
				reversed.Add(reversed, l.debit)
			case LineWaiver:
				waived.Add(waived, l.credit)
			}
			line := l.StatementLine
			line.Balance = money.Format(balance, cur)
			sc.Lines = append(sc.Lines, line)
		}
		overdue := new(big.Rat)
		for _, d := range dues {
			if d.Currency == cur && d.DueDate < to {
				overdue.Add(overdue, d.state(to).outstanding)
			}
		}
		sc.OpeningBalance, sc.Charged, sc.Paid, sc.Reversed, sc.Waived = money.Format(opening, cur), money.Format(charged, cur),
			money.Format(paid, cur), money.Format(reversed, cur), money.Format(waived, cur)
		sc.ClosingBalance, sc.Overdue = money.Format(balance, cur), money.Format(overdue, cur)
		out.Currencies = append(out.Currencies, sc)
	}
	return out
}

func (s *Service) statementTenancy(ctx context.Context, t Tenancy) (StatementTenancy, error) {
	v, err := s.viewer(ctx, t.Book, []Tenancy{t})
	if err != nil {
		return StatementTenancy{}, err
	}
	my, err := v.mine(ctx, t)
	if err != nil {
		return StatementTenancy{}, err
	}
	return StatementTenancy{ID: t.ID, Status: t.Status, StatusLabel: StatusLabel(t.Status), ManagedBy: my.ManagedBy,
		Property: StatementProperty{Name: my.Property.Name, Address: my.Property.Address}, Unit: my.Unit, Tenants: my.Tenants}, nil
}

// statementRange validates from and to (defaults: the start date, today).
func (s *Service) statementRange(ctx context.Context, t Tenancy, from, to string) (string, string, error) {
	f := map[string]string{}
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" {
		from = t.StartDate
	} else if !validDate(from) {
		f["from"] = "must be a date (YYYY-MM-DD)"
	}
	if to == "" {
		to = s.tenancyToday(ctx, t)
	} else if !validDate(to) {
		f["to"] = "must be a date (YYYY-MM-DD)"
	}
	if len(f) == 0 && to < from {
		f["to"] = "must be on or after from (" + from + ")"
	}
	return from, to, invalid(f)
}

func (s *Service) statementOf(ctx context.Context, t Tenancy, from, to string) (Statement, error) {
	from, to, err := s.statementRange(ctx, t, from, to)
	if err != nil {
		return Statement{}, err
	}
	dues, err := s.store.Dues(ctx, []uuid.UUID{t.ID})
	if err != nil {
		return Statement{}, err
	}
	recs, err := s.store.Receipts(ctx, t.ID)
	if err != nil {
		return Statement{}, err
	}
	st, err := s.statementTenancy(ctx, t)
	if err != nil {
		return Statement{}, err
	}
	return buildStatement(t, st, dues, recs, from, to, s.clock()), nil
}

// TenancyStatement serves GET .../tenancies/{tenancyId}/statement.
func (s *Service) TenancyStatement(ctx context.Context, p *auth.Principal, bookID, tenancyID uuid.UUID, from, to string) (Statement, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Statement{}, err
	}
	t, err := s.tenancy(ctx, a, tenancyID)
	if err != nil {
		return Statement{}, err
	}
	return s.statementOf(ctx, t, from, to)
}

// MyStatement serves GET /v1/me/tenancies/{id}/statement.
func (s *Service) MyStatement(ctx context.Context, p *auth.Principal, id uuid.UUID, from, to string) (Statement, error) {
	t, err := s.myTenancy(ctx, p, id)
	if err != nil {
		return Statement{}, err
	}
	return s.statementOf(ctx, t, from, to)
}

// StatementFileName is the download name of a statement.
func StatementFileName(st Statement, format string) string {
	return fmt.Sprintf("statement-%s-%s-%s.%s", st.Tenancy.ID.String()[:8], st.From, st.To, format)
}

// cell guards a text cell against spreadsheet formula injection.
func cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// StatementCSV renders a statement as CSV.
func StatementCSV(st Statement) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	rows := [][]string{{"currency", "row", "date", "type", "description", "reference", "debit", "credit", "balance"}}
	for _, c := range st.Currencies {
		rows = append(rows, []string{c.Currency, "OPENING", st.From, "", "Opening balance", "", "", "", c.OpeningBalance})
		for _, l := range c.Lines {
			rows = append(rows, []string{c.Currency, "LINE", l.Date, l.Type, cell(l.Description), cell(l.Reference), l.Debit, l.Credit, l.Balance})
		}
		rows = append(rows, []string{c.Currency, "CLOSING", st.To, "", "Closing balance", "", "", "", c.ClosingBalance})
	}
	if err := w.WriteAll(rows); err != nil {
		return nil, fmt.Errorf("statement csv: %w", err)
	}
	return buf.Bytes(), nil
}

const statementFont = "GoText"

// StatementPDF renders a statement with gofpdf and the Go fonts (as the
// leases): the manager, property, unit, tenants and period, then per
// currency the opening balance, the lines and the totals.
func StatementPDF(st Statement) ([]byte, error) {
	f := fpdf.New("P", "mm", "A4", "")
	f.SetCatalogSort(true)
	at := st.GeneratedAt.UTC()
	f.SetCreationDate(at)
	f.SetModificationDate(at)
	f.SetProducer("RightProperty365", false)
	f.SetCreator("RightProperty365", false)
	f.SetTitle("Tenant statement "+st.From+" to "+st.To, true)
	f.AddUTF8FontFromBytes(statementFont, "", goregular.TTF)
	f.AddUTF8FontFromBytes(statementFont, "B", gobold.TTF)
	f.SetMargins(15, 15, 15)
	f.SetAutoPageBreak(true, 18)
	f.SetFooterFunc(func() {
		f.SetY(-12)
		f.SetFont("Helvetica", "", 8)
		f.SetTextColor(110, 110, 110)
		f.CellFormat(0, 5, fmt.Sprintf("Statement %s to %s - page %d", st.From, st.To, f.PageNo()), "", 0, "R", false, 0, "")
		f.SetTextColor(0, 0, 0)
	})
	f.AddPage()
	f.SetFont(statementFont, "B", 16)
	f.CellFormat(0, 9, "Tenant statement", "", 1, "L", false, 0, "")
	f.SetFont(statementFont, "", 10)
	property := st.Tenancy.Property.Name
	if st.Tenancy.Unit != nil {
		property += ", " + st.Tenancy.Unit.Label
	}
	names := make([]string, 0, len(st.Tenancy.Tenants))
	for _, n := range st.Tenancy.Tenants {
		names = append(names, n.Name)
	}
	for _, row := range [][2]string{
		{"Managed by", st.Tenancy.ManagedBy.Name},
		{"Property", property},
		{"Address", st.Tenancy.Property.Address},
		{"Tenants", strings.Join(names, ", ")},
		{"Period", st.From + " to " + st.To},
		{"Generated", at.Format("2006-01-02 15:04 UTC")},
	} {
		f.SetFont(statementFont, "B", 10)
		f.CellFormat(32, 6, row[0], "", 0, "L", false, 0, "")
		f.SetFont(statementFont, "", 10)
		f.MultiCell(0, 6, row[1], "", "L", false)
	}
	if len(st.Currencies) == 0 {
		f.Ln(4)
		f.MultiCell(0, 6, "There is nothing on this tenancy's rent account yet.", "", "L", false)
	}
	widths := []float64{22, 74, 28, 28, 28}
	for _, c := range st.Currencies {
		f.Ln(5)
		f.SetFont(statementFont, "B", 12)
		f.CellFormat(0, 7, c.Currency, "", 1, "L", false, 0, "")
		f.SetFont(statementFont, "B", 9)
		f.SetFillColor(235, 235, 235)
		for i, h := range []string{"Date", "Description", "Debit", "Credit", "Balance"} {
			align := "R"
			if i < 2 {
				align = "L"
			}
			f.CellFormat(widths[i], 6, h, "B", 0, align, true, 0, "")
		}
		f.Ln(-1)
		f.SetFont(statementFont, "", 9)
		row := func(date, desc, debit, credit, balance string) {
			f.CellFormat(widths[0], 6, date, "", 0, "L", false, 0, "")
			f.CellFormat(widths[1], 6, truncate(f, desc, widths[1]), "", 0, "L", false, 0, "")
			f.CellFormat(widths[2], 6, debit, "", 0, "R", false, 0, "")
			f.CellFormat(widths[3], 6, credit, "", 0, "R", false, 0, "")
			f.CellFormat(widths[4], 6, balance, "", 1, "R", false, 0, "")
		}
		row(st.From, "Opening balance", "", "", c.OpeningBalance)
		for _, l := range c.Lines {
			desc := l.Description
			if l.Reference != "" {
				desc += " - " + l.Reference
			}
			row(l.Date, desc, blankZero(l.Debit), blankZero(l.Credit), l.Balance)
		}
		f.SetFont(statementFont, "B", 9)
		row(st.To, "Closing balance", "", "", c.ClosingBalance)
		f.Ln(2)
		f.SetFont(statementFont, "", 9)
		for _, t := range [][2]string{{"Charged", c.Charged}, {"Paid", c.Paid}, {"Reversed", c.Reversed}, {"Waived", c.Waived},
			{"Overdue on " + st.To, c.Overdue}} {
			f.CellFormat(widths[0]+widths[1]+widths[2]+widths[3], 5, t[0], "", 0, "R", false, 0, "")
			f.CellFormat(widths[4], 5, t[1]+" "+c.Currency, "", 1, "R", false, 0, "")
		}
	}
	if err := f.Error(); err != nil {
		return nil, fmt.Errorf("render statement: %w", err)
	}
	var buf bytes.Buffer
	if err := f.Output(&buf); err != nil {
		return nil, fmt.Errorf("render statement: %w", err)
	}
	return buf.Bytes(), nil
}

func blankZero(v string) string {
	if money.Rat(v).Sign() == 0 {
		return ""
	}
	return v
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
