package mail

import (
	"fmt"
	"time"
)

// zoneFamilies name time zones as people say them. Each entry is the name in
// standard time and, where the zone changes its clocks, the name in summer
// (daylight saving) time; "" means the name does not change.
//
// The platform is global, so the table cannot name every zone: it covers the
// default zone of the countries most likely to be used first. A zone that is
// not here is still named correctly, by its offset and its IANA name
// ("UTC+05:45, Asia/Kathmandu") — see ZoneName.
var zoneFamilies = []struct {
	standard, summer string
	zones            []string
}{
	{"Coordinated Universal Time", "", []string{"UTC", "Etc/UTC"}},

	// Africa.
	{"Central Africa Time", "", []string{"Africa/Harare", "Africa/Gaborone", "Africa/Maputo", "Africa/Lusaka", "Africa/Blantyre", "Africa/Kigali", "Africa/Bujumbura", "Africa/Windhoek", "Africa/Juba", "Africa/Khartoum"}},
	{"South Africa Standard Time", "", []string{"Africa/Johannesburg", "Africa/Maseru", "Africa/Mbabane"}},
	{"East Africa Time", "", []string{"Africa/Nairobi", "Africa/Kampala", "Africa/Dar_es_Salaam", "Africa/Addis_Ababa", "Africa/Mogadishu", "Africa/Djibouti", "Africa/Asmara", "Indian/Antananarivo", "Indian/Comoro", "Indian/Mayotte"}},
	{"West Africa Time", "", []string{"Africa/Lagos", "Africa/Luanda", "Africa/Kinshasa", "Africa/Douala", "Africa/Libreville", "Africa/Malabo", "Africa/Niamey", "Africa/Ndjamena", "Africa/Bangui", "Africa/Brazzaville", "Africa/Porto-Novo"}},
	{"Greenwich Mean Time", "", []string{"Africa/Accra", "Africa/Abidjan", "Africa/Dakar", "Africa/Bamako", "Africa/Banjul", "Africa/Conakry", "Africa/Freetown", "Africa/Monrovia", "Africa/Lome", "Africa/Ouagadougou", "Africa/Nouakchott", "Africa/Bissau", "Africa/Sao_Tome", "Atlantic/Reykjavik", "Atlantic/St_Helena"}},
	{"Mauritius Time", "", []string{"Indian/Mauritius"}},
	{"Seychelles Time", "", []string{"Indian/Mahe"}},
	{"Eastern European Time", "Eastern European Summer Time", []string{"Africa/Cairo"}},
	{"Eastern European Time", "", []string{"Africa/Tripoli"}},
	{"Central European Time", "", []string{"Africa/Algiers", "Africa/Tunis"}},

	// Europe.
	{"Greenwich Mean Time", "British Summer Time", []string{"Europe/London", "Europe/Guernsey", "Europe/Jersey", "Europe/Isle_of_Man"}},
	{"Greenwich Mean Time", "Irish Standard Time", []string{"Europe/Dublin"}},
	{"Western European Time", "Western European Summer Time", []string{"Europe/Lisbon", "Atlantic/Faroe"}},
	{"Central European Time", "Central European Summer Time", []string{
		"Europe/Paris", "Europe/Berlin", "Europe/Madrid", "Europe/Rome", "Europe/Amsterdam", "Europe/Brussels", "Europe/Vienna", "Europe/Zurich",
		"Europe/Stockholm", "Europe/Oslo", "Europe/Copenhagen", "Europe/Warsaw", "Europe/Prague", "Europe/Budapest", "Europe/Belgrade",
		"Europe/Zagreb", "Europe/Ljubljana", "Europe/Bratislava", "Europe/Luxembourg", "Europe/Monaco", "Europe/Malta", "Europe/Andorra",
		"Europe/Vaduz", "Europe/San_Marino", "Europe/Vatican", "Europe/Tirane", "Europe/Sarajevo", "Europe/Skopje", "Europe/Podgorica",
		"Europe/Gibraltar", "Arctic/Longyearbyen",
	}},
	{"Eastern European Time", "Eastern European Summer Time", []string{
		"Europe/Athens", "Europe/Helsinki", "Europe/Mariehamn", "Europe/Sofia", "Europe/Bucharest", "Europe/Riga", "Europe/Tallinn",
		"Europe/Vilnius", "Europe/Kyiv", "Europe/Chisinau", "Asia/Nicosia", "Asia/Beirut",
	}},
	{"Moscow Standard Time", "", []string{"Europe/Moscow", "Europe/Minsk"}},
	{"Türkiye Time", "", []string{"Europe/Istanbul"}},

	// Middle East and Asia.
	{"Gulf Standard Time", "", []string{"Asia/Dubai", "Asia/Muscat"}},
	{"Arabia Standard Time", "", []string{"Asia/Riyadh", "Asia/Kuwait", "Asia/Qatar", "Asia/Bahrain", "Asia/Baghdad", "Asia/Aden"}},
	{"Israel Standard Time", "Israel Daylight Time", []string{"Asia/Jerusalem"}},
	{"India Standard Time", "", []string{"Asia/Kolkata"}},
	{"Sri Lanka Standard Time", "", []string{"Asia/Colombo"}},
	{"Pakistan Standard Time", "", []string{"Asia/Karachi"}},
	{"Bangladesh Standard Time", "", []string{"Asia/Dhaka"}},
	{"Nepal Time", "", []string{"Asia/Kathmandu"}},
	{"China Standard Time", "", []string{"Asia/Shanghai", "Asia/Macau"}},
	{"Hong Kong Time", "", []string{"Asia/Hong_Kong"}},
	{"Taipei Standard Time", "", []string{"Asia/Taipei"}},
	{"Singapore Standard Time", "", []string{"Asia/Singapore"}},
	{"Malaysia Time", "", []string{"Asia/Kuala_Lumpur"}},
	{"Philippine Standard Time", "", []string{"Asia/Manila"}},
	{"Japan Standard Time", "", []string{"Asia/Tokyo"}},
	{"Korea Standard Time", "", []string{"Asia/Seoul"}},
	{"Indochina Time", "", []string{"Asia/Bangkok", "Asia/Ho_Chi_Minh", "Asia/Phnom_Penh", "Asia/Vientiane"}},
	{"Western Indonesia Time", "", []string{"Asia/Jakarta"}},

	// Oceania.
	{"Australian Eastern Standard Time", "Australian Eastern Daylight Time", []string{"Australia/Sydney"}},
	{"New Zealand Standard Time", "New Zealand Daylight Time", []string{"Pacific/Auckland"}},

	// The Americas.
	{"Eastern Standard Time", "Eastern Daylight Time", []string{"America/New_York", "America/Toronto", "America/Nassau", "America/Port-au-Prince", "America/Grand_Turk"}},
	{"Eastern Standard Time", "", []string{"America/Panama", "America/Jamaica", "America/Cayman"}},
	{"Central Standard Time", "", []string{"America/Mexico_City", "America/Guatemala", "America/Costa_Rica", "America/El_Salvador", "America/Tegucigalpa", "America/Managua", "America/Belize"}},
	{"Atlantic Standard Time", "", []string{
		"America/Puerto_Rico", "America/Santo_Domingo", "America/Barbados", "America/Port_of_Spain", "America/Curacao", "America/Aruba",
		"America/Antigua", "America/Anguilla", "America/Dominica", "America/Grenada", "America/St_Kitts", "America/St_Lucia", "America/St_Vincent",
		"America/Montserrat", "America/Tortola", "America/St_Thomas", "America/Guadeloupe", "America/Martinique", "America/Kralendijk",
		"America/Lower_Princes", "America/Marigot", "America/St_Barthelemy",
	}},
	{"Brasília Time", "", []string{"America/Sao_Paulo"}},
	{"Argentina Time", "", []string{"America/Argentina/Buenos_Aires"}},
	{"Chile Standard Time", "Chile Summer Time", []string{"America/Santiago"}},
	{"Colombia Time", "", []string{"America/Bogota"}},
	{"Peru Time", "", []string{"America/Lima"}},
	{"Venezuela Time", "", []string{"America/Caracas"}},
}

