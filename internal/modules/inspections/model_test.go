package inspections

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTransitions(t *testing.T) {
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{StatusScheduled, StatusInProgress, true},
		{StatusScheduled, StatusCancelled, true},
		{StatusScheduled, StatusCompleted, false},
		{StatusInProgress, StatusCompleted, true},
		{StatusInProgress, StatusCancelled, true},
		{StatusInProgress, StatusScheduled, false},
		{StatusCompleted, StatusInProgress, false},
		{StatusCompleted, StatusCancelled, false},
		{StatusCancelled, StatusInProgress, false},
		{StatusCancelled, StatusCompleted, false},
	} {
		if got := CanMove(c.from, c.to); got != c.ok {
			t.Errorf("%s -> %s: %v, want %v", c.from, c.to, got, c.ok)
		}
	}
	for _, st := range Statuses() {
		if Open(st) != (st == StatusScheduled || st == StatusInProgress) {
			t.Errorf("Open(%s)", st)
		}
		if !Open(st) && len(Transitions(st)) != 0 {
			t.Errorf("%s is frozen but has transitions", st)
		}
	}
}

func rows() []TemplateRow {
	return []TemplateRow{
		{Code: "home", Label: "Home"},
		{Code: "home-kitchen", Label: "Kitchen", ParentCode: "home"},
		{Code: "home-kitchen-sink", Label: "Sink", ParentCode: "home-kitchen"},
		{Code: "home-kitchen-sink-tap", Label: "Too deep", ParentCode: "home-kitchen-sink"},
		{Code: "home-empty", Label: "Empty room", ParentCode: "home"},
		{Code: "home-hall", Label: "Hall", ParentCode: "home"},
		{Code: "home-hall-door", Label: "Door", ParentCode: "home-hall"},
		{Code: "orphan-item", Label: "Orphan", ParentCode: "gone"},
		{Code: "bare", Label: "No items"},
		{Code: "bare-room", Label: "Room", ParentCode: "bare"},
	}
}

func TestBuildTemplates(t *testing.T) {
	tpls := BuildTemplates(rows())
	if len(tpls) != 1 || tpls[0].Code != "home" || tpls[0].ItemCount() != 2 || len(tpls[0].Areas) != 3 {
		t.Fatalf("templates: %+v", tpls)
	}
	c := Snapshot(tpls[0])
	if len(c.Areas) != 2 || c.Areas[0].Label != "Kitchen" || c.Areas[0].Items[0].Label != "Sink" || c.Areas[1].Items[0].Label != "Door" {
		t.Fatalf("snapshot: %+v", c)
	}
	// Two snapshots never share ids.
	d := Snapshot(tpls[0])
	if c.Areas[0].ID == d.Areas[0].ID || c.Areas[0].Items[0].ID == d.Areas[0].Items[0].ID {
		t.Fatal("snapshots share ids")
	}
}

func str(s string) *string { return &s }

func TestRecordIsPartial(t *testing.T) {
	c := Snapshot(BuildTemplates(rows())[0])
	sink, door := c.Areas[0].Items[0].ID, c.Areas[1].Items[0].ID
	var in FindingsInput
	if err := json.Unmarshal([]byte(`{"items":[{"item_id":"`+sink.String()+`","status":"ATTENTION","note":"Drips"}]}`), &in); err != nil {
		t.Fatal(err)
	}
	next, changed, f := Record(c, in)
	if len(f) != 0 || !changed || next.Areas[0].Items[0].Status != FindingAttention || next.Areas[1].Items[0].Status != "" {
		t.Fatalf("record: %+v %v", next, f)
	}
	if c.Areas[0].Items[0].Status != "" {
		t.Fatal("Record changed its input")
	}
	if missing := next.Missing(); len(missing) != 1 || missing[0] != door {
		t.Fatalf("missing: %v", missing)
	}
	// Absent note keeps it; null status clears.
	if err := json.Unmarshal([]byte(`{"items":[{"item_id":"`+sink.String()+`","status":null}]}`), &in); err != nil {
		t.Fatal(err)
	}
	again, _, f := Record(next, in)
	if len(f) != 0 || again.Areas[0].Items[0].Status != "" || again.Areas[0].Items[0].Note != "Drips" {
		t.Fatalf("null status: %+v %v", again.Areas[0].Items[0], f)
	}
	_, _, f = Record(next, FindingsInput{Items: []FindingInput{{ItemID: &door, Status: Optional{Set: true, Value: str("BROKEN")}},
		{ItemID: &door}, {ItemID: &uuid.Nil}}})
	if f["items[0].status"] == "" || f["items[1].item_id"] == "" || f["items[2].item_id"] == "" {
		t.Fatalf("findings validation: %v", f)
	}
}

