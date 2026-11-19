package devseed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/inspections"
	"rightproperty365.com/backend/internal/modules/tenancies"
)

// Sample inspections (fixed IDs b77x; their areas and items have ids
// derived from the inspection's).
var (
	// InspectionMoveInID is RPI-2026-900001: Flat 1's move-in, COMPLETED on
	// the tenancy's start date, with findings (two ATTENTION, one DAMAGED).
	InspectionMoveInID = id("b771")
	// InspectionRoutineID is RPI-2026-900002: Flat 1's routine inspection,
	// SCHEDULED 7 days ahead, no findings.
	InspectionRoutineID = id("b772")
)

// sampleTemplate returns a default template as the inspections module
// builds it from the reference rows.
func sampleTemplate(code string) inspections.Template {
	var rows []inspections.TemplateRow
	for _, it := range InspectionTemplateRows() {
		r := inspections.TemplateRow{Code: it.Code, Label: it.Label}
		if it.ParentCode != nil {
			r.ParentCode = *it.ParentCode
		}
		rows = append(rows, r)
	}
	for _, t := range inspections.BuildTemplates(rows) {
		if t.Code == code {
			return t
		}
	}
	panic("devseed: no inspection template " + code)
}

// seedChecklist copies a template with ids derived from the inspection.
func seedChecklist(inspection uuid.UUID, t inspections.Template) inspections.Checklist {
	c := inspections.Checklist{Areas: []inspections.Area{}}
	for i, a := range t.Areas {
		area := inspections.Area{ID: uuid.NewSHA1(inspection, fmt.Appendf(nil, "area-%d", i)), Label: a.Label, Items: []inspections.Item{}}
		for j, it := range a.Items {
			area.Items = append(area.Items, inspections.Item{ID: uuid.NewSHA1(inspection, fmt.Appendf(nil, "item-%d-%d", i, j)),
				Label: it.Label})
		}
		c.Areas = append(c.Areas, area)
	}
	return c
}

// InspectionSamples returns the sample inspections, dates relative to now:
// Flat 1's completed move-in (on the tenancy's start date) and its routine
// inspection 7 days ahead.
func InspectionSamples(now time.Time) []inspections.Inspection {
	now = now.UTC().Truncate(time.Second)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	ptr := func(u uuid.UUID) *uuid.UUID { return &u }
	tp := func(t time.Time) *time.Time { return &t }
	book := tenancies.OrganisationBook(SampleEstates.ID)
	chipo := EstatesAgent2ID
	tpl := sampleTemplate("residential")

	// The move-in: on the tenancy's start date (TenancySamples: two months ago).
	start := day.AddDate(0, -2, 0)
	moveIn := seedChecklist(InspectionMoveInID, tpl)
	notes := map[string][2]string{
		"Living room/Walls and ceiling": {inspections.FindingAttention, "Small scuff marks beside the door"},
		"Kitchen/Sink and taps":         {inspections.FindingAttention, "Cold tap drips slightly"},
		"Bathroom/Tiles and grout":      {inspections.FindingDamaged, "Cracked tile next to the bath"},
	}
	for i, a := range moveIn.Areas {
		for j, it := range a.Items {
			status, note := inspections.FindingOK, ""
			if a.Label == "Second bedroom" {
				status = inspections.FindingNA
			}
			if n, ok := notes[a.Label+"/"+it.Label]; ok {
				status, note = n[0], n[1]
			}
			moveIn.Areas[i].Items[j].Status, moveIn.Areas[i].Items[j].Note = status, note
		}
	}
	completed := start.Add(11 * time.Hour)
	first := inspections.Inspection{ID: InspectionMoveInID, Reference: "RPI-2026-900001", Book: book, PropertyID: PropertyAvondaleID,
		UnitID: ptr(UnitFlat1ID), TenancyID: ptr(TenancyFlat1ID), Type: inspections.TypeMoveIn, Status: inspections.StatusCompleted,
		ScheduledFor: start.Add(9 * time.Hour), InspectorID: ptr(chipo), InspectorName: "Chipo Dube", TemplateCode: tpl.Code,
		TemplateLabel: tpl.Label, Checklist: moveIn,
		Summary:       "The flat is clean and in good order. The dripping tap and the cracked bathroom tile were noted at move-in.",
		TenantSignOff: "Kudzai Sithole", AgentSignOff: "Chipo Dube", Version: 3, StartedAt: tp(start.Add(9 * time.Hour)),
		CompletedAt: tp(completed), CompletedBy: ptr(chipo), CompletedByName: "Chipo Dube", CreatedBy: ptr(chipo),
		CreatedByName: "Chipo Dube", CreatedAt: start.AddDate(0, 0, -5), UpdatedAt: completed}

	// The routine inspection, 7 days ahead at 09:00 UTC.
	when := day.AddDate(0, 0, 7).Add(9 * time.Hour)
	second := inspections.Inspection{ID: InspectionRoutineID, Reference: "RPI-2026-900002", Book: book, PropertyID: PropertyAvondaleID,
		UnitID: ptr(UnitFlat1ID), TenancyID: ptr(TenancyFlat1ID), Type: inspections.TypeRoutine, Status: inspections.StatusScheduled,
		ScheduledFor: when, InspectorID: ptr(chipo), InspectorName: "Chipo Dube", TemplateCode: tpl.Code, TemplateLabel: tpl.Label,
		Checklist: seedChecklist(InspectionRoutineID, tpl), CreatedBy: ptr(chipo), CreatedByName: "Chipo Dube",
		CreatedAt: now.AddDate(0, 0, -1), UpdatedAt: now.AddDate(0, 0, -1)}
	return []inspections.Inspection{first, second}
}

// SeedMemoryInspections stores the sample inspections in memory (no audit,
// no notifications). After the tenancies.
func SeedMemoryInspections(store *inspections.MemoryStore, now time.Time) error {
	for _, in := range InspectionSamples(now) {
		in := in
		if err := store.Put(inspections.Change{Create: &in}); err != nil {
			return err
		}
	}
	return nil
}

// seedPostgresInspections writes the samples (needs migration 0038; after
// the tenancies). The seeded inspections are replaced on every run (their
// dates are relative to now); deleting them removes their photo rows too.
func seedPostgresInspections(ctx context.Context, tx pgx.Tx, now time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM inspections WHERE id = ANY($1)`,
		[]uuid.UUID{InspectionMoveInID, InspectionRoutineID}); err != nil {
		return fmt.Errorf("seed inspections: %w", err)
	}
	for _, in := range InspectionSamples(now) {
		in := in
		if err := inspections.ApplyTx(ctx, tx, inspections.Change{Create: &in}); err != nil {
			return err
		}
	}
	return nil
}
