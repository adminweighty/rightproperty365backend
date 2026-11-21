package markets

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// countryTable is every officially assigned ISO 3166-1 alpha-2 code with an
// English short name, the country's ISO 4217 currency and a default IANA
// time zone: the zone of the capital.
//
// Origin: compiled by hand from the ISO 3166-1 code list, the ISO 4217
// currency list and the IANA time zone database (zone1970.tab). It is data in
// the repository and is never fetched at run time. Names are the short names
// people use ("Russia", "South Korea", "Bolivia"), not ISO's inverted forms.
// Tests check the count, the shape of every code and currency and that every
// zone loads; the table has NOT been reconciled line by line against the
// standards, so check a row before relying on it for anything legal.
//
// Things to know:
//
//   - ONE zone per country. Countries with several zones (the United States,
//     Canada, Russia, Australia, Brazil, Indonesia, ...) will need a time zone
//     per city before appointments or stays there are shown in local time.
//   - ONE currency per country: the legal tender most prices are quoted in.
//     Zimbabwe stays on USD, as it was seeded from the start (migration 0001).
//   - Territories without a currency of their own carry the currency used
//     there; uninhabited ones (AQ, BV, HM) carry a nominal one.
//
// Format: code|name|currency|zone, one per line, ordered by code.
const countryTable = `
AD|Andorra|EUR|Europe/Andorra
AE|United Arab Emirates|AED|Asia/Dubai
AF|Afghanistan|AFN|Asia/Kabul
AG|Antigua and Barbuda|XCD|America/Antigua
AI|Anguilla|XCD|America/Anguilla
AL|Albania|ALL|Europe/Tirane
AM|Armenia|AMD|Asia/Yerevan
AO|Angola|AOA|Africa/Luanda
AQ|Antarctica|USD|Antarctica/McMurdo
AR|Argentina|ARS|America/Argentina/Buenos_Aires
AS|American Samoa|USD|Pacific/Pago_Pago
AT|Austria|EUR|Europe/Vienna
AU|Australia|AUD|Australia/Sydney
AW|Aruba|AWG|America/Aruba
AX|Åland Islands|EUR|Europe/Mariehamn
AZ|Azerbaijan|AZN|Asia/Baku
BA|Bosnia and Herzegovina|BAM|Europe/Sarajevo
BB|Barbados|BBD|America/Barbados
BD|Bangladesh|BDT|Asia/Dhaka
BE|Belgium|EUR|Europe/Brussels
BF|Burkina Faso|XOF|Africa/Ouagadougou
BG|Bulgaria|EUR|Europe/Sofia
BH|Bahrain|BHD|Asia/Bahrain
BI|Burundi|BIF|Africa/Bujumbura
BJ|Benin|XOF|Africa/Porto-Novo
BL|Saint Barthélemy|EUR|America/St_Barthelemy
BM|Bermuda|BMD|Atlantic/Bermuda
BN|Brunei|BND|Asia/Brunei
BO|Bolivia|BOB|America/La_Paz
BQ|Bonaire, Sint Eustatius and Saba|USD|America/Kralendijk
BR|Brazil|BRL|America/Sao_Paulo
BS|Bahamas|BSD|America/Nassau
BT|Bhutan|BTN|Asia/Thimphu
BV|Bouvet Island|NOK|UTC
BW|Botswana|BWP|Africa/Gaborone
BY|Belarus|BYN|Europe/Minsk
BZ|Belize|BZD|America/Belize
CA|Canada|CAD|America/Toronto
CC|Cocos (Keeling) Islands|AUD|Indian/Cocos
CD|Congo (Democratic Republic)|CDF|Africa/Kinshasa
CF|Central African Republic|XAF|Africa/Bangui
CG|Congo|XAF|Africa/Brazzaville
CH|Switzerland|CHF|Europe/Zurich
CI|Côte d'Ivoire|XOF|Africa/Abidjan
CK|Cook Islands|NZD|Pacific/Rarotonga
CL|Chile|CLP|America/Santiago
CM|Cameroon|XAF|Africa/Douala
CN|China|CNY|Asia/Shanghai
CO|Colombia|COP|America/Bogota
CR|Costa Rica|CRC|America/Costa_Rica
CU|Cuba|CUP|America/Havana
CV|Cabo Verde|CVE|Atlantic/Cape_Verde
CW|Curaçao|XCG|America/Curacao
CX|Christmas Island|AUD|Indian/Christmas
CY|Cyprus|EUR|Asia/Nicosia
CZ|Czechia|CZK|Europe/Prague
DE|Germany|EUR|Europe/Berlin
DJ|Djibouti|DJF|Africa/Djibouti
DK|Denmark|DKK|Europe/Copenhagen
DM|Dominica|XCD|America/Dominica
DO|Dominican Republic|DOP|America/Santo_Domingo
DZ|Algeria|DZD|Africa/Algiers
EC|Ecuador|USD|America/Guayaquil
EE|Estonia|EUR|Europe/Tallinn
EG|Egypt|EGP|Africa/Cairo
EH|Western Sahara|MAD|Africa/El_Aaiun
ER|Eritrea|ERN|Africa/Asmara
ES|Spain|EUR|Europe/Madrid
ET|Ethiopia|ETB|Africa/Addis_Ababa
FI|Finland|EUR|Europe/Helsinki
FJ|Fiji|FJD|Pacific/Fiji
FK|Falkland Islands|FKP|Atlantic/Stanley
FM|Micronesia|USD|Pacific/Pohnpei
FO|Faroe Islands|DKK|Atlantic/Faroe
FR|France|EUR|Europe/Paris
GA|Gabon|XAF|Africa/Libreville
GB|United Kingdom|GBP|Europe/London
GD|Grenada|XCD|America/Grenada
GE|Georgia|GEL|Asia/Tbilisi
GF|French Guiana|EUR|America/Cayenne
GG|Guernsey|GBP|Europe/Guernsey
GH|Ghana|GHS|Africa/Accra
GI|Gibraltar|GIP|Europe/Gibraltar
GL|Greenland|DKK|America/Nuuk
GM|Gambia|GMD|Africa/Banjul
GN|Guinea|GNF|Africa/Conakry
GP|Guadeloupe|EUR|America/Guadeloupe
GQ|Equatorial Guinea|XAF|Africa/Malabo
GR|Greece|EUR|Europe/Athens
GS|South Georgia and the South Sandwich Islands|GBP|Atlantic/South_Georgia
GT|Guatemala|GTQ|America/Guatemala
GU|Guam|USD|Pacific/Guam
GW|Guinea-Bissau|XOF|Africa/Bissau
GY|Guyana|GYD|America/Guyana
HK|Hong Kong|HKD|Asia/Hong_Kong
HM|Heard Island and McDonald Islands|AUD|Indian/Kerguelen
HN|Honduras|HNL|America/Tegucigalpa
HR|Croatia|EUR|Europe/Zagreb
HT|Haiti|HTG|America/Port-au-Prince
HU|Hungary|HUF|Europe/Budapest
ID|Indonesia|IDR|Asia/Jakarta
IE|Ireland|EUR|Europe/Dublin
IL|Israel|ILS|Asia/Jerusalem
IM|Isle of Man|GBP|Europe/Isle_of_Man
IN|India|INR|Asia/Kolkata
IO|British Indian Ocean Territory|USD|Indian/Chagos
IQ|Iraq|IQD|Asia/Baghdad
IR|Iran|IRR|Asia/Tehran
IS|Iceland|ISK|Atlantic/Reykjavik
IT|Italy|EUR|Europe/Rome
JE|Jersey|GBP|Europe/Jersey
JM|Jamaica|JMD|America/Jamaica
JO|Jordan|JOD|Asia/Amman
JP|Japan|JPY|Asia/Tokyo
KE|Kenya|KES|Africa/Nairobi
KG|Kyrgyzstan|KGS|Asia/Bishkek
KH|Cambodia|KHR|Asia/Phnom_Penh
KI|Kiribati|AUD|Pacific/Tarawa
KM|Comoros|KMF|Indian/Comoro
KN|Saint Kitts and Nevis|XCD|America/St_Kitts
KP|North Korea|KPW|Asia/Pyongyang
KR|South Korea|KRW|Asia/Seoul
KW|Kuwait|KWD|Asia/Kuwait
KY|Cayman Islands|KYD|America/Cayman
KZ|Kazakhstan|KZT|Asia/Almaty
LA|Laos|LAK|Asia/Vientiane
LB|Lebanon|LBP|Asia/Beirut
LC|Saint Lucia|XCD|America/St_Lucia
LI|Liechtenstein|CHF|Europe/Vaduz
LK|Sri Lanka|LKR|Asia/Colombo
LR|Liberia|LRD|Africa/Monrovia
LS|Lesotho|LSL|Africa/Maseru
LT|Lithuania|EUR|Europe/Vilnius
LU|Luxembourg|EUR|Europe/Luxembourg
LV|Latvia|EUR|Europe/Riga
LY|Libya|LYD|Africa/Tripoli
MA|Morocco|MAD|Africa/Casablanca
MC|Monaco|EUR|Europe/Monaco
MD|Moldova|MDL|Europe/Chisinau
ME|Montenegro|EUR|Europe/Podgorica
MF|Saint Martin (French part)|EUR|America/Marigot
MG|Madagascar|MGA|Indian/Antananarivo
MH|Marshall Islands|USD|Pacific/Majuro
MK|North Macedonia|MKD|Europe/Skopje
ML|Mali|XOF|Africa/Bamako
MM|Myanmar|MMK|Asia/Yangon
MN|Mongolia|MNT|Asia/Ulaanbaatar
MO|Macao|MOP|Asia/Macau
MP|Northern Mariana Islands|USD|Pacific/Saipan
MQ|Martinique|EUR|America/Martinique
MR|Mauritania|MRU|Africa/Nouakchott
MS|Montserrat|XCD|America/Montserrat
MT|Malta|EUR|Europe/Malta
MU|Mauritius|MUR|Indian/Mauritius
MV|Maldives|MVR|Indian/Maldives
MW|Malawi|MWK|Africa/Blantyre
MX|Mexico|MXN|America/Mexico_City
MY|Malaysia|MYR|Asia/Kuala_Lumpur
MZ|Mozambique|MZN|Africa/Maputo
NA|Namibia|NAD|Africa/Windhoek
NC|New Caledonia|XPF|Pacific/Noumea
NE|Niger|XOF|Africa/Niamey
NF|Norfolk Island|AUD|Pacific/Norfolk
NG|Nigeria|NGN|Africa/Lagos
NI|Nicaragua|NIO|America/Managua
NL|Netherlands|EUR|Europe/Amsterdam
NO|Norway|NOK|Europe/Oslo
NP|Nepal|NPR|Asia/Kathmandu
NR|Nauru|AUD|Pacific/Nauru
NU|Niue|NZD|Pacific/Niue
NZ|New Zealand|NZD|Pacific/Auckland
OM|Oman|OMR|Asia/Muscat
PA|Panama|PAB|America/Panama
PE|Peru|PEN|America/Lima
PF|French Polynesia|XPF|Pacific/Tahiti
PG|Papua New Guinea|PGK|Pacific/Port_Moresby
PH|Philippines|PHP|Asia/Manila
PK|Pakistan|PKR|Asia/Karachi
PL|Poland|PLN|Europe/Warsaw
PM|Saint Pierre and Miquelon|EUR|America/Miquelon
PN|Pitcairn|NZD|Pacific/Pitcairn
PR|Puerto Rico|USD|America/Puerto_Rico
PS|Palestine|ILS|Asia/Hebron
PT|Portugal|EUR|Europe/Lisbon
PW|Palau|USD|Pacific/Palau
PY|Paraguay|PYG|America/Asuncion
QA|Qatar|QAR|Asia/Qatar
RE|Réunion|EUR|Indian/Reunion
RO|Romania|RON|Europe/Bucharest
RS|Serbia|RSD|Europe/Belgrade
RU|Russia|RUB|Europe/Moscow
RW|Rwanda|RWF|Africa/Kigali
SA|Saudi Arabia|SAR|Asia/Riyadh
SB|Solomon Islands|SBD|Pacific/Guadalcanal
SC|Seychelles|SCR|Indian/Mahe
SD|Sudan|SDG|Africa/Khartoum
SE|Sweden|SEK|Europe/Stockholm
SG|Singapore|SGD|Asia/Singapore
SH|Saint Helena, Ascension and Tristan da Cunha|SHP|Atlantic/St_Helena
SI|Slovenia|EUR|Europe/Ljubljana
SJ|Svalbard and Jan Mayen|NOK|Arctic/Longyearbyen
SK|Slovakia|EUR|Europe/Bratislava
SL|Sierra Leone|SLE|Africa/Freetown
SM|San Marino|EUR|Europe/San_Marino
SN|Senegal|XOF|Africa/Dakar
SO|Somalia|SOS|Africa/Mogadishu
SR|Suriname|SRD|America/Paramaribo
SS|South Sudan|SSP|Africa/Juba
ST|Sao Tome and Principe|STN|Africa/Sao_Tome
SV|El Salvador|USD|America/El_Salvador
SX|Sint Maarten (Dutch part)|XCG|America/Lower_Princes
SY|Syria|SYP|Asia/Damascus
SZ|Eswatini|SZL|Africa/Mbabane
TC|Turks and Caicos Islands|USD|America/Grand_Turk
TD|Chad|XAF|Africa/Ndjamena
TF|French Southern Territories|EUR|Indian/Kerguelen
TG|Togo|XOF|Africa/Lome
TH|Thailand|THB|Asia/Bangkok
TJ|Tajikistan|TJS|Asia/Dushanbe
TK|Tokelau|NZD|Pacific/Fakaofo
TL|Timor-Leste|USD|Asia/Dili
TM|Turkmenistan|TMT|Asia/Ashgabat
TN|Tunisia|TND|Africa/Tunis
TO|Tonga|TOP|Pacific/Tongatapu
TR|Türkiye|TRY|Europe/Istanbul
TT|Trinidad and Tobago|TTD|America/Port_of_Spain
TV|Tuvalu|AUD|Pacific/Funafuti
TW|Taiwan|TWD|Asia/Taipei
TZ|Tanzania|TZS|Africa/Dar_es_Salaam
UA|Ukraine|UAH|Europe/Kyiv
UG|Uganda|UGX|Africa/Kampala
UM|United States Minor Outlying Islands|USD|Pacific/Wake
US|United States|USD|America/New_York
UY|Uruguay|UYU|America/Montevideo
UZ|Uzbekistan|UZS|Asia/Tashkent
VA|Vatican City|EUR|Europe/Vatican
VC|Saint Vincent and the Grenadines|XCD|America/St_Vincent
VE|Venezuela|VES|America/Caracas
VG|British Virgin Islands|USD|America/Tortola
VI|U.S. Virgin Islands|USD|America/St_Thomas
VN|Vietnam|VND|Asia/Ho_Chi_Minh
VU|Vanuatu|VUV|Pacific/Efate
WF|Wallis and Futuna|XPF|Pacific/Wallis
WS|Samoa|WST|Pacific/Apia
YE|Yemen|YER|Asia/Aden
YT|Mayotte|EUR|Indian/Mayotte
ZA|South Africa|ZAR|Africa/Johannesburg
ZM|Zambia|ZMW|Africa/Lusaka
ZW|Zimbabwe|USD|Africa/Harare
`

