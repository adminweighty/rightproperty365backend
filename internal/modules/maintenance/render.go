package maintenance

import (
	"context"
	"errors"
	"math/big"
	"slices"
	"sort"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/money"
)

func minorUnit(cur string) int { return money.MinorUnit(cur) }

// viewer holds what the views of a set of requests need.
type viewer struct {
	s          *Service
	a          access
	vocab      vocab
	properties map[uuid.UUID]tenancies.Property
	costs      map[uuid.UUID][]Cost
	entries    map[uuid.UUID]*ledger.Entry
}

func (s *Service) viewer(ctx context.Context, a access, list []Request) (*viewer, error) {
	v := &viewer{s: s, a: a, entries: map[uuid.UUID]*ledger.Entry{}, costs: map[uuid.UUID][]Cost{}}
	var err error
	if v.vocab, err = s.vocab(ctx); err != nil {
		return nil, err
	}
	var props, ids []uuid.UUID
	for _, r := range list {
		ids = append(ids, r.ID)
		if !slices.Contains(props, r.PropertyID) {
			props = append(props, r.PropertyID)
		}
	}
	if v.properties, err = s.tenancies.Properties(ctx, props); err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		costs, err := s.store.Costs(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, c := range costs {
			v.costs[c.RequestID] = append(v.costs[c.RequestID], c)
			if s.ledgerEntry == nil {
				continue
			}
			e, err := s.ledgerEntry(ctx, c.LedgerEntryID)
			switch {
			case errors.Is(err, ledger.ErrEntryNotFound):
			case err != nil:
				return nil, err
			default:
				v.entries[c.LedgerEntryID] = &e
			}
		}
	}
	return v, nil
}

func (v *viewer) reversed(c Cost) bool {
	e := v.entries[c.LedgerEntryID]
	return e != nil && e.ReversedBy != nil
}

