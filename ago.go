package main

import (
	"fmt"
	"time"
)

func agoR(d time.Duration, maxPrec int) (s string) {
	if maxPrec <= 0 {
		return ""
	}
	ranges := []struct {
		lt   time.Duration
		div  time.Duration
		unit string
	}{
		{time.Minute, time.Second, "s"},
		{time.Hour, time.Minute, "m"},
		{2 * day, time.Hour, "h"},
		{month, day, "d"},
		{3 * month, week, "w"},
		{2 * year, month, "mo"},
	}
	div, unit := year, "y"
	for _, r := range ranges {
		if d < r.lt {
			div, unit = r.div, r.unit
			break
		}
	}
	v := int64(d.Seconds() / div.Seconds())
	r := time.Duration(d.Seconds()-float64(v)*div.Seconds()) * time.Second
	tail := ""
	if r > 1*time.Second {
		tail = agoR(r, maxPrec-1)
	}
	return fmt.Sprintf("%2d%-2s%s", v, unit, tail)
}

func ago(d time.Duration, maxPrec int) string {
	s := agoR(d, maxPrec)
	if d > 0*time.Second {
		return s + " ago"
	}
	return "in " + s
}
