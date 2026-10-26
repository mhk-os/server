package lobby

import "time"

// Clock is the time source of lobby timers; tests replace it with a manual clock.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f on its own goroutine after d.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending AfterFunc.
type Timer interface {
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