func costTotals(costs []Cost, reversed func(Cost) bool) []CostTotal {
	sums := map[string]*big.Rat{}
	counts := map[string]int{}
	for _, c := range costs {
		if reversed(c) {
			continue
		}
		if sums[c.Currency] == nil {
			sums[c.Currency] = new(big.Rat)
		}
		sums[c.Currency].Add(sums[c.Currency], money.Rat(c.Amount))
		counts[c.Currency]++
	}
	out := []CostTotal{}
	for cur, sum := range sums {
		out = append(out, CostTotal{Currency: cur, MinorUnit: money.MinorUnit(cur), Amount: money.Format(sum, cur), Count: counts[cur]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

func actorView(kind string, id *uuid.UUID, name string) ActorView {
	return ActorView{Kind: kind, UserID: id, Name: name}
}

func providerView(p *Provider) *ProviderView {
	if p == nil {
		return nil
	}
	return &ProviderView{ID: p.ProfileID, Kind: p.Kind, Slug: p.Slug, Name: p.Name}
}

func unitRef(prop tenancies.Property, id *uuid.UUID) *UnitRef {
	if id == nil {
		return nil
	}
	u, _ := prop.Unit(*id)
	return &UnitRef{ID: *id, Label: u.Label, Archived: u.DeletedAt != nil}
}

func (v *viewer) summary(r Request) Summary {
	prop := v.properties[r.PropertyID]
	return Summary{ID: r.ID, Reference: r.Reference, Status: r.Status, StatusLabel: StatusLabel(r.Status),
		Category: label(v.vocab.categories, r.Category, r.CategoryLabel), Priority: label(v.vocab.priorities, r.Priority, r.PriorityLabel),
		Title: r.Title, Property: SummaryProperty{ID: r.PropertyID, Name: prop.Name, Archived: prop.DeletedAt != nil},
		Unit: unitRef(prop, r.UnitID), TenancyID: r.TenancyID, RaisedBy: actorView(r.RaisedByKind, r.RaisedBy, r.RaisedByName),
		Provider: providerView(r.Provider), PhotoCount: r.PhotoCount, CommentCount: r.CommentCount,
		CostTotals: costTotals(v.costs[r.ID], v.reversed), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, StatusChangedAt: r.StatusChangedAt}
}

func timelineItem(e Event) TimelineItem {
	return TimelineItem{ID: e.ID, Type: e.Type, At: e.At, Actor: actorView(e.ActorKind, e.ActorUserID, e.ActorName),
		FromStatus: strPtr(e.FromStatus), ToStatus: strPtr(e.ToStatus), Text: strPtr(e.Text), Internal: e.Internal, Details: e.Details}
}

func (v *viewer) costView(c Cost) CostView {
	out := CostView{ID: c.ID, Amount: c.Amount, Currency: c.Currency, MinorUnit: money.MinorUnit(c.Currency), Description: c.Description,
		Method: c.Method, MethodLabel: ledger.MethodLabel(c.Method), IncurredOn: c.IncurredOn, LedgerEntryID: c.LedgerEntryID,
		LedgerStatus: ledger.StatusUnverified, LedgerStatusLabel: ledger.StatusLabel(ledger.StatusUnverified),
		RecordedBy: tenancies.PersonRef{UserID: c.RecordedBy, Name: c.RecordedByName}, RecordedAt: c.RecordedAt}
	if e := v.entries[c.LedgerEntryID]; e != nil {
		out.LedgerStatus, out.LedgerStatusLabel, out.Reversed = e.Status, ledger.StatusLabel(e.Status), e.ReversedBy != nil
	}
	return out
}

// PhotoPath is the API path of a photo for the managers.
func PhotoPath(bookID, requestID, photoID uuid.UUID) string {
	return "/v1/agent/organisations/" + bookID.String() + "/maintenance/" + requestID.String() + "/photos/" + photoID.String()
}

// MyPhotoPath is the API path of a photo for the tenant.
func MyPhotoPath(tenancyID, requestID, photoID uuid.UUID) string {
	return "/v1/me/tenancies/" + tenancyID.String() + "/maintenance/" + requestID.String() + "/photos/" + photoID.String()
}

func (v *viewer) photoView(r Request, p Photo) PhotoView {
	return PhotoView{ID: p.ID, URL: PhotoPath(v.a.book.ID(), r.ID, p.ID), ContentType: p.ContentType, Width: p.Width, Height: p.Height,
		Size: p.Size, UploadedBy: actorView(p.UploaderKind, p.UploadedBy, p.UploadedByName), UploadedAt: p.UploadedAt,
		Can: PhotoCan{Delete: v.a.write && Open(r.Status)}}
}

// view builds the full request (its photos, costs and timeline read here).
func (v *viewer) view(ctx context.Context, r Request) (RequestView, error) {
	prop := v.properties[r.PropertyID]
	out := RequestView{Summary: v.summary(r), Description: r.Description, Photos: []PhotoView{}, Costs: []CostView{},
		Timeline: []TimelineItem{}, ResolvedAt: r.ResolvedAt, ClosedAt: r.ClosedAt}
	out.Property = RequestProperty{ID: r.PropertyID, Name: prop.Name, Address: prop.Address, MarketCode: prop.MarketCode,
		Archived: prop.DeletedAt != nil}
	if r.TenancyID != nil {
		briefs, err := v.s.tenancies.Briefs(ctx, v.a.book, []uuid.UUID{*r.TenancyID})
		if err != nil {
			return RequestView{}, err
		}
		if b, ok := briefs[*r.TenancyID]; ok {
			out.Tenancy = &b
		}
	}
	if r.Provider != nil && r.AssignedAt != nil {
		as := &AssignmentView{Provider: *providerView(r.Provider), AssignedAt: *r.AssignedAt,
			AssignedBy: tenancies.PersonRef{UserID: r.AssignedBy, Name: r.AssignedByName}}
		if r.EnquiryID != nil {
			as.Enquiry = &EnquiryRef{ID: *r.EnquiryID, Reference: r.EnquiryReference}
		}
		out.Assignment = as
	}
	photos, err := v.s.store.Photos(ctx, r.ID)
	if err != nil {
		return RequestView{}, err
	}
	for _, p := range photos {
		out.Photos = append(out.Photos, v.photoView(r, p))
	}
	for _, c := range v.costs[r.ID] {
		out.Costs = append(out.Costs, v.costView(c))
	}
	events, err := v.s.store.Events(ctx, r.ID)
	if err != nil {
		return RequestView{}, err
	}
	for _, e := range events {
		out.Timeline = append(out.Timeline, timelineItem(e))
	}
	out.Can = RequestCan{Transitions: []string{}}
	if v.a.write {
		out.Can = RequestCan{Edit: Open(r.Status), Transitions: Transitions(r.Status), Assign: Assignable(r.Status), AddCost: true,
			Comment: true, AddPhoto: Open(r.Status)}
	}
	return out, nil
}

func (s *Service) oneView(ctx context.Context, a access, id uuid.UUID) (RequestView, error) {
	r, err := s.request(ctx, a, id)
	if err != nil {
		return RequestView{}, err
	}
	v, err := s.viewer(ctx, a, []Request{r})
	if err != nil {
		return RequestView{}, err
	}
	return v.view(ctx, r)
}
