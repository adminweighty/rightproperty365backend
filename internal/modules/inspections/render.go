package inspections

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/tenancies"
)

// viewer holds what the views of a set of inspections need.
type viewer struct {
	s          *Service
	a          access
	properties map[uuid.UUID]tenancies.Property
}

func (s *Service) viewer(ctx context.Context, a access, list []Inspection) (*viewer, error) {
	v := &viewer{s: s, a: a}
	var props []uuid.UUID
	for _, in := range list {
		if !slices.Contains(props, in.PropertyID) {
			props = append(props, in.PropertyID)
		}
	}
	var err error
	if v.properties, err = s.tenancies.Properties(ctx, props); err != nil {
		return nil, err
	}
	return v, nil
}

func unitRef(prop tenancies.Property, id *uuid.UUID) *UnitRef {
	if id == nil {
		return nil
	}
	u, _ := prop.Unit(*id)
	return &UnitRef{ID: *id, Label: u.Label, Archived: u.DeletedAt != nil}
}

func person(id *uuid.UUID, name string) tenancies.PersonRef {
	return tenancies.PersonRef{UserID: id, Name: name}
}

func personPtr(id *uuid.UUID, name string, set bool) *tenancies.PersonRef {
	if !set {
		return nil
	}
	p := person(id, name)
	return &p
}

func (v *viewer) summary(in Inspection) Summary {
	prop := v.properties[in.PropertyID]
	return Summary{ID: in.ID, Reference: in.Reference, Type: in.Type, TypeLabel: TypeLabel(in.Type), Status: in.Status,
		StatusLabel: StatusLabel(in.Status), Property: SummaryProperty{ID: in.PropertyID, Name: prop.Name, Archived: prop.DeletedAt != nil},
		Unit: unitRef(prop, in.UnitID), TenancyID: in.TenancyID, ScheduledFor: in.ScheduledFor,
		Inspector: person(in.InspectorID, in.InspectorName), Template: TemplateRef{Code: in.TemplateCode, Label: in.TemplateLabel},
		Progress: in.Checklist.Progress(), PhotoCount: in.PhotoCount, StartedAt: in.StartedAt, CompletedAt: in.CompletedAt,
		CancelledAt: in.CancelledAt, CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt}
}

// PhotoPath is the API path of a photo for the managers.
func PhotoPath(bookID, inspectionID, photoID uuid.UUID) string {
	return "/v1/agent/organisations/" + bookID.String() + "/inspections/" + inspectionID.String() + "/photos/" + photoID.String()
}

// ReportPath is the API path of the report for the managers.
func ReportPath(bookID, inspectionID uuid.UUID) string {
	return "/v1/agent/organisations/" + bookID.String() + "/inspections/" + inspectionID.String() + "/report.pdf"
}

// MyPhotoPath is the API path of a photo for the tenant.
func MyPhotoPath(tenancyID, inspectionID, photoID uuid.UUID) string {
	return "/v1/me/tenancies/" + tenancyID.String() + "/inspections/" + inspectionID.String() + "/photos/" + photoID.String()
}

// MyReportPath is the API path of the report for the tenant.
func MyReportPath(tenancyID, inspectionID uuid.UUID) string {
	return "/v1/me/tenancies/" + tenancyID.String() + "/inspections/" + inspectionID.String() + "/report.pdf"
}

func (v *viewer) photoView(in Inspection, p Photo) PhotoView {
	return PhotoView{ID: p.ID, ItemID: p.ItemID, URL: PhotoPath(v.a.book.ID(), in.ID, p.ID), ContentType: p.ContentType, Width: p.Width,
		Height: p.Height, Size: p.Size, UploadedBy: person(p.UploadedBy, p.UploadedByName), UploadedAt: p.UploadedAt,
		Can: PhotoCan{Delete: v.a.write && Open(in.Status)}}
}

