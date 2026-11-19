package inspections

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// TemplateRow is one active row of the reference set
// inspection_checklist_templates, in the set's order (sort_order, label).
type TemplateRow struct {
	Code       string
	Label      string
	ParentCode string
}

// TemplateItem is one item of a template.
type TemplateItem struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// TemplateArea is one area of a template.
type TemplateArea struct {
	Code  string         `json:"code"`
	Label string         `json:"label"`
	Items []TemplateItem `json:"items"`
}

// Template is one checklist template.
type Template struct {
	Code  string         `json:"code"`
	Label string         `json:"label"`
	Areas []TemplateArea `json:"areas"`
}

// ItemCount is the number of items of a template.
func (t Template) ItemCount() int {
	n := 0
	for _, a := range t.Areas {
		n += len(a.Items)
	}
	return n
}

// BuildTemplates turns the rows of the set into templates: a row without a
// parent is a template, a row whose parent is a template an area, a row
// whose parent is an area an item; deeper rows (and rows under an unknown
// or inactive parent) are ignored. Templates without items are left out.
func BuildTemplates(rows []TemplateRow) []Template {
	kind := map[string]int{} // 1 template, 2 area, 3 item
	for _, r := range rows {
		if r.ParentCode == "" {
			kind[r.Code] = 1
		}
	}
	for depth := 2; depth <= 3; depth++ {
		for _, r := range rows {
			if r.ParentCode != "" && kind[r.Code] == 0 && kind[r.ParentCode] == depth-1 {
				kind[r.Code] = depth
			}
		}
	}
	var out []Template
	for _, t := range rows {
		if kind[t.Code] != 1 {
			continue
		}
		tpl := Template{Code: t.Code, Label: t.Label, Areas: []TemplateArea{}}
		for _, a := range rows {
			if kind[a.Code] != 2 || a.ParentCode != t.Code {
				continue
			}
			area := TemplateArea{Code: a.Code, Label: a.Label, Items: []TemplateItem{}}
			for _, it := range rows {
				if kind[it.Code] == 3 && it.ParentCode == a.Code {
					area.Items = append(area.Items, TemplateItem{Code: it.Code, Label: it.Label})
				}
			}
			tpl.Areas = append(tpl.Areas, area)
		}
		if tpl.ItemCount() > 0 && tpl.ItemCount() <= MaxItems && len(tpl.Areas) <= MaxAreas {
			out = append(out, tpl)
		}
	}
	if out == nil {
		out = []Template{}
	}
	return out
}

// Snapshot copies a template into a new checklist (its own ids); empty
// areas are left out and labels are cut to MaxLabel.
func Snapshot(t Template) Checklist {
	c := Checklist{Areas: []Area{}}
	for _, a := range t.Areas {
		if len(a.Items) == 0 {
			continue
		}
		area := Area{ID: newID(), Label: cut(a.Label, MaxLabel), Items: []Item{}}
		for _, it := range a.Items {
			if len(area.Items) == MaxItemsPerArea {
				break
			}
			area.Items = append(area.Items, Item{ID: newID(), Label: cut(it.Label, MaxLabel)})
		}
		c.Areas = append(c.Areas, area)
	}
	return c
}

