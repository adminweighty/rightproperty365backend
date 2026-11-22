package maintenance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/money"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// CostInput is the body of POST .../maintenance/{requestId}/costs.
type CostInput struct {
	Amount      *string `json:"amount"`
	Currency    string  `json:"currency"`
	Description string  `json:"description"`
	Method      *string `json:"method"`
	IncurredOn  *string `json:"incurred_on"`
}

// ledgerFields renames the ledger's validation fields for the cost body.
func ledgerFields(err error) error {
	var lv *ledger.ValidationError
	switch {
	case errors.As(err, &lv):
		out := map[string]string{}
		for k, v := range lv.Fields {
			out["ledger."+k] = v
		}
		return &ValidationError{Fields: out}
	case errors.Is(err, ledger.ErrForbidden):
		return ErrForbidden
	case errors.Is(err, ledger.ErrBookNotFound):
		return ErrBookNotFound
	case errors.Is(err, ledger.ErrUnauthenticated):
		return ErrUnauthenticated
	}
	return err
}

func amount(f map[string]string, field string, raw *string, cur string) string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		f[field] = "is required: a positive decimal string such as \"85.00\""
		return ""
	}
	r, problem := money.ParseAmount(*raw, cur)
	switch problem {
	case "":
		return money.Format(r, cur)
	case money.ProblemDecimals:
		if d := money.MinorUnit(cur); d == 0 {
			f[field] = "must be a whole number for " + cur + " (it has no minor unit)"
		} else {
			f[field] = fmt.Sprintf("must have at most %d decimals for %s", d, cur)
		}
	case money.ProblemZero:
		f[field] = "must be more than zero"
	default:
		f[field] = "must be a positive decimal string such as \"85.00\" (no sign, exponent or thousands separator)"
	}
	return ""
}

// AddCost serves POST .../maintenance/{requestId}/costs: the cost, its
// COST_ADDED item and its EXPENSE ledger entry in one transaction.
func (s *Service) AddCost(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in CostInput) (CostResult, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return CostResult{}, err
	}
	r, err := s.request(ctx, a, id)
	if err != nil {
		return CostResult{}, err
	}
	if s.ledger == nil {
		return CostResult{}, ErrNoLedger
	}
	f := map[string]string{}
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	curs, err := s.allowedCurrencies(ctx)
	if err != nil {
		return CostResult{}, err
	}
	amt := ""
	switch {
	case cur == "":
		f["currency"] = "is required"
	case !money.ValidCurrencyCode(cur) || !slices.Contains(curs, cur):
		f["currency"] = "must be an ISO 4217 currency code from reference data, such as USD"
	default:
		amt = amount(f, "amount", in.Amount, cur)
	}
	desc := text(f, "description", in.Description, 1, MaxCostText)
	method := ledger.MethodOther
	if in.Method != nil && strings.TrimSpace(*in.Method) != "" {
		method = strings.TrimSpace(*in.Method)
		if !slices.Contains(ledger.Methods(), method) {
			f["method"] = "must be CASH, BANK, MOBILE_MONEY or OTHER"
		}
	}
	today := s.requestToday(ctx, r)
	incurred := today
	if in.IncurredOn != nil && strings.TrimSpace(*in.IncurredOn) != "" {
		incurred = strings.TrimSpace(*in.IncurredOn)
		switch {
		case !validDate(incurred):
			f["incurred_on"] = "must be a date (YYYY-MM-DD)"
		case incurred > today:
			f["incurred_on"] = "may not be in the future"
		}
	}
	if err := invalid(f); err != nil {
		return CostResult{}, err
	}
	props, err := s.tenancies.Properties(ctx, []uuid.UUID{r.PropertyID})
	if err != nil {
		return CostResult{}, err
	}
	listing, err := s.tenancies.PropertyListing(ctx, props[r.PropertyID])
	if err != nil {
		return CostResult{}, err
	}
	ledgerDesc := "Maintenance " + r.Reference + ": " + desc
	if n := []rune(ledgerDesc); len(n) > ledger.MaxDescription {
		ledgerDesc = string(n[:ledger.MaxDescription])
	}
	lch, entry, err := s.ledger.PrepareMaintenanceCost(ctx, p, bookID, ledger.MaintenanceCost{Amount: amt, Currency: cur, Method: method,
		EffectiveDate: incurred, Reference: r.Reference, Description: ledgerDesc, PropertyID: r.PropertyID, ListingID: listing})
	if err != nil {
		return CostResult{}, ledgerFields(err)
	}
	now := s.clock()
	by := p.UserID
	cost := Cost{ID: newID(), RequestID: r.ID, Amount: amt, Currency: cur, Description: desc, Method: method, IncurredOn: incurred,
		LedgerEntryID: entry.ID, RecordedBy: &by, RecordedByName: p.Name, RecordedAt: now}
	ev := managerActor(p).event(r.ID, EventCostAdded, now)
	ev.Details = map[string]any{"cost_id": cost.ID.String(), "amount": amt, "currency": cur, "description": desc}
	ch := Change{Ledger: &lch, Touch: map[uuid.UUID]time.Time{r.ID: now}, Costs: []Cost{cost}, Events: []Event{ev},
		Audit: s.audit(ctx, a, orgaudit.ActionMaintenanceCostRecorded, r, map[string]any{"cost_id": cost.ID.String(), "amount": amt,
			"currency": cur, "ledger_entry_id": entry.ID.String()})}
	if err := s.store.Apply(ctx, ch); err != nil {
		return CostResult{}, err
	}
	view, err := s.oneView(ctx, a, id)
	if err != nil {
		return CostResult{}, err
	}
	out := CostResult{Request: view}
	for _, c := range view.Costs {
		if c.ID == cost.ID {
			out.Cost = c
		}
	}
	return out, nil
}
