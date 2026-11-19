package tenancies

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

// What the later parts of property management (maintenance, part c;
// inspections, part d) read from this module: the caller's access to a
// book, the book's properties and tenancies, and the tenant's own
// tenancies. The rules stay here, so every part answers the same 404s.

// BookAccess is what a caller may do in one book.
type BookAccess struct {
	Book Book
	// Write is OWNER, MANAGER, AGENT (or the direct owner); STAFF read only.
	Write bool
	// Name is the book's name: the organisation's, or the direct owner's.
	Name string
}

// Access returns the caller's access to a book: ErrUnauthenticated, or
// ErrBookNotFound when the book is not theirs.
func (s *Service) Access(p *auth.Principal, bookID uuid.UUID) (BookAccess, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return BookAccess{}, err
	}
	return BookAccess{Book: a.book, Write: a.write, Name: a.name}, nil
}

// LiveProperty returns a live property of the book with its units, or
// ErrPropertyNotFound (another book's, or archived).
func (s *Service) LiveProperty(ctx context.Context, book Book, id uuid.UUID) (Property, error) {
	p, err := s.store.Property(ctx, id)
	if err != nil {
		return Property{}, err
	}
	if !p.Book.Same(book) || p.DeletedAt != nil {
		return Property{}, ErrPropertyNotFound
	}
	return p, nil
}

// Properties returns the properties with those IDs (archived ones too), by ID.
func (s *Service) Properties(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Property, error) {
	out := map[uuid.UUID]Property{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := s.store.PropertiesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		out[p.ID] = p
	}
	return out, nil
}

// BookTenancy returns a tenancy of the book, or ErrTenancyNotFound.
func (s *Service) BookTenancy(ctx context.Context, book Book, id uuid.UUID) (Tenancy, error) {
	t, err := s.store.Tenancy(ctx, id)
	if err != nil {
		return Tenancy{}, err
	}
	if !t.Book.Same(book) {
		return Tenancy{}, ErrTenancyNotFound
	}
	return t, nil
}

// TenancyByID returns a tenancy whatever its book (the notification
// sources only), or ErrTenancyNotFound.
func (s *Service) TenancyByID(ctx context.Context, id uuid.UUID) (Tenancy, error) {
	return s.store.Tenancy(ctx, id)
}

// Briefs returns the tenancies of the book with those IDs as TenancyBrief
// (contract 0.36 "Shared pieces (b)"), by ID; another book's are left out.
func (s *Service) Briefs(ctx context.Context, book Book, ids []uuid.UUID) (map[uuid.UUID]TenancyBrief, error) {
	out := map[uuid.UUID]TenancyBrief{}
	var list []Tenancy
	for _, id := range ids {
		t, err := s.store.Tenancy(ctx, id)
		if errors.Is(err, ErrTenancyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if t.Book.Same(book) {
			list = append(list, t)
		}
	}
	v, err := s.viewer(ctx, book, list)
	if err != nil {
		return nil, err
	}
	for _, t := range list {
		b, err := v.brief(ctx, t)
		if err != nil {
			return nil, err
		}
		out[t.ID] = b
	}
	return out, nil
}

// PropertyListing returns the property's listing when it is still a
// listing of the book (nil otherwise): what a ledger entry may link.
func (s *Service) PropertyListing(ctx context.Context, p Property) (*uuid.UUID, error) {
	if p.ListingID == nil || s.links == nil {
		return nil, nil
	}
	_, ok, err := s.links.Listing(ctx, p.Book, *p.ListingID)
	if err != nil || !ok {
		return nil, err
	}
	id := *p.ListingID
	return &id, nil
}

// Mine returns one of the caller's own tenancies (never a DRAFT), or
// ErrTenancyNotFound (anybody else's), as GET /v1/me/tenancies/{id}.
func (s *Service) Mine(ctx context.Context, p *auth.Principal, id uuid.UUID) (Tenancy, error) {
	return s.myTenancy(ctx, p, id)
}