func cut(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// Clone returns a deep copy.
func (c Checklist) Clone() Checklist {
	out := Checklist{Areas: make([]Area, 0, len(c.Areas))}
	for _, a := range c.Areas {
		a.Items = slices.Clone(a.Items)
		out.Areas = append(out.Areas, a)
	}
	return out
}

// Item returns the item with that id.
func (c Checklist) Item(id uuid.UUID) (Item, Area, bool) {
	for _, a := range c.Areas {
		for _, it := range a.Items {
			if it.ID == id {
				return it, a, true
			}
		}
	}
	return Item{}, Area{}, false
}

// ItemIDs returns every item id, in order.
func (c Checklist) ItemIDs() []uuid.UUID {
	var out []uuid.UUID
	for _, a := range c.Areas {
		for _, it := range a.Items {
			out = append(out, it.ID)
		}
	}
	return out
}

// Missing returns the items without a status, in order.
func (c Checklist) Missing() []uuid.UUID {
	out := []uuid.UUID{}
	for _, a := range c.Areas {
		for _, it := range a.Items {
			if it.Status == "" {
				out = append(out, it.ID)
			}
		}
	}
	return out
}

// Progress counts the items and their findings.
type Progress struct {
	Items     int `json:"items"`
	Done      int `json:"done"`
	OK        int `json:"ok"`
	Attention int `json:"attention"`
	Damaged   int `json:"damaged"`
	NA        int `json:"na"`
}

// Progress counts the checklist.
func (c Checklist) Progress() Progress {
	var p Progress
	for _, a := range c.Areas {
		for _, it := range a.Items {
			p.Items++
			switch it.Status {
			case FindingOK:
				p.OK++
			case FindingAttention:
				p.Attention++
			case FindingDamaged:
				p.Damaged++
			case FindingNA:
				p.NA++
			default:
				continue
			}
			p.Done++
		}
	}
	return p
}

// Marshal is the checklist as stored (JSON).
func (c Checklist) Marshal() ([]byte, error) {
	if c.Areas == nil {
		c.Areas = []Area{}
	}
	for i := range c.Areas {
		if c.Areas[i].Items == nil {
			c.Areas[i].Items = []Item{}
		}
	}
	return json.Marshal(c)
}

// UnmarshalChecklist reads a stored checklist.
func UnmarshalChecklist(b []byte) (Checklist, error) {
	var c Checklist
	if len(bytes.TrimSpace(b)) == 0 {
		return Checklist{Areas: []Area{}}, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Checklist{}, fmt.Errorf("read checklist: %w", err)
	}
	if c.Areas == nil {
		c.Areas = []Area{}
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// PUT .../checklist
// ---------------------------------------------------------------------------

// ItemInput is one item of PUT .../checklist.
type ItemInput struct {
	ID    *uuid.UUID `json:"id"`
	Label string     `json:"label"`
}

// AreaInput is one area of PUT .../checklist.
type AreaInput struct {
	ID    *uuid.UUID  `json:"id"`
	Label string      `json:"label"`
	Items []ItemInput `json:"items"`
}

// ChecklistInput is the body of PUT .../checklist.
type ChecklistInput struct {
	Areas []AreaInput `json:"areas"`
}

// Restructure applies PUT .../checklist to a checklist: kept ids keep
// their finding (an item may move to another area), null ids are new, the
// rest is removed. It returns the new checklist, the removed item ids and
// the per-field problems.
func Restructure(cur Checklist, in ChecklistInput) (Checklist, []uuid.UUID, map[string]string) {
	f := map[string]string{}
	areas := map[uuid.UUID]bool{}
	items := map[uuid.UUID]Item{}
	for _, a := range cur.Areas {
		areas[a.ID] = true
		for _, it := range a.Items {
			items[it.ID] = it
		}
	}
	switch {
	case len(in.Areas) == 0:
		f["areas"] = "must hold at least one area"
	case len(in.Areas) > MaxAreas:
		f["areas"] = fmt.Sprintf("must hold at most %d areas", MaxAreas)
	}
	out := Checklist{Areas: []Area{}}
	seenArea, seenItem := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	total := 0
	for i, a := range in.Areas {
		if i >= MaxAreas {
			break
		}
		at := fmt.Sprintf("areas[%d]", i)
		area := Area{Label: text(f, at+".label", a.Label, 1, MaxLabel), Items: []Item{}}
		if a.ID != nil {
			switch {
			case !areas[*a.ID]:
				f[at+".id"] = "is not an area of this inspection"
			case seenArea[*a.ID]:
				f[at+".id"] = "is listed twice"
			}
			seenArea[*a.ID] = true
			area.ID = *a.ID
		} else {
			area.ID = newID()
		}
		if len(a.Items) > MaxItemsPerArea {
			f[at+".items"] = fmt.Sprintf("must hold at most %d items", MaxItemsPerArea)
		}
		for j, it := range a.Items {
			if j >= MaxItemsPerArea {
				break
			}
			total++
			ip := fmt.Sprintf("%s.items[%d]", at, j)
			item := Item{Label: text(f, ip+".label", it.Label, 1, MaxLabel)}
			if it.ID != nil {
				old, ok := items[*it.ID]
				switch {
				case !ok:
					f[ip+".id"] = "is not an item of this inspection"
				case seenItem[*it.ID]:
					f[ip+".id"] = "is listed twice"
				default:
					item.Status, item.Note = old.Status, old.Note
				}
				seenItem[*it.ID] = true
				item.ID = *it.ID
			} else {
				item.ID = newID()
			}
			area.Items = append(area.Items, item)
		}
		out.Areas = append(out.Areas, area)
	}
	switch {
	case total == 0 && len(in.Areas) > 0:
		f["areas"] = "must hold at least one item"
	case total > MaxItems:
		f["areas"] = fmt.Sprintf("must hold at most %d items in all", MaxItems)
	}
	var removed []uuid.UUID
	for _, a := range cur.Areas {
		for _, it := range a.Items {
			if !seenItem[it.ID] {
				removed = append(removed, it.ID)
			}
		}
	}
	return out, removed, f
}

// ---------------------------------------------------------------------------
// PUT .../findings
// ---------------------------------------------------------------------------

// Optional is a JSON value that may be absent (Set false), null (Set true,
// Value nil) or a string.
type Optional struct {
	Set   bool
	Value *string
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *Optional) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(bytes.TrimSpace(b)) == "null" {
		o.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	o.Value = &s
	return nil
}

// FindingInput is one line of PUT .../findings.
type FindingInput struct {
	ItemID *uuid.UUID `json:"item_id"`
	Status Optional   `json:"status"`
	Note   Optional   `json:"note"`
}

// FindingsInput is the body of PUT .../findings.
type FindingsInput struct {
	Items []FindingInput `json:"items"`
}

// Record applies PUT .../findings to a checklist (only the listed items
// change). It returns the new checklist, whether anything changed and the
// per-field problems.
func Record(cur Checklist, in FindingsInput) (Checklist, bool, map[string]string) {
	f := map[string]string{}
	switch {
	case len(in.Items) == 0:
		f["items"] = "must hold at least one item"
	case len(in.Items) > MaxItems:
		f["items"] = fmt.Sprintf("must hold at most %d items", MaxItems)
	}
	next := cur.Clone()
	where := map[uuid.UUID][2]int{}
	for i, a := range next.Areas {
		for j, it := range a.Items {
			where[it.ID] = [2]int{i, j}
		}
	}
	seen := map[uuid.UUID]bool{}
	changed := false
	for k, line := range in.Items {
		if k >= MaxItems {
			break
		}
		at := fmt.Sprintf("items[%d]", k)
		if line.ItemID == nil {
			f[at+".item_id"] = "is required"
			continue
		}
		pos, ok := where[*line.ItemID]
		switch {
		case !ok:
			f[at+".item_id"] = "is not an item of this inspection"
			continue
		case seen[*line.ItemID]:
			f[at+".item_id"] = "is listed twice"
			continue
		}
		seen[*line.ItemID] = true
		it := &next.Areas[pos[0]].Items[pos[1]]
		if line.Status.Set {
			st := ""
			if line.Status.Value != nil {
				st = strings.TrimSpace(*line.Status.Value)
				if !slices.Contains(FindingStatuses(), st) {
					f[at+".status"] = "must be one of " + strings.Join(FindingStatuses(), ", ") + ", or null"
				}
			}
			if st != it.Status {
				it.Status, changed = st, true
			}
		}
		if line.Note.Set {
			note := ""
			if line.Note.Value != nil {
				note = text(f, at+".note", *line.Note.Value, 0, MaxNote)
			}
			if note != it.Note {
				it.Note, changed = note, true
			}
		}
	}
	return next, changed, f
}

// ---------------------------------------------------------------------------
// The move-out comparison
// ---------------------------------------------------------------------------

func matchKey(area, item string) string {
	return strings.ToLower(strings.TrimSpace(area)) + "\x00" + strings.ToLower(strings.TrimSpace(item))
}

// Baselines maps each item of c to the item of base with the same area and
// item labels (case-insensitive, trimmed).
func Baselines(c, base Checklist) map[uuid.UUID]Item {
	byKey := map[string]Item{}
	for _, a := range base.Areas {
		for _, it := range a.Items {
			k := matchKey(a.Label, it.Label)
			if _, dup := byKey[k]; !dup {
				byKey[k] = it
			}
		}
	}
	out := map[uuid.UUID]Item{}
	for _, a := range c.Areas {
		for _, it := range a.Items {
			if b, ok := byKey[matchKey(a.Label, it.Label)]; ok {
				out[it.ID] = b
			}
		}
	}
	return out
}
