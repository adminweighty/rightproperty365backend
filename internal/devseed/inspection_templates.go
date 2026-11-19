package devseed

import "rightproperty365.com/backend/internal/modules/reference"

// The default inspection checklist templates (migration 0038): a tree in
// the reference set inspection_checklist_templates. A template row has no
// parent; an area's parent is its template (code <template>-<area>); an
// item's parent is its area (code <area code>-<item>).

type checklistArea struct {
	code, label string
	items       []string // code, label pairs
}

type checklistTemplate struct {
	code, label string
	areas       []checklistArea
}

func checklistTemplates() []checklistTemplate {
	return []checklistTemplate{
		{"residential", "Standard residential", []checklistArea{
			{"entrance", "Entrance and hallway", []string{"door", "Front door and locks", "walls", "Walls and ceiling", "floor", "Floor",
				"lights", "Lights and switches"}},
			{"living", "Living room", []string{"walls", "Walls and ceiling", "floor", "Floor and skirting", "windows", "Windows and curtains",
				"lights", "Lights, switches and sockets"}},
			{"kitchen", "Kitchen", []string{"sink", "Sink and taps", "cupboards", "Cupboards and drawers", "worktops", "Worktops",
				"stove", "Stove and oven", "walls", "Walls and floor"}},
			{"bathroom", "Bathroom", []string{"bath", "Bath or shower", "toilet", "Toilet", "basin", "Basin and taps", "tiles", "Tiles and grout",
				"fan", "Extractor fan"}},
			{"bedroom1", "Main bedroom", []string{"walls", "Walls and ceiling", "floor", "Floor", "windows", "Windows",
				"cupboards", "Built-in cupboards"}},
			{"bedroom2", "Second bedroom", []string{"walls", "Walls and ceiling", "floor", "Floor", "windows", "Windows"}},
			{"exterior", "Exterior and garden", []string{"garden", "Garden and lawn", "gate", "Gate and fencing", "gutters", "Gutters and drains"}},
			{"meters", "Meters, keys and safety", []string{"smoke-alarm", "Smoke alarm", "electricity", "Electricity meter reading",
				"water", "Water meter reading", "keys", "Keys handed over"}},
		}},
		{"commercial", "Short commercial", []checklistArea{
			{"reception", "Reception and entrance", []string{"doors", "Doors and locks", "walls", "Walls and ceiling", "floor", "Floor"}},
			{"office", "Office space", []string{"walls", "Walls and ceiling", "floor", "Floor and carpets", "lights", "Lights",
				"aircon", "Air conditioning"}},
			{"kitchenette", "Kitchenette and toilets", []string{"sink", "Sink and taps", "toilets", "Toilets", "basins", "Hand basins"}},
			{"safety", "Safety and services", []string{"extinguishers", "Fire extinguishers", "exits", "Emergency exits",
				"electricity", "Electricity meter reading"}},
		}},
	}
}

// InspectionTemplateRows returns the rows of inspection_checklist_templates,
// in the migration's order.
func InspectionTemplateRows() []reference.Item {
	var out []reference.Item
	for ti, t := range checklistTemplates() {
		out = append(out, reference.Item{Code: t.code, Label: t.label, SortOrder: (ti + 1) * 10, Active: true})
		for ai, a := range t.areas {
			areaCode, parent := t.code+"-"+a.code, t.code
			out = append(out, reference.Item{Code: areaCode, Label: a.label, SortOrder: (ai + 1) * 10, ParentCode: &parent, Active: true})
			for i := 0; i < len(a.items); i += 2 {
				p := areaCode
				out = append(out, reference.Item{Code: areaCode + "-" + a.items[i], Label: a.items[i+1], SortOrder: (i/2 + 1) * 10,
					ParentCode: &p, Active: true})
			}
		}
	}
	return out
}
