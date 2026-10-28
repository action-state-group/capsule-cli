package cli

import (
	"fmt"
	"time"
)

// A profile's clock_tolerance setting. The plugin that keeps the profile's log
// applies it; the core validates the setting with the rest of the profile.

// defaultClockTolerance is how far ahead of this clock a book's last commit
// time may be before the book is refused for writing. maxClockTolerance
// caps it: while the last record is ahead of the clock, every new record is
// pinned to its time, so the tolerance bounds how far a record can be pinned
// -- at most an hour, which can still carry a record across a period's end.
const (
	defaultClockTolerance = 5 * time.Minute
	MaxClockTolerance     = time.Hour
)

// ClockToleranceDuration is the profile's clock_tolerance, or its default.
func (p Profile) ClockToleranceDuration() (time.Duration, error) {
	if p.ClockTolerance == "" {
		return defaultClockTolerance, nil
	}
	d, err := time.ParseDuration(p.ClockTolerance)
	if err != nil || d < 0 || d > MaxClockTolerance {
		return 0, Hint(ErrInput, fmt.Sprintf("clock_tolerance must be a duration from 0 to %s, such as 5m; it bounds how long a span of records can be pinned to one time, so it is not a way to keep writing through a clock jump", MaxClockTolerance))
	}
	return d, nil
}
