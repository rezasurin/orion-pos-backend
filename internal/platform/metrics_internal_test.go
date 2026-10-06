package platform

import (
	"testing"
	"time"
)

func TestDailyAtFiresOnceADayInJakarta(t *testing.T) {
	s := dailyAt{hour: 1}
	at := func(v string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", v, MetricsZone)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for from, want := range map[string]string{
		"2026-10-06 00:30": "2026-10-06 01:00",
		"2026-10-06 01:00": "2026-10-07 01:00",
		"2026-10-06 23:59": "2026-10-07 01:00",
	} {
		if got := s.Next(at(from).UTC()); !got.Equal(at(want)) {
			t.Errorf("Next(%s) = %s, want %s", from, got.In(MetricsZone), want)
		}
	}
}
