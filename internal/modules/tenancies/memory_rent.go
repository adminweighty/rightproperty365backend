package tenancies

import (
	"context"
	"errors"
	"slices"
	"sort"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/platform/money"
)

// LedgerMemory is what the memory store needs of the ledger's memory store.
type LedgerMemory interface {
	ApplyWith(ctx context.Context, ch ledger.Change, with func() error) error
	Entry(ctx context.Context, id uuid.UUID) (ledger.Entry, error)
}

// SetLedger connects the ledger's memory store (ConnectLedger does it, with
// the reversal hook).
func (s *MemoryStore) SetLedger(l LedgerMemory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger = l
}

// ConnectLedger joins a tenancies store and a ledger store of the same kind:
// a rent receipt's ledger entry is written in the receipt's transaction,
// and a ledger reversal of that entry marks the receipt reversed in the
// reversal's transaction. Other store kinds are left alone.
func ConnectLedger(store Store, ledgerStore ledger.Store) error {
	switch t := store.(type) {
	case *MemoryStore:
		l, ok := ledgerStore.(*ledger.MemoryStore)
		if !ok {
			return errors.New("tenancies: a memory store needs the ledger's memory store")
		}
		t.SetLedger(l)
		l.SetReversalHook(t.ReleaseReceipts)
	case *PostgresStore:
		l, ok := ledgerStore.(*ledger.PostgresStore)
		if !ok {
			return errors.New("tenancies: a Postgres store needs the ledger's Postgres store")
		}
		l.SetReversalHook(ReleaseReceiptsTx)
	}
	return nil
}

// ReleaseReceipts is the ledger memory store's reversal hook: the receipt
// of each reversed entry is marked reversed.
func (s *MemoryStore) ReleaseReceipts(_ context.Context, list []ledger.Reversal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rev := range list {
		for id, r := range s.receipts {
			if r.LedgerEntryID != rev.EntryID || r.ReversedAt != nil {
				continue
			}
			at, entry := rev.At, rev.ReversalID
			r.ReversedAt, r.ReversalEntryID, r.ReversedOn = &at, &entry, rev.EffectiveDate
			s.receipts[id] = r
		}
	}
	return nil
}

// ActiveTenancies implements Store.
func (s *MemoryStore) ActiveTenancies(_ context.Context) ([]Tenancy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Tenancy
	for _, t := range s.tenancies {
		if t.Status == StatusActive {
			out = append(out, cloneTenancy(t))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}

// withAllocations fills a due's allocations from the receipts.
func (s *MemoryStore) withAllocations(d Due) Due {
	d.Allocations = []DueAllocation{}
	var recs []Receipt
	for _, r := range s.receipts {
		recs = append(recs, r)
	}
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].ReceivedOn != recs[j].ReceivedOn {
			return recs[i].ReceivedOn < recs[j].ReceivedOn
		}
		if !recs[i].RecordedAt.Equal(recs[j].RecordedAt) {
			return recs[i].RecordedAt.Before(recs[j].RecordedAt)
		}
		return recs[i].ID.String() < recs[j].ID.String()
	})
	for _, r := range recs {
		for _, a := range r.Allocations {
			if a.DueID == d.ID {
				d.Allocations = append(d.Allocations, DueAllocation{ReceiptID: r.ID, Amount: a.Amount, ReceivedOn: r.ReceivedOn, ReversedOn: r.ReversedOn})
			}
		}
	}
	return d
}

// Dues implements Store.
func (s *MemoryStore) Dues(_ context.Context, tenancyIDs []uuid.UUID) ([]Due, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Due
	for _, d := range s.dues {
		if slices.Contains(tenancyIDs, d.TenancyID) {
			out = append(out, s.withAllocations(d))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TenancyID != out[j].TenancyID {
			return out[i].TenancyID.String() < out[j].TenancyID.String()
		}
		return out[i].DueDate < out[j].DueDate
	})
	return out, nil
}

// Due implements Store.
func (s *MemoryStore) Due(_ context.Context, id uuid.UUID) (Due, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.dues[id]
	if !ok {
		return Due{}, ErrDueNotFound
	}
	return s.withAllocations(d), nil
}

// Receipts implements Store. The ledger is read after the store's lock is
// released (the ledger's reversal hook locks this store).
func (s *MemoryStore) Receipts(ctx context.Context, tenancyID uuid.UUID) ([]Receipt, error) {
	s.mu.Lock()
	var out []Receipt
	for _, r := range s.receipts {
		if r.TenancyID == tenancyID {
			r.Allocations = slices.Clone(r.Allocations)
			out = append(out, r)
		}
	}
	dueDates := map[uuid.UUID]string{}
	for _, d := range s.dues {
		dueDates[d.ID] = d.DueDate
	}
	l := s.ledger
	s.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ReceivedOn != b.ReceivedOn {
			return a.ReceivedOn > b.ReceivedOn
		}
		if !a.RecordedAt.Equal(b.RecordedAt) {
			return a.RecordedAt.After(b.RecordedAt)
		}
		return a.ID.String() > b.ID.String()
	})
	for i := range out {
		sort.SliceStable(out[i].Allocations, func(x, y int) bool {
			return dueDates[out[i].Allocations[x].DueID] < dueDates[out[i].Allocations[y].DueID]
		})
		if l == nil {
			continue
		}
		e, err := l.Entry(ctx, out[i].LedgerEntryID)
		if errors.Is(err, ledger.ErrEntryNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[i].LedgerStatus, out[i].LedgerAuthor = e.Status, e.AuthorUserID
	}
	return out, nil
}

// checkRent verifies the rent parts of a Change, as Postgres would.
func (s *MemoryStore) checkRent(ch Change) error {
	if w := ch.Waive; w != nil {
		d, ok := s.dues[w.DueID]
		if !ok {
			return ErrDueNotFound
		}
		if st := s.withAllocations(d).state(forever); st.status == DueWaived || st.outstanding.Sign() <= 0 {
			return ErrDueChanged
		}
	}
	for _, r := range ch.Receipts {
		for _, a := range r.Allocations {
			d, ok := s.dues[a.DueID]
			if !ok || d.TenancyID != r.TenancyID {
				return ErrDueChanged
			}
			st := s.withAllocations(d).state(forever)
			if st.status == DueWaived || money.Rat(a.Amount).Cmp(st.outstanding) > 0 {
				return ErrDueChanged
			}
		}
	}
	return nil
}

// applyRent writes the rent parts of a checked Change.
func (s *MemoryStore) applyRent(ch Change) {
	for _, d := range ch.Dues {
		taken := false
		for _, x := range s.dues {
			if x.TenancyID == d.TenancyID && x.DueDate == d.DueDate {
				taken = true
				break
			}
		}
		if !taken {
			d.Allocations = nil
			s.dues[d.ID] = d
		}
	}
	for _, id := range ch.DeleteDues {
		d, ok := s.dues[id]
		if !ok || d.WaivedAt != nil || len(s.withAllocations(d).Allocations) > 0 {
			continue
		}
		delete(s.dues, id)
	}
	if w := ch.Waive; w != nil {
		d := s.dues[w.DueID]
		at := w.At
		d.WaivedAt, d.WaivedBy, d.WaivedByName, d.WaiveReason = &at, w.By, w.ByName, w.Reason
		s.dues[d.ID] = d
	}
	for _, r := range ch.Receipts {
		r.Allocations = slices.Clone(r.Allocations)
		r.LedgerStatus, r.LedgerAuthor = "", nil
		s.receipts[r.ID] = r
	}
}
