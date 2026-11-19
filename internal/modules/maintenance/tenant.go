package maintenance

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/auth"
)

// The tenant's own requests: those linked to one of their tenancies (as
// GET /v1/me/tenancies/{id}: never a DRAFT, matched by verified email).
// Anybody else's tenancy is ErrTenancyNotFound; a request of another
// tenancy is ErrNotFound.

func (s *Service) myTenancy(ctx context.Context, p *auth.Principal, tenancyID uuid.UUID) (tenancies.Tenancy, error) {
	if p == nil {
		return tenancies.Tenancy{}, ErrUnauthenticated
	}
	t, err := s.tenancies.Mine(ctx, p, tenancyID)
	if errors.Is(err, tenancies.ErrUnauthenticated) {
		return tenancies.Tenancy{}, ErrUnauthenticated
	}
	return t, err
}

func (s *Service) myRequest(ctx context.Context, t tenancies.Tenancy, id uuid.UUID) (Request, error) {
	r, err := s.store.Request(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if r.TenancyID == nil || *r.TenancyID != t.ID || !r.Book.Same(t.Book) {
		return Request{}, ErrNotFound
	}
	return r, nil
}

// tenantVisible reports whether a timeline item is shown to the tenant.
func tenantVisible(e Event) bool {
	switch e.Type {
	case EventCreated, EventStatusChanged, EventAssigned, EventPhotoAdded:
		return true
	case EventComment:
		return !e.Internal
	}
	return false
}

func (s *Service) myView(ctx context.Context, p *auth.Principal, t tenancies.Tenancy, r Request, v vocab, prop tenancies.Property) (MyRequest, error) {
	you := func(id *uuid.UUID) bool { return id != nil && *id == p.UserID }
	out := MyRequest{ID: r.ID, Reference: r.Reference, Status: r.Status, StatusLabel: StatusLabel(r.Status),
		Category: label(v.categories, r.Category, r.CategoryLabel), Priority: label(v.priorities, r.Priority, r.PriorityLabel),
		Title: r.Title, Description: r.Description, Property: NameOnly{Name: prop.Name},
		RaisedBy: MyRaisedBy{Kind: r.RaisedByKind, You: r.RaisedByKind == ActorTenant && you(r.RaisedBy)},
		Photos:   []MyPhoto{}, Timeline: []MyTimelineItem{}, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, StatusChangedAt: r.StatusChangedAt,
		Can: MyCan{Comment: Open(r.Status), AddPhoto: Open(r.Status)}}
	if r.UnitID != nil {
		u, _ := prop.Unit(*r.UnitID)
		out.Unit = &LabelOnly{Label: u.Label}
	}
	if r.Provider != nil {
		out.Provider = &NameOnly{Name: r.Provider.Name}
	}
	photos, err := s.store.Photos(ctx, r.ID)
	if err != nil {
		return MyRequest{}, err
	}
	for _, ph := range photos {
		out.Photos = append(out.Photos, MyPhoto{ID: ph.ID, URL: MyPhotoPath(t.ID, r.ID, ph.ID), ContentType: ph.ContentType, Width: ph.Width,
			Height: ph.Height, UploadedAt: ph.UploadedAt})
	}
	events, err := s.store.Events(ctx, r.ID)
	if err != nil {
		return MyRequest{}, err
	}
	for _, e := range events {
		if tenantVisible(e) {
			out.Timeline = append(out.Timeline, myItem(p, t, e))
		}
	}
	return out, nil
}

func myItem(p *auth.Principal, t tenancies.Tenancy, e Event) MyTimelineItem {
	a := MyActor{Kind: e.ActorKind, Name: e.ActorName}
	switch e.ActorKind {
	case ActorManager:
		a.Name = t.BookName
	case ActorTenant:
		a.You = e.ActorUserID != nil && *e.ActorUserID == p.UserID
	}
	return MyTimelineItem{ID: e.ID, Type: e.Type, At: e.At, Actor: a, FromStatus: strPtr(e.FromStatus), ToStatus: strPtr(e.ToStatus),
		Text: strPtr(e.Text)}
}

func (s *Service) myOne(ctx context.Context, p *auth.Principal, t tenancies.Tenancy, r Request) (MyRequest, error) {
	v, err := s.vocab(ctx)
	if err != nil {
		return MyRequest{}, err
	}
	props, err := s.tenancies.Properties(ctx, []uuid.UUID{r.PropertyID})
	if err != nil {
		return MyRequest{}, err
	}
	return s.myView(ctx, p, t, r, v, props[r.PropertyID])
}

// MyOptions serves GET /v1/me/tenancies/{id}/maintenance/options.
func (s *Service) MyOptions(ctx context.Context, p *auth.Principal, tenancyID uuid.UUID) (MyOptions, error) {
	if _, err := s.myTenancy(ctx, p, tenancyID); err != nil {
		return MyOptions{}, err
	}
	v, err := s.vocab(ctx)
	if err != nil {
		return MyOptions{}, err
	}
	return MyOptions{Categories: v.categories, Priorities: v.priorities, PhotoRules: photoRules()}, nil
}

// MyList serves GET /v1/me/tenancies/{id}/maintenance.
func (s *Service) MyList(ctx context.Context, p *auth.Principal, tenancyID uuid.UUID) ([]MyRequest, error) {
	t, err := s.myTenancy(ctx, p, tenancyID)
	if err != nil {
		return nil, err
	}
	list, err := s.store.RequestsOfTenancy(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	v, err := s.vocab(ctx)
	if err != nil {
		return nil, err
	}
	props, err := s.tenancies.Properties(ctx, []uuid.UUID{t.PropertyID})
	if err != nil {
		return nil, err
	}
	out := make([]MyRequest, 0, len(list))
	for _, r := range list {
		if !r.Book.Same(t.Book) {
			continue
		}
		prop, ok := props[r.PropertyID]
		if !ok {
			more, err := s.tenancies.Properties(ctx, []uuid.UUID{r.PropertyID})
			if err != nil {
				return nil, err
			}
			prop = more[r.PropertyID]
		}
		mv, err := s.myView(ctx, p, t, r, v, prop)
		if err != nil {
			return nil, err
		}
		out = append(out, mv)
	}
	return out, nil
}

// MyGet serves GET /v1/me/tenancies/{id}/maintenance/{requestId}.
func (s *Service) MyGet(ctx context.Context, p *auth.Principal, tenancyID, id uuid.UUID) (MyRequest, error) {
	t, err := s.myTenancy(ctx, p, tenancyID)
	if err != nil {
		return MyRequest{}, err
	}
	r, err := s.myRequest(ctx, t, id)
	if err != nil {
		return MyRequest{}, err
	}
	return s.myOne(ctx, p, t, r)
}

// MyCreate serves POST /v1/me/tenancies/{id}/maintenance: on the tenancy's
// property and unit, only while the tenancy is ACTIVE.
func (s *Service) MyCreate(ctx context.Context, p *auth.Principal, tenancyID uuid.UUID, in MyInput) (MyRequest, error) {
	t, err := s.myTenancy(ctx, p, tenancyID)
	if err != nil {
		return MyRequest{}, err
	}
	if t.Status != tenancies.StatusActive {
		return MyRequest{}, &tenancies.StateError{Status: t.Status}
	}
	now := s.clock()
	by, tid := p.UserID, t.ID
	r := Request{ID: newID(), Book: t.Book, PropertyID: t.PropertyID, UnitID: t.UnitID, TenancyID: &tid, Status: StatusNew,
		RaisedByKind: ActorTenant, RaisedBy: &by, RaisedByName: p.Name, StatusChangedAt: now, CreatedAt: now, UpdatedAt: now}
	f := map[string]string{}
	if err := s.fill(ctx, &r, in.Category, in.Priority, in.Title, in.Description, f); err != nil {
		return MyRequest{}, err
	}
	if err := invalid(f); err != nil {
		return MyRequest{}, err
	}
	if err := s.create(ctx, r, tenantActor(p)); err != nil {
		return MyRequest{}, err
	}
	got, err := s.store.Request(ctx, r.ID)
	if err != nil {
		return MyRequest{}, err
	}
	return s.myOne(ctx, p, t, got)
}

// MyComment serves POST /v1/me/tenancies/{id}/maintenance/{requestId}/comments.
func (s *Service) MyComment(ctx context.Context, p *auth.Principal, tenancyID, id uuid.UUID, in CommentInput) (MyTimelineItem, error) {
	t, err := s.myTenancy(ctx, p, tenancyID)
	if err != nil {
		return MyTimelineItem{}, err
	}
	r, err := s.myRequest(ctx, t, id)
	if err != nil {
		return MyTimelineItem{}, err
	}
	if !Open(r.Status) {
		return MyTimelineItem{}, ErrClosed
	}
	f := map[string]string{}
	body := text(f, "text", in.Text, 1, MaxComment)
	if err := invalid(f); err != nil {
		return MyTimelineItem{}, err
	}
	now := s.clock()
	ev := tenantActor(p).event(r.ID, EventComment, now)
	ev.Text = body
	if err := s.store.Apply(ctx, Change{Touch: map[uuid.UUID]time.Time{r.ID: now}, Events: []Event{ev}}); err != nil {
		return MyTimelineItem{}, err
	}
	return myItem(p, t, ev), nil
}

// MyAddPhoto serves POST /v1/me/tenancies/{id}/maintenance/{requestId}/photos.
func (s *Service) MyAddPhoto(ctx context.Context, p *auth.Principal, tenancyID, id uuid.UUID, content []byte) (MyPhoto, error) {
	t, err := s.myTenancy(ctx, p, tenancyID)
	if err != nil {
		return MyPhoto{}, err
	}
	r, err := s.myRequest(ctx, t, id)
	if err != nil {
		return MyPhoto{}, err
	}
	ph, err := s.addPhoto(ctx, r, tenantActor(p), content)
	if err != nil {
		return MyPhoto{}, err
	}
	return MyPhoto{ID: ph.ID, URL: MyPhotoPath(t.ID, r.ID, ph.ID), ContentType: ph.ContentType, Width: ph.Width, Height: ph.Height,
		UploadedAt: ph.UploadedAt}, nil
}

// MyOpenPhoto serves GET /v1/me/tenancies/{id}/maintenance/{requestId}/photos/{photoId}.
func (s *Service) MyOpenPhoto(ctx context.Context, p *auth.Principal, tenancyID, id, photoID uuid.UUID) (io.ReadCloser, error) {
	t, err := s.myTenancy(ctx, p, tenancyID)
	if err != nil {
		return nil, err
	}
	if _, err := s.myRequest(ctx, t, id); err != nil {
		return nil, err
	}
	ph, err := s.photoOf(ctx, id, photoID)
	if err != nil {
		return nil, err
	}
	return s.open(ctx, ph)
}
