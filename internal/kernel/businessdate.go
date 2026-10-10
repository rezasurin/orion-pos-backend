package kernel

import "time"

// Indonesian time zones. None has daylight saving time, so a fixed offset is exact and needs no
// tzdata in the binary.
var zoneOffsets = map[string]*time.Location{
	"Asia/Jakarta":  time.FixedZone("WIB", 7*3600),
	"Asia/Makassar": time.FixedZone("WITA", 8*3600),
	"Asia/Jayapura": time.FixedZone("WIT", 9*3600),
}

// LocationFor returns the time zone for one of the three supported outlet time zones, and false
// for anything else.
func LocationFor(tz string) (*time.Location, bool) {
	loc, ok := zoneOffsets[tz]
	return loc, ok
}

// BusinessDate is the business day an instant belongs to (BACKEND_PLAN.md section 4.9): its
// calendar date in the outlet's time zone, or the day before when it falls before the day's
// cutoff, so with a cutoff of 04:00 a sale at 02:30 counts towards the previous day. The result is
// midnight UTC of that date, the form a database date column takes.
//
// It is computed once, when a sale is projected, from the device's own clock, and stored; changing
// an outlet's time zone or cutoff later does not move history.
func BusinessDate(t time.Time, loc *time.Location, cutoff time.Duration) time.Time {
	y, m, d := t.In(loc).Add(-cutoff).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
