package tunnel

import "time"

func timeAfter(d time.Duration) <-chan time.Time {
	return time.After(d)
}
