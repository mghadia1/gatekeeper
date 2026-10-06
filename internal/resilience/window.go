// Package resilience holds the gateway's protections against failing
// backends: a circuit breaker and a retry budget. Both count events over a
// sliding window of the last ten seconds.
package resilience

import "time"

// windowBuckets one-second buckets make the sliding window.
const windowBuckets = 10

// window counts two kinds of events per second over the last ten seconds.
// It is not safe for concurrent use; its owner holds a lock.
type window struct {
	buckets [windowBuckets]struct {
		second int64
		a, b   int
	}
}

func (w *window) bucket(now time.Time) *struct {
	second int64
	a, b   int
} {
	sec := now.Unix()
	bk := &w.buckets[sec%windowBuckets]
	if bk.second != sec { // last used ten or more seconds ago: start over
		bk.second, bk.a, bk.b = sec, 0, 0
	}
	return bk
}

func (w *window) add(now time.Time, a, b int) {
	bk := w.bucket(now)
	bk.a += a
	bk.b += b
}

// totals sums the buckets from the last ten seconds.
func (w *window) totals(now time.Time) (a, b int) {
	oldest := now.Unix() - windowBuckets + 1
	for _, bk := range w.buckets {
		if bk.second >= oldest {
			a += bk.a
			b += bk.b
		}
	}
	return a, b
}

func (w *window) reset() { *w = window{} }