// zoneNames is zoneFamilies by IANA zone: {standard, summer}.
var zoneNames = func() map[string][2]string {
	out := map[string][2]string{}
	for _, f := range zoneFamilies {
		for _, z := range f.zones {
			out[z] = [2]string{f.standard, f.summer}
		}
	}
	return out
}()

// summerTime reports whether the instant is in the zone's summer (daylight
// saving) time: its offset is ahead of the zone's standard offset that year.
// The standard offset is the smaller of the offsets in January and in July,
// which is right in both hemispheres and does not depend on how the zone
// database labels the seasons (Ireland's summer time is its "standard" time
// there).
func summerTime(local time.Time) bool {
	loc, year := local.Location(), local.Year()
	_, january := time.Date(year, time.January, 1, 12, 0, 0, 0, loc).Zone()
	_, july := time.Date(year, time.July, 1, 12, 0, 0, 0, loc).Zone()
	_, now := local.Zone()
	return now > min(january, july)
}

// ZoneName names the time zone of a local time: "Gulf Standard Time",
// "Eastern Daylight Time" in summer and "Eastern Standard Time" in winter,
// or, for a zone the table does not name, its offset and IANA name:
// "UTC+05:45, Asia/Kathmandu". zone is the IANA name local was converted with.
func ZoneName(zone string, local time.Time) string {
	if names, ok := zoneNames[zone]; ok {
		if names[1] != "" && summerTime(local) {
			return names[1]
		}
		return names[0]
	}
	_, offset := local.Zone()
	sign := '+'
	if offset < 0 {
		sign, offset = '-', -offset
	}
	return fmt.Sprintf("UTC%c%02d:%02d, %s", sign, offset/3600, offset%3600/60, zone)
}