// CountryCount is the number of officially assigned ISO 3166-1 alpha-2 codes.
const CountryCount = 249

// fixedIDs are the IDs of the five countries that existed before the full
// list (migrations 0001 and 0013). They never change: organisations,
// listings, profiles and reference rows point at them.
var fixedIDs = map[string]uuid.UUID{
	"ZW": ZimbabweID,
	"BW": uuid.MustParse("0195c0de-0000-7000-8000-00000000a002"),
	"ZA": uuid.MustParse("0195c0de-0000-7000-8000-00000000a003"),
	"AE": uuid.MustParse("0195c0de-0000-7000-8000-00000000a004"),
	"GB": UnitedKingdomID,
}

// sortOrders keep the two countries the platform started in at the top of
// the unfiltered list; every other country has DefaultSortOrder and follows
// by name.
var sortOrders = map[string]int{"ZW": 10, "GB": 20}

// DefaultSortOrder is the sort_order of a country without a place of its own.
const DefaultSortOrder = 100

// IDFor returns the fixed ID of a country: one of the five original IDs, or
// an ID derived from the code (…-0000c0de<hex of the two letters>), which is
// what migration 0014 inserts. A row that already existed under another ID
// keeps it in the database, so SQL should look a country up by its code.
func IDFor(code string) uuid.UUID {
	if id, ok := fixedIDs[code]; ok {
		return id
	}
	if len(code) != 2 {
		return uuid.Nil
	}
	return uuid.MustParse(fmt.Sprintf("0195c0de-0000-7000-8000-0000c0de%02x%02x", code[0], code[1]))
}