func TestRestructureKeepsFindings(t *testing.T) {
	c := Snapshot(BuildTemplates(rows())[0])
	c.Areas[0].Items[0].Status, c.Areas[0].Items[0].Note = FindingDamaged, "Cracked"
	sink, door := c.Areas[0].Items[0].ID, c.Areas[1].Items[0].ID
	hall := c.Areas[1].ID
	// The sink moves into the hall; the kitchen and the door go; a new item comes.
	next, removed, f := Restructure(c, ChecklistInput{Areas: []AreaInput{{ID: &hall, Label: "Hall", Items: []ItemInput{
		{ID: &sink, Label: "Sink (moved)"}, {Label: "Coat hooks"}}}}})
	if len(f) != 0 || len(next.Areas) != 1 || len(next.Areas[0].Items) != 2 || next.Areas[0].Items[0].Status != FindingDamaged ||
		next.Areas[0].Items[0].Note != "Cracked" || next.Areas[0].Items[1].ID == uuid.Nil || !slices.Equal(removed, []uuid.UUID{door}) {
		t.Fatalf("restructure: %+v %v %v", next, removed, f)
	}
	_, _, f = Restructure(c, ChecklistInput{Areas: []AreaInput{{Label: ""}, {ID: &sink, Label: "x", Items: []ItemInput{{ID: &hall, Label: ""}}}}})
	if f["areas[0].label"] == "" || f["areas[1].id"] == "" || f["areas[1].items[0].id"] == "" || f["areas[1].items[0].label"] == "" {
		t.Fatalf("restructure validation: %v", f)
	}
	if _, _, f = Restructure(c, ChecklistInput{}); f["areas"] == "" {
		t.Fatal("an empty checklist was accepted")
	}
}

func TestBaselinesMatchByLabels(t *testing.T) {
	base := Snapshot(BuildTemplates(rows())[0])
	base.Areas[0].Items[0].Status = FindingDamaged
	now := Snapshot(BuildTemplates(rows())[0])
	now.Areas[0].Label = "  KITCHEN "
	got := Baselines(now, base)
	if b, ok := got[now.Areas[0].Items[0].ID]; !ok || b.Status != FindingDamaged || len(got) != 2 {
		t.Fatalf("baselines: %+v", got)
	}
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 80, 60))
	for x := range 80 {
		img.Set(x, 20, color.RGBA{G: 180, A: 255})
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The report renders (a PDF, with a thumbnail and a move-in column), also
// with long notes that cross pages.
func TestRenderReport(t *testing.T) {
	c := Snapshot(BuildTemplates(rows())[0])
	for i := range c.Areas {
		for j := range c.Areas[i].Items {
			c.Areas[i].Items[j].Status = FindingOK
		}
	}
	c.Areas[0].Items[0].Status, c.Areas[0].Items[0].Note = FindingDamaged, strings.Repeat("Cracked basin, ", 60)
	for i := range 60 {
		c.Areas[1].Items = append(c.Areas[1].Items, Item{ID: uuid.New(), Label: "Extra item with a rather long label " + string(rune('A'+i%26)),
			Status: FindingNA})
	}
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	thumb, err := thumbnail(bytes.NewReader(testJPEG(t)))
	if err != nil {
		t.Fatal(err)
	}
	base := Inspection{Reference: "RPI-2026-000001", Checklist: Snapshot(BuildTemplates(rows())[0]), CompletedAt: &now}
	in := Inspection{Reference: "RPI-2026-000002", Type: TypeMoveOut, Status: StatusCompleted, ScheduledFor: now, StartedAt: &now,
		CompletedAt: &now, InspectorName: "Chipo Dube", TemplateLabel: "Home", Checklist: c, Summary: "Mostly fine. Ünïcödé ✓",
		TenantSignOff: "Kudzai Sithole", AgentSignOff: "Chipo Dube"}
	pdf, err := RenderReport(Report{Inspection: in, ManagedBy: "Sample Estates", Property: "Avondale Court", Address: "12 King George Road",
		Unit: "Flat 1", Tenants: []string{"Kudzai Sithole"}, Baseline: &base,
		PhotoCounts: map[uuid.UUID]int{c.Areas[0].Items[0].ID: 1},
		Thumbnails:  map[uuid.UUID][][]byte{c.Areas[0].Items[0].ID: {thumb}}, GeneratedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF")) || len(pdf) < 2000 || !bytes.Contains(pdf, []byte("/DCTDecode")) {
		t.Fatalf("report: %d bytes", len(pdf))
	}
	// A draft without photos renders too.
	in.Status, in.CompletedAt = StatusInProgress, nil
	if pdf, err = RenderReport(Report{Inspection: in, GeneratedAt: now}); err != nil || !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Fatalf("draft report: %v", err)
	}
}

// The outbox payload carries ids, the reference, the type and the status
// only, and only when there is a tenancy to tell.
func TestNoticePayloadIsSafe(t *testing.T) {
	tenancy := uuid.New()
	in := Inspection{ID: uuid.New(), Reference: "RPI-2026-000003", Type: TypeMoveIn, Status: StatusCompleted, TenancyID: &tenancy,
		Summary: "Kudzai Sithole, +263771234567", TenantSignOff: "Kudzai Sithole", InspectorName: "Chipo Dube"}
	n := notice(OutboxCompleted, in, uuid.New(), false, time.Now())
	if len(n) != 1 {
		t.Fatalf("notices: %+v", n)
	}
	raw, _ := json.Marshal(n[0].Payload)
	for _, banned := range []string{"Kudzai", "Chipo", "+263", "Sithole"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("payload carries %q: %s", banned, raw)
		}
	}
	in.TenancyID = nil
	if len(notice(OutboxCompleted, in, uuid.New(), false, time.Now())) != 0 {
		t.Fatal("a notice without a tenancy")
	}
}
