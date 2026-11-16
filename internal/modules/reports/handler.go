package reports

import (
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Handler serves /v1/admin/reports/*.
type Handler struct {
	store  Store
	logger *slog.Logger
	now    func() time.Time
	// markets validates market codes (nil: any two letters).
	markets func(ctx context.Context) ([]string, error)
}

// NewHandler builds the handler.
func NewHandler(store Store, markets func(ctx context.Context) ([]string, error), now func() time.Time, logger *slog.Logger) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{store: store, logger: logger, now: now, markets: markets}
}

var marketRe = regexp.MustCompile(`^[A-Z]{2}$`)

const dayLayout = "2006-01-02"

// rangeOf reads from, to (inclusive days, UTC) and market (comma-separated
// country codes). Defaults: the last 30 days, every market.
func (h *Handler) rangeOf(r *http.Request) (Range, map[string]string) {
	q := r.URL.Query()
	fields := map[string]string{}
	today := h.now().UTC().Truncate(24 * time.Hour)
	to := today
	if raw := q.Get("to"); raw != "" {
		t, err := time.Parse(dayLayout, raw)
		if err != nil {
			fields["to"] = "must be a date (YYYY-MM-DD)"
		} else {
			to = t
		}
	}
	from := to.Add(-DefaultRange + 24*time.Hour)
	if raw := q.Get("from"); raw != "" {
		t, err := time.Parse(dayLayout, raw)
		if err != nil {
			fields["from"] = "must be a date (YYYY-MM-DD)"
		} else {
			from = t
		}
	}
	end := to.Add(24 * time.Hour)
	switch {
	case len(fields) > 0:
	case from.After(to):
		fields["from"] = "must not be after to"
	case end.Sub(from) > MaxRange:
		fields["from"] = "the range can be at most three years"
	}
	out := Range{From: from, To: end}
	if raw := strings.TrimSpace(q.Get("market")); raw != "" {
		var known []string
		if h.markets != nil {
			codes, err := h.markets(r.Context())
			if err == nil {
				known = codes
			}
		}
		for _, code := range strings.Split(raw, ",") {
			code = strings.ToUpper(strings.TrimSpace(code))
			ok := marketRe.MatchString(code)
			if ok && known != nil {
				ok = false
				for _, k := range known {
					ok = ok || k == code
				}
			}
			if !ok {
				fields["market"] = "must be known country codes, separated by commas"
				continue
			}
			out.Markets = append(out.Markets, code)
		}
	}
	return out, fields
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

type countView struct {
	Group      string  `json:"group"`
	MarketCode *string `json:"market_code"`
	Count      int     `json:"count"`
}

type sectionView struct {
	Total int         `json:"total"`
	Rows  []countView `json:"rows"`
}

func section(counts []Count) sectionView {
	s := sectionView{Rows: make([]countView, 0, len(counts))}
	for _, c := range counts {
		v := countView{Group: c.Group, Count: c.Count}
		if c.MarketCode != "" {
			code := c.MarketCode
			v.MarketCode = &code
		}
		s.Rows = append(s.Rows, v)
		s.Total += c.Count
	}
	return s
}

type amountView struct {
	CurrencyCode string  `json:"currency_code"`
	MarketCode   *string `json:"market_code"`
	Total        string  `json:"total"`
	Payments     int     `json:"payments"`
}

type summaryView struct {
	From        string   `json:"from"`
	To          string   `json:"to"`
	MarketCodes []string `json:"market_codes"`
	// NewUsers counts per account role (an account with several roles is
	// counted once per role; NONE for no role). Total is of rows, not people.
	NewUsers          sectionView  `json:"new_users"`
	Onboarding        sectionView  `json:"onboarding_applications"`
	Listings          sectionView  `json:"listings"`
	PropertyRequests  sectionView  `json:"property_requests"`
	BusinessEnquiries sectionView  `json:"business_enquiries"`
	Leases            sectionView  `json:"leases"`
	Agreements        sectionView  `json:"agreements"`
	Subscriptions     sectionView  `json:"subscriptions"`
	Revenue           []amountView `json:"revenue"`
}

// Summary handles GET /v1/admin/reports/summary.
func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	rng, fields := h.rangeOf(r)
	if len(fields) > 0 {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", fields)
		return
	}
	ctx := r.Context()
	out := summaryView{From: rng.From.Format(dayLayout), To: rng.To.Add(-24 * time.Hour).Format(dayLayout), MarketCodes: rng.Markets, Revenue: []amountView{}}
	for _, part := range []struct {
		into *sectionView
		read func(context.Context, Range) ([]Count, error)
	}{
		{&out.NewUsers, h.store.NewUsers}, {&out.Onboarding, h.store.Onboarding}, {&out.Listings, h.store.Listings},
		{&out.PropertyRequests, h.store.PropertyRequests}, {&out.BusinessEnquiries, h.store.BusinessEnquiries},
		{&out.Leases, h.store.Leases}, {&out.Agreements, h.store.Agreements}, {&out.Subscriptions, h.store.Subscriptions},
	} {
		counts, err := part.read(ctx, rng)
		if err != nil {
			httpx.WriteInternalError(w, r, h.logger, err)
			return
		}
		*part.into = section(counts)
	}
	amounts, err := h.store.Revenue(ctx, rng)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	for _, a := range amounts {
		v := amountView{CurrencyCode: a.CurrencyCode, Total: a.Total, Payments: a.Payments}
		if a.MarketCode != "" {
			code := a.MarketCode
			v.MarketCode = &code
		}
		out.Revenue = append(out.Revenue, v)
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// CSV exports
// ---------------------------------------------------------------------------

// cell neutralises spreadsheet formulas: a value that starts with = + - @
// (or a tab or carriage return) is prefixed with an apostrophe.
func cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func timeCell(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func idCell(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// export runs one CSV export: the limit, the headers once the total is
// known, then one line per row.
func (h *Handler) export(w http.ResponseWriter, r *http.Request, name string, header []string,
	run func(ctx context.Context, rng Range, limit int, start func(int), write func([]string) error) error) {
	if principal(w, r) == nil {
		return
	}
	rng, fields := h.rangeOf(r)
	limit := DefaultExportLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxExportLimit {
			fields["limit"] = fmt.Sprintf("must be a whole number between 1 and %d", MaxExportLimit)
		} else {
			limit = n
		}
	}
	if len(fields) > 0 {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", fields)
		return
	}
	cw := csv.NewWriter(w)
	started, written := false, 0
	start := func(total int) {
		started = true
		hd := w.Header()
		hd.Set("Content-Type", "text/csv; charset=utf-8")
		hd.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s-to-%s.csv"`, name, rng.From.Format(dayLayout),
			rng.To.Add(-24*time.Hour).Format(dayLayout)))
		hd.Set("X-Total-Count", strconv.Itoa(total))
		hd.Set("X-Export-Limit", strconv.Itoa(limit))
		hd.Set("X-Export-Truncated", strconv.FormatBool(total > limit))
		private(w)
		w.WriteHeader(http.StatusOK)
		_ = cw.Write(header)
	}
	err := run(r.Context(), rng, limit, start, func(record []string) error {
		for i := range record {
			record[i] = cell(record[i])
		}
		written++
		if err := cw.Write(record); err != nil {
			return err
		}
		if written%500 == 0 {
			cw.Flush()
		}
		return cw.Error()
	})
	if err != nil && !started {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	if err != nil {
		// The status is already sent: the file ends early, and the log says why.
		h.logger.ErrorContext(r.Context(), "csv export failed part way", slog.String("export", name), slog.String("error", err.Error()))
	}
	cw.Flush()
}

// Users handles GET /v1/admin/reports/users.csv.
func (h *Handler) Users(w http.ResponseWriter, r *http.Request) {
	h.export(w, r, "users", []string{"id", "name", "email", "status", "email_verified", "account_roles", "platform_roles", "market_codes",
		"created_at", "last_sign_in_at"},
		func(ctx context.Context, rng Range, limit int, start func(int), write func([]string) error) error {
			return h.store.ExportUsers(ctx, rng, limit, start, func(u UserRow) error {
				created := u.CreatedAt
				return write([]string{u.ID.String(), u.Name, u.Email, u.Status, strconv.FormatBool(u.EmailVerified), strings.Join(u.AccountRoles, " "),
					strings.Join(u.PlatformRoles, " "), strings.Join(u.MarketCodes, " "), timeCell(&created), timeCell(u.LastSignInAt)})
			})
		})
}

// Listings handles GET /v1/admin/reports/listings.csv.
func (h *Handler) Listings(w http.ResponseWriter, r *http.Request) {
	h.export(w, r, "listings", []string{"id", "reference", "title", "status", "market_code", "deal", "category", "property_type", "price",
		"currency_code", "organisation_id", "agent_user_id", "seller_user_id", "created_at", "published_at", "expires_at"},
		func(ctx context.Context, rng Range, limit int, start func(int), write func([]string) error) error {
			return h.store.ExportListings(ctx, rng, limit, start, func(l listings.ReportRow) error {
				created := l.CreatedAt
				return write([]string{l.ID.String(), l.Reference, l.Title, l.Status, l.MarketCode, l.Deal, l.Category, l.PropertyType, l.Price,
					l.Currency, idCell(l.OrganisationID), idCell(l.AgentUserID), idCell(l.SellerUserID), timeCell(&created), timeCell(l.PublishedAt),
					timeCell(l.ExpiresAt)})
			})
		})
}

// Subscriptions handles GET /v1/admin/reports/subscriptions.csv.
func (h *Handler) Subscriptions(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	h.export(w, r, "subscriptions", []string{"id", "invoice_number", "status", "plan_name", "period", "subscriber_kind", "subscriber_name",
		"market_code", "currency_code", "amount", "payment_amount", "payment_reference", "payment_received_on", "starts_at", "ends_at",
		"created_at", "refund_due"},
		func(ctx context.Context, rng Range, limit int, start func(int), write func([]string) error) error {
			return h.store.ExportSubscriptions(ctx, rng, limit, start, func(x subscriptions.Subscription) error {
				var payAmount, payRef, payOn string
				if x.Payment != nil {
					payAmount, payRef, payOn = x.Payment.Amount, x.Payment.Reference, x.Payment.ReceivedOn
				}
				created := x.CreatedAt
				return write([]string{x.ID.String(), x.InvoiceNumber, x.EffectiveStatus(now), x.PlanName, x.Period, x.SubscriberKind, x.SubscriberName,
					x.MarketCode, x.CurrencyCode, x.Amount, payAmount, payRef, payOn, timeCell(x.StartsAt), timeCell(x.EndsAt), timeCell(&created),
					strconv.FormatBool(x.RefundDue)})
			})
		})
}
