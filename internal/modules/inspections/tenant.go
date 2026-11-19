package inspections

import (
	"context"
	"errors"
	"io"
	"sort"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/auth"
)

// The tenant's own inspections: the COMPLETED ones linked to one of their
// tenancies (as GET /v1/me/tenancies/{id}: never a DRAFT, matched by
// verified email), read-only. Anybody else's tenancy is
// ErrTenancyNotFound; any other inspection is ErrNotFound.

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

func tenantVisible(t tenancies.Tenancy, in Inspection) bool {
	return in.TenancyID != nil && *in.TenancyID == t.ID && in.Book.Same(t.Book) && in.Status == StatusCompleted
}

func (s *Service) myInspection(ctx context.Context, t tenancies.Tenancy, id uuid.UUID) (Inspection, error) {
	in, err := s.store.Inspection(ctx, id)
	if err != nil {
		return Inspection{}, err
	}
	if !tenantVisible(t, in) {
		return Inspection{}, ErrNotFound
	}
	return in, nil
}

func (s *Service) myView(ctx context.Context, t tenancies.Tenancy, in Inspection, prop tenancies.Property) (MyInspection, error) {
	out := MyInspection{ID: in.ID, Reference: in.Reference, Type: in.Type, TypeLabel: TypeLabel(in.Type), Status: in.Status,
		StatusLabel: StatusLabel(in.Status), Property: NameOnly{Name: prop.Name}, ScheduledFor: in.ScheduledFor, CompletedAt: in.CompletedAt,
		Inspector: NameOnly{Name: in.InspectorName}, Template: LabelOnly{Label: in.TemplateLabel}, Progress: in.Checklist.Progress(),
		Areas: []MyArea{}, Summary: in.Summary, SignOff: SignOff{TenantName: strPtr(in.TenantSignOff), AgentName: strPtr(in.AgentSignOff)},
		ReportURL: MyReportPath(t.ID, in.ID)}
	if in.UnitID != nil {
		u, _ := prop.Unit(*in.UnitID)
		out.Unit = &LabelOnly{Label: u.Label}
	}
	base, err := s.baseline(ctx, in)
	if err != nil {
		return MyInspection{}, err
	}
	var baselines map[uuid.UUID]Item
	if base != nil {
		out.Baseline = &MyBaselineRef{Reference: base.Reference, CompletedAt: *base.CompletedAt}
		baselines = Baselines(in.Checklist, base.Checklist)
	}
	photos, err := s.store.Photos(ctx, in.ID)
	if err != nil {
		return MyInspection{}, err
	}
	byItem := photosByItem(photos)
	for _, a := range in.Checklist.Areas {
		ma := MyArea{ID: a.ID, Label: a.Label, Items: []MyItem{}}
		for _, it := range a.Items {
			mi := MyItem{ID: it.ID, Label: it.Label, Status: strPtr(it.Status), Note: it.Note, Photos: []MyPhoto{}}
			if it.Status != "" {
				mi.StatusLabel = strPtr(FindingLabel(it.Status))
			}
			for _, p := range byItem[it.ID] {
				mi.Photos = append(mi.Photos, MyPhoto{ID: p.ID, URL: MyPhotoPath(t.ID, in.ID, p.ID), ContentType: p.ContentType,
					Width: p.Width, Height: p.Height})
			}
			if base != nil {
				b, ok := baselines[it.ID]
				mi.Baseline = baselineView(b, ok)
			}
			ma.Items = append(ma.Items, mi)
		}
		out.Areas = append(out.Areas, ma)
	}
	return out, nil
}

func (s *Service) myProperty(ctx context.Context, id uuid.UUID) (tenancies.Property, error) {
	props, err := s.tenancies.Properties(ctx, []uuid.UUID{id})
	if err != nil {
		return tenancies.Property{}, err
	}
	return props[id], nil
}

// MyList serves GET /v1/me/tenancies/{id}/inspections.
func (s *Service) MyList(ctx context.Context, p *auth.Principal, tenancyID uuid.UUID) ([]MyInspection, error) {
	t, err := s.myTenancy(ctx, p, tenancyID)
	if err != nil {
		return nil, err
	}
	list, err := s.store.InspectionsOfTenancy(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	var shown []Inspection
	for _, in := range list {
		if tenantVisible(t, in) {
			shown = append(shown, in)
		}
	}
	sort.SliceStable(shown, func(i, j int) bool { return shown[i].CompletedAt.After(*shown[j].CompletedAt) })
	out := make([]MyInspection, 0, len(shown))
	props := map[uuid.UUID]tenancies.Property{}
	for _, in := range shown {
		prop, ok := props[in.PropertyID]
		if !ok {
			if prop, err = s.myProperty(ctx, in.PropertyID); err != nil {
				return nil, err
			}
			props[in.PropertyID] = prop
		}
		mv, err := s.myView(ctx, t, in, prop)
		if err != nil {
			return nil, err
		}
		out = append(out, mv)
	}
	return out, nil
}

// MyGet serves GET /v1/me/tenancies/{id}/inspections/{inspectionId}.
func (s *Service) MyGet(ctx context.Context, p *auth.Principal, tenancyID, id uuid.UUID) (MyInspection, error) {
	t, err := s.myTenancy(ctx, p, tenancyID)
	if err != nil {
		return MyInspection{}, err
	}
	in, err := s.myInspection(ctx, t, id)
	if err != nil {
		return MyInspection{}, err
	}
	prop, err := s.myProperty(ctx, in.PropertyID)
	if err != nil {
		return MyInspection{}, err
	}
	return s.myView(ctx, t, in, prop)
}

// MyReport serves GET /v1/me/tenancies/{id}/inspections/{inspectionId}/report.pdf.
func (s *Service) MyReport(ctx context.Context, p *auth.Principal, tenancyID, id uuid.UUID) ([]byte, string, error) {
	t, err := s.myTenancy(ctx, p, tenancyID)
	if err != nil {
		return nil, "", err
	}
	in, err := s.myInspection(ctx, t, id)
	if err != nil {
		return nil, "", err
	}
	pdf, err := s.report(ctx, in, t.BookName)
	if err != nil {
		return nil, "", err
	}
	return pdf, ReportFileName(in), nil
}

// MyOpenPhoto serves GET /v1/me/tenancies/{id}/inspections/{inspectionId}/photos/{photoId}.
func (s *Service) MyOpenPhoto(ctx context.Context, p *auth.Principal, tenancyID, id, photoID uuid.UUID) (io.ReadCloser, error) {
	t, err := s.myTenancy(ctx, p, tenancyID)
	if err != nil {
		return nil, err
	}
	if _, err := s.myInspection(ctx, t, id); err != nil {
		return nil, err
	}
	ph, err := s.photoOf(ctx, id, photoID)
	if err != nil {
		return nil, err
	}
	return s.openFile(ctx, ph)
}