// Seed returns every country, ordered as the API lists them: by sort order
// (Zimbabwe, then the United Kingdom), then by name. All are enabled.
func Seed() []Market {
	lines := strings.Split(strings.TrimSpace(countryTable), "\n")
	out := make([]Market, 0, len(lines))
	for _, line := range lines {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 4 {
			panic("markets: malformed country row: " + line)
		}
		m := Market{
			ID: IDFor(parts[0]), Code: parts[0], Name: parts[1], CurrencyCode: parts[2], Timezone: parts[3],
			Status: StatusActive, Enabled: true, SortOrder: DefaultSortOrder, AdminStatus: AdminActive, ExtraCurrencies: []string{},
		}
		if n, ok := sortOrders[m.Code]; ok {
			m.SortOrder = n
		}
		out = append(out, m)
	}
	Order(out)
	return out
}

// MigrationRow is the statement migration 0014 holds for a country: an
// upsert that keeps an existing row's ID, display order and `enabled` flag.
// A test keeps the migration and the table above in step.
func MigrationRow(m Market) string {
	return fmt.Sprintf(`INSERT INTO markets (id, code, name, currency_code, timezone, status, sort_order) VALUES ('%s', '%s', '%s', '%s', '%s', 'ACTIVE', %d) ON CONFLICT (code) DO UPDATE SET name = EXCLUDED.name, currency_code = EXCLUDED.currency_code, timezone = EXCLUDED.timezone, status = 'ACTIVE', updated_at = now();`,
		m.ID, m.Code, strings.ReplaceAll(m.Name, "'", "''"), m.CurrencyCode, m.Timezone, m.SortOrder)
}
