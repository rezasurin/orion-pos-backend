package kernel

import (
	"testing"
	"time"
)

func TestBusinessDate(t *testing.T) {
	wib, _ := LocationFor("Asia/Jakarta")
	wit, _ := LocationFor("Asia/Jayapura")
	date := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	at := func(loc *time.Location, y int, m time.Month, d, h, min int) time.Time {
		return time.Date(y, m, d, h, min, 0, 0, loc)
	}

	tests := []struct {
		name   string
		t      time.Time
		loc    *time.Location
		cutoff time.Duration
		want   time.Time
	}{
		{"midday, no cutoff", at(wib, 2026, 10, 2, 12, 0), wib, 0, date(2026, 10, 2)},
		{"just after midnight, no cutoff", at(wib, 2026, 10, 3, 0, 1), wib, 0, date(2026, 10, 3)},
		{"last minute of the day", at(wib, 2026, 10, 2, 23, 59), wib, 0, date(2026, 10, 2)},
		{"before a 4am cutoff belongs to the day before", at(wib, 2026, 10, 3, 2, 30), wib, 4 * time.Hour, date(2026, 10, 2)},
		{"exactly at the cutoff starts the new day", at(wib, 2026, 10, 3, 4, 0), wib, 4 * time.Hour, date(2026, 10, 3)},
		{"one minute before the cutoff", at(wib, 2026, 10, 3, 3, 59), wib, 4 * time.Hour, date(2026, 10, 2)},
		{"cutoff crosses a month end", at(wib, 2026, 11, 1, 1, 0), wib, 3 * time.Hour, date(2026, 10, 31)},
		{"cutoff crosses a year end", at(wib, 2027, 1, 1, 0, 30), wib, 3 * time.Hour, date(2026, 12, 31)},
		// 17:30 UTC is 00:30 the next day in Jakarta: the date follows the outlet, not UTC.
		{"UTC evening is already tomorrow in WIB", time.Date(2026, 10, 2, 17, 30, 0, 0, time.UTC), wib, 0, date(2026, 10, 3)},
		{"15:30 UTC is already tomorrow in WIT", time.Date(2026, 10, 2, 15, 30, 0, 0, time.UTC), wit, 0, date(2026, 10, 3)},
		{"but still today in WIB", time.Date(2026, 10, 2, 15, 30, 0, 0, time.UTC), wib, 0, date(2026, 10, 2)},
		{"leap day", at(wib, 2028, 2, 29, 23, 0), wib, 0, date(2028, 2, 29)},
		{"after the leap day", at(wib, 2028, 3, 1, 1, 0), wib, 2 * time.Hour, date(2028, 2, 29)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BusinessDate(tt.t, tt.loc, tt.cutoff); !got.Equal(tt.want) {
				t.Errorf("BusinessDate = %v, want %v", got.Format(time.DateOnly), tt.want.Format(time.DateOnly))
			}
		})
	}
}

// A device whose clock is hours off still lands on the date its own clock says, in the outlet's
// time zone: the date comes from device_time, never from when the server received the event.
func TestBusinessDateFollowsTheDeviceClock(t *testing.T) {
	wib, _ := LocationFor("Asia/Jakarta")
	serverNow := time.Date(2026, 10, 3, 9, 0, 0, 0, wib)
	deviceFiveHoursBehind := serverNow.Add(-5 * time.Hour) // the sale was rung up at 04:00 by the device
	if got := BusinessDate(deviceFiveHoursBehind, wib, 0); !got.Equal(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v", got)
	}
	deviceTenHoursBehind := serverNow.Add(-10 * time.Hour) // 23:00 the day before
	if got := BusinessDate(deviceTenHoursBehind, wib, 0); !got.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v", got)
	}
}

func TestLocationFor(t *testing.T) {
	if _, ok := LocationFor("Europe/Paris"); ok {
		t.Error("an unsupported zone was accepted")
	}
	for tz, off := range map[string]int{"Asia/Jakarta": 7, "Asia/Makassar": 8, "Asia/Jayapura": 9} {
		loc, ok := LocationFor(tz)
		if !ok {
			t.Fatalf("%s unsupported", tz)
		}
		if _, got := time.Date(2026, 1, 1, 0, 0, 0, 0, loc).Zone(); got != off*3600 {
			t.Errorf("%s offset = %d", tz, got)
		}
	}
}
