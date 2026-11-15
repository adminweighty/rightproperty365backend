package leases

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Variable is one placeholder a template may use: {{name}}.
type Variable struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Example     string `json:"example"`
}

// Variables is the fixed, documented set of placeholders. A template that
// uses any other {{name}} is refused (VALIDATION_FAILED).
func Variables() []Variable {
	return []Variable{
		{"reference", "The lease's reference", "RPL-2026-000123"},
		{"agreement_date", "The date the lease was prepared (the last draft) or sent for signing", "14 November 2026"},
		{"landlord_names", "Every landlord's name, joined with commas and \"and\"", "Lloyd Landlord and Grace Moyo"},
		{"tenant_names", "Every tenant's name, joined the same way", "Tariro Tenant"},
		{"landlord_details", "One line per landlord: name and email address", "Lloyd Landlord (landlord@example.test)"},
		{"tenant_details", "One line per tenant: name and email address", "Tariro Tenant (tenant@example.test)"},
		{"property_address", "The property's address", "12 Sample Road, Avondale, Harare, Zimbabwe"},
		{"country_name", "The country of the property (the template's country)", "Zimbabwe"},
		{"lease_kind", "Residential or commercial", "residential"},
		{"start_date", "The first day of the lease", "1 December 2026"},
		{"end_date", "The last day of the lease, or a note that it is periodic", "30 November 2027"},
		{"term", "The whole term in words", "from 1 December 2026 to 30 November 2027"},
		{"rent_amount", "The rent, two decimals with thousands separators", "950.00"},
		{"rent_currency", "The rent's currency code", "USD"},
		{"rent_period", "The rent period in words: month, week or year", "month"},
		{"payment_day", "When the rent is due", "the 1st day of each month"},
		{"deposit_amount", "The deposit with its currency, or \"no deposit\"", "USD 950.00"},
		{"notice_period_days", "The notice period in days", "30"},
		{"special_conditions", "The special conditions, or \"None.\"", "None."},
	}
}

// VariableNames returns the names of Variables.
func VariableNames() []string {
	names := make([]string, 0, len(Variables()))
	for _, v := range Variables() {
		names = append(names, v.Name)
	}
	return names
}

var placeholderRe = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// UnknownPlaceholders returns the placeholders of text that are not in the
// variable set, and whether the braces are balanced ({{ without }}).
func UnknownPlaceholders(text string) (unknown []string, malformed bool) {
	known := VariableNames()
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(known, m[1]) && !slices.Contains(unknown, m[1]) {
			unknown = append(unknown, m[1])
		}
	}
	rest := placeholderRe.ReplaceAllString(text, "")
	return unknown, strings.Contains(rest, "{{") || strings.Contains(rest, "}}")
}

// Fill replaces every placeholder of text with its value.
func Fill(text string, values map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
		name := placeholderRe.FindStringSubmatch(m)[1]
		if v, ok := values[name]; ok {
			return v
		}
		return m
	})
}

// LongDate writes a YYYY-MM-DD date as "1 December 2026".
func LongDate(raw string) string {
	t, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		return raw
	}
	return t.Format("2 January 2006")
}

// FormatAmount writes a decimal string with two decimals and thousands
// separators: "1250.5" -> "1,250.50".
func FormatAmount(raw string) string {
	whole, frac, _ := strings.Cut(raw, ".")
	for len(frac) < 2 {
		frac += "0"
	}
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String() + "." + frac[:2]
}

func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

var weekdays = []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// PaymentDayText writes when the rent is due.
func PaymentDayText(period string, day int) string {
	switch period {
	case PeriodWeekly:
		if day >= 1 && day <= 7 {
			return weekdays[day-1] + " of each week"
		}
	case PeriodAnnually:
		return "the " + ordinal(day) + " day of the month in which each year of the lease begins"
	}
	return "the " + ordinal(day) + " day of each month"
}

var periodWords = map[string]string{PeriodMonthly: "month", PeriodWeekly: "week", PeriodAnnually: "year"}

// JoinNames writes "A", "A and B", "A, B and C".
func JoinNames(parties []Party) string {
	names := make([]string, 0, len(parties))
	for _, p := range parties {
		names = append(names, p.Name)
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func partyLines(parties []Party) string {
	lines := make([]string, 0, len(parties))
	for _, p := range parties {
		lines = append(lines, fmt.Sprintf("%s (%s)", p.Name, p.Email))
	}
	return strings.Join(lines, "\n")
}

// Values computes every variable of a lease.
func Values(in Input, reference, countryName, leaseKind, agreementDate string) map[string]string {
	end := "no fixed end date (the lease continues from period to period until it is ended by notice)"
	term := "from " + LongDate(in.StartDate) + ", continuing from period to period until it is ended by notice"
	if !in.Periodic && in.EndDate != "" {
		end = LongDate(in.EndDate)
		term = "from " + LongDate(in.StartDate) + " to " + LongDate(in.EndDate)
	}
	deposit := "no deposit"
	if in.DepositAmount != "" {
		deposit = in.RentCurrency + " " + FormatAmount(in.DepositAmount)
	}
	special := strings.TrimSpace(in.SpecialConditions)
	if special == "" {
		special = "None."
	}
	return map[string]string{
		"reference":          reference,
		"agreement_date":     LongDate(agreementDate),
		"landlord_names":     JoinNames(in.Landlords),
		"tenant_names":       JoinNames(in.Tenants),
		"landlord_details":   partyLines(in.Landlords),
		"tenant_details":     partyLines(in.Tenants),
		"property_address":   in.PropertyAddress,
		"country_name":       countryName,
		"lease_kind":         strings.ToLower(leaseKind),
		"start_date":         LongDate(in.StartDate),
		"end_date":           end,
		"term":               term,
		"rent_amount":        FormatAmount(in.RentAmount),
		"rent_currency":      in.RentCurrency,
		"rent_period":        periodWords[in.RentPeriod],
		"payment_day":        PaymentDayText(in.RentPeriod, in.PaymentDay),
		"deposit_amount":     deposit,
		"notice_period_days": strconv.Itoa(in.NoticePeriodDays),
		"special_conditions": special,
	}
}
