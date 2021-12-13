package main

import (
	"fmt"
	"sort"
	"time"
)

type bucket struct {
	interval time.Duration
	snaps    []*snap
}

func newBucket(interval time.Duration, size int) *bucket {
	if size <= 0 {
		panic("bug: bucket size must be positive")
	}
	return &bucket{
		interval: interval,
		snaps:    make([]*snap, 0, size),
	}
}

func (b bucket) String() string {
	return fmt.Sprintf("%s: %v", b.interval, b.snaps)
}

// cascade is a cascade of buckets which support hierarchical eviction.
type cascade []*bucket

func newCascade() cascade {
	return make(cascade, 0)
}

func (c *cascade) addBucket(b *bucketJSON) {
	*c = append(*c, &bucket{
		interval: time.Duration(*b.Interval),
		snaps:    make([]*snap, 0, *b.Size),
	})
}

// insert puts in snapshots into the top bucket. If that bucket is full, oldest
// snapshots are evicted to lower buckets. Any snapshots which don't fit the
// last bucket are returned in out.
//
// Insertion respect bucket intervals: TODO.
func (c cascade) insert(in []*snap) (out []*snap) {
	sort.Slice(in, func(i, j int) bool {
		return in[i].created.Before(in[j].created)
	})
	var overflow []*snap
	for _, b := range c {
		var prevCreated time.Time
		var insertAt int
		for i, s := range in {
			d := s.created.Sub(prevCreated)
			if (i > 0 && d < b.interval) || cap(b.snaps) == 0 {
				out = append(out, s)
				continue
			}
			b.snaps = b.snaps[0 : insertAt+1]
			if t := b.snaps[insertAt]; t != nil {
				overflow = append(overflow, t)
			}
			b.snaps[insertAt] = s
			insertAt++
			insertAt %= cap(b.snaps)
			prevCreated = s.created
		}
		in = overflow
		overflow = overflow[:0]
	}
	out = append(out, in...)
	return out
}

func (c cascade) reset() {
	for _, b := range c {
		for i := range b.snaps {
			b.snaps[i] = nil
		}
		b.snaps = b.snaps[:0]
	}
}
