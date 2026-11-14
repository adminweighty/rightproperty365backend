package mail

import (
	"testing"
	"time"
)

// Appointment and other emails name the time zone of any country's default
// zone correctly: by season where the clocks change, and never by a name
// that is wrong for the instant.
func TestLocalDateTimeNamesAnyZone(t *testing.T) {
	winter := time.Date(2027, 1, 12, 10, 0, 0, 0, time.UTC)
	summer := time.Date(2027, 7, 13, 10, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		at   time.Time
		zone string
		want string
	}{
		{winter, "Asia/Dubai", "Tuesday 12 January 2027, 14:00 (Gulf Standard Time)"},
		{summer, "Asia/Dubai", "Tuesday 13 July 2027, 14:00 (Gulf Standard Time)"},
		{winter, "America/New_York", "Tuesday 12 January 2027, 05:00 (Eastern Standard Time)"},
		{summer, "America/New_York", "Tuesday 13 July 2027, 06:00 (Eastern Daylight Time)"},
		{winter, "Asia/Kolkata", "Tuesday 12 January 2027, 15:30 (India Standard Time)"},
		{summer, "Asia/Kolkata", "Tuesday 13 July 2027, 15:30 (India Standard Time)"},
		{winter, "Europe/London", "Tuesday 12 January 2027, 10:00 (Greenwich Mean Time)"},
		{summer, "Europe/London", "Tuesday 13 July 2027, 11:00 (British Summer Time)"},
		{winter, "Africa/Harare", "Tuesday 12 January 2027, 12:00 (Central Africa Time)"},
		{summer, "Europe/Paris", "Tuesday 13 July 2027, 12:00 (Central European Summer Time)"},
		{winter, "Europe/Paris", "Tuesday 12 January 2027, 11:00 (Central European Time)"},
		// The zone database calls Ireland's summer time its standard time;
		// the name still follows the season.
		{winter, "Europe/Dublin", "Tuesday 12 January 2027, 10:00 (Greenwich Mean Time)"},
		{summer, "Europe/Dublin", "Tuesday 13 July 2027, 11:00 (Irish Standard Time)"},
		// The southern hemisphere: summer is in January.
		{winter, "Australia/Sydney", "Tuesday 12 January 2027, 21:00 (Australian Eastern Daylight Time)"},
		{summer, "Australia/Sydney", "Tuesday 13 July 2027, 20:00 (Australian Eastern Standard Time)"},
		{summer, "Indian/Mauritius", "Tuesday 13 July 2027, 14:00 (Mauritius Time)"},
		{summer, "Africa/Nairobi", "Tuesday 13 July 2027, 13:00 (East Africa Time)"},
		// A zone without a name in the table: its offset and IANA name.
		{summer, "Pacific/Tongatapu", "Tuesday 13 July 2027, 23:00 (UTC+13:00, Pacific/Tongatapu)"},
		{summer, "America/St_Johns", "Tuesday 13 July 2027, 07:30 (UTC-02:30, America/St_Johns)"},
		{winter, "America/St_Johns", "Tuesday 12 January 2027, 06:30 (UTC-03:30, America/St_Johns)"},
		{summer, "", "Tuesday 13 July 2027, 10:00 (Coordinated Universal Time)"},
		{summer, "Not/AZone", "Tuesday 13 July 2027, 10:00 (Coordinated Universal Time)"},
	} {
		if got := LocalDateTime(tt.at, tt.zone); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.zone, got, tt.want)
		}
	}
	// Every zone the table names exists.
	for zone := range zoneNames {
		if _, err := time.LoadLocation(zone); err != nil {
			t.Errorf("zone %s in the name table does not load: %v", zone, err)
		}
	}
}