func baselineView(b Item, ok bool) *BaselineView {
	if !ok {
		return nil
	}
	out := &BaselineView{Status: strPtr(b.Status), Note: b.Note}
	if b.Status != "" {
		out.StatusLabel = strPtr(FindingLabel(b.Status))
	}
	return out
}

func photosByItem(photos []Photo) map[uuid.UUID][]Photo {
	out := map[uuid.UUID][]Photo{}
	for _, p := range photos {
		out[p.ItemID] = append(out[p.ItemID], p)
	}
	return out
}

// view builds the full inspection (its photos, tenancy and baseline read here).
func (v *viewer) view(ctx context.Context, in Inspection) (InspectionView, error) {
	prop := v.properties[in.PropertyID]
	out := InspectionView{Summary: v.summary(in), Areas: []AreaView{}, SummaryText: in.Summary,
		SignOff:      SignOff{TenantName: strPtr(in.TenantSignOff), AgentName: strPtr(in.AgentSignOff)},
		CancelReason: strPtr(in.CancelReason), CreatedBy: person(in.CreatedBy, in.CreatedByName),
		CompletedBy: personPtr(in.CompletedBy, in.CompletedByName, in.CompletedAt != nil),
		CancelledBy: personPtr(in.CancelledBy, in.CancelledByName, in.CancelledAt != nil),
		ReportURL:   ReportPath(v.a.book.ID(), in.ID)}
	out.Property = FullProperty{ID: in.PropertyID, Name: prop.Name, Address: prop.Address, MarketCode: prop.MarketCode,
		Archived: prop.DeletedAt != nil}
	if in.TenancyID != nil {
		briefs, err := v.s.tenancies.Briefs(ctx, v.a.book, []uuid.UUID{*in.TenancyID})
		if err != nil {
			return InspectionView{}, err
		}
		if b, ok := briefs[*in.TenancyID]; ok {
			out.Tenancy = &b
		}
	}
	base, err := v.s.baseline(ctx, in)
	if err != nil {
		return InspectionView{}, err
	}
	var baselines map[uuid.UUID]Item
	if base != nil {
		out.Baseline = &BaselineRef{ID: base.ID, Reference: base.Reference, CompletedAt: *base.CompletedAt}
		baselines = Baselines(in.Checklist, base.Checklist)
	}
	photos, err := v.s.store.Photos(ctx, in.ID)
	if err != nil {
		return InspectionView{}, err
	}
	byItem := photosByItem(photos)
	for i, a := range in.Checklist.Areas {
		av := AreaView{ID: a.ID, Label: a.Label, Position: i + 1, Items: []ItemView{}}
		for j, it := range a.Items {
			iv := ItemView{ID: it.ID, Label: it.Label, Position: j + 1, Status: strPtr(it.Status), Note: it.Note, Photos: []PhotoView{}}
			if it.Status != "" {
				iv.StatusLabel = strPtr(FindingLabel(it.Status))
			}
			for _, p := range byItem[it.ID] {
				iv.Photos = append(iv.Photos, v.photoView(in, p))
			}
			if base != nil {
				b, ok := baselines[it.ID]
				iv.Baseline = baselineView(b, ok)
			}
			av.Items = append(av.Items, iv)
		}
		out.Areas = append(out.Areas, av)
	}
	if v.a.write {
		open := Open(in.Status)
		out.Can = InspectionCan{Edit: open, EditChecklist: open, RecordFindings: open, AddPhoto: open,
			Start: in.Status == StatusScheduled, Complete: in.Status == StatusInProgress, Cancel: open}
	}
	return out, nil
}

func (s *Service) oneView(ctx context.Context, a access, id uuid.UUID) (InspectionView, error) {
	in, err := s.inspection(ctx, a, id)
	if err != nil {
		return InspectionView{}, err
	}
	v, err := s.viewer(ctx, a, []Inspection{in})
	if err != nil {
		return InspectionView{}, err
	}
	return v.view(ctx, in)
}
