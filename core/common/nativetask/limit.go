package nativetask

import (
	"strconv"
	"sync"
	"time"
)

// Customer reads are bounded per API key so a polling loop cannot crowd out
// other customers. Limits are per gateway instance; they protect capacity and
// are not a billing control.
const (
	statusReadsPerSecond = 20
	statusReadBurst      = 40
	// Each download holds a socket and up to bufferedArtifactLimit bytes. The
	// group cap stops one account's many keys from taking every global slot.
	artifactDownloadsPerKey   = 4
	artifactDownloadsPerGroup = 8
	artifactDownloadsGlobal   = 16
)

type readBucket struct {
	tokens float64
	last   time.Time
}

type readLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*readBucket
	active  map[string]int
	groups  map[string]int
	total   int
}

var customerReads = newReadLimiter(time.Now)

// AllowCustomerRead applies the same per-key read limit to other customer
// polling endpoints, such as image task status.
func AllowCustomerRead(group string, token int) bool {
	return customerReads.allowStatus(group, token)
}

func newReadLimiter(now func() time.Time) *readLimiter {
	return &readLimiter{now: now, buckets: map[string]*readBucket{}, active: map[string]int{}, groups: map[string]int{}}
}

func readKey(group string, token int) string { return group + "/" + strconv.Itoa(token) }

// allowStatus is a token bucket: statusReadsPerSecond sustained, statusReadBurst at once.
func (l *readLimiter) allowStatus(group string, token int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	key := readKey(group, token)
	bucket := l.buckets[key]
	if bucket == nil {
		if len(l.buckets) >= 10000 {
			l.evictIdle(now)
		}
		bucket = &readBucket{tokens: statusReadBurst, last: now}
		l.buckets[key] = bucket
	}
	bucket.tokens += now.Sub(bucket.last).Seconds() * statusReadsPerSecond
	if bucket.tokens > statusReadBurst {
		bucket.tokens = statusReadBurst
	}
	bucket.last = now
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// A full bucket that has been idle for a minute carries no state worth keeping.
func (l *readLimiter) evictIdle(now time.Time) {
	for key, bucket := range l.buckets {
		if now.Sub(bucket.last) > time.Minute {
			delete(l.buckets, key)
		}
	}
}

// acquireDownload bounds concurrent artifact downloads per key, per group and overall.
// The returned release must be called exactly once.
func (l *readLimiter) acquireDownload(group string, token int) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := readKey(group, token)
	if l.total >= artifactDownloadsGlobal || l.active[key] >= artifactDownloadsPerKey || l.groups[group] >= artifactDownloadsPerGroup {
		return nil, false
	}
	l.total++
	l.active[key]++
	l.groups[group]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if l.active[key]--; l.active[key] <= 0 {
				delete(l.active, key)
			}
			if l.groups[group]--; l.groups[group] <= 0 {
				delete(l.groups, group)
			}
		})
	}, true
}
