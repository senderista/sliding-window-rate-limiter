package ratelimit

import (
    "fmt"
    "errors"
    "sync"
    "time"
)

type slidingWindow struct {
    intervalSec int
    subintervalSec int
    startTime time.Time
    lastUpdateTime time.Time
    currentBucket int
    buckets []int
}

// Returns a new sliding window state with total interval length `intervalSec` and subintervals of length `subintervalSec`.
// We assume all methods on this type are protected by a mutex in the owner.
func newState(intervalSec int, subintervalSec int) (state *slidingWindow, err error) {
    // the window subinterval needs to evenly divide the interval, since we have an integral number of buckets
    if intervalSec % subintervalSec != 0 {
        return nil, errors.New("subinterval must evenly divide interval")
    }
    // We need an extra bucket in order to store a full interval of data when the current time is not on the subinterval boundary.
    // Note that we estimate the counts in the trailing bucket by weighting them by the trailing subinterval's adjusted length:
    // e.g. for a 1-minute interval with a single subinterval, we would use 2 buckets, one to represent the current minute and
    // the other to represent the previous minute. If we are only 30s into the current minute, we estimate the total count as
    // the count in the current minute plus half the count in the previous minute. If we are 45s into the current minute, we
    // estimate the total count as the count in the current minute plus 0.25 times the count in the previous minute, etc.
    // Since this approximation assumes that all counts in the trailing subinterval are uniformly distributed over time, it can
    // lead to both undercounting and overcounting, but is more accurate overall.
    numBuckets := (intervalSec / subintervalSec) + 1
    currentTime := time.Now()
    return &slidingWindow{
        intervalSec: intervalSec,
        subintervalSec: subintervalSec,
        startTime: currentTime,
        lastUpdateTime: currentTime,
        currentBucket: 0,
        buckets: make([]int, numBuckets),
    }, nil
}

// For internal/debugging use only.
func (state *slidingWindow) dump() string {
    return fmt.Sprintf("Current bucket: %d\n", state.currentBucket) + fmt.Sprintf("Buckets: %d\n", state.buckets)
}

func (state *slidingWindow) syncWithCurrentTime(currentTime time.Time) {
    // calculate current bucket index: number of subintervals elapsed since start time, mod number of buckets
    timeSinceStartSec := int(currentTime.Sub(state.startTime).Seconds())
    subintervalsSinceLastSync := timeSinceStartSec / state.subintervalSec
    currentBucket := subintervalsSinceLastSync % len(state.buckets)
    // if last update was > intervalSec ago, then zero out all buckets, otherwise zero out buckets for any subintervals
    // that have elapsed since the last sync.
    timeSinceLastUpdateSec := int(currentTime.Sub(state.lastUpdateTime).Seconds())
    if timeSinceLastUpdateSec > state.intervalSec {
        // zero out all buckets
        for i, _ := range state.buckets {
            state.buckets[i] = 0
        }
    } else {
        // expire old buckets in order until we reach the current bucket index
        cb := state.currentBucket
        for cb != currentBucket {
            cb = (cb + 1) % len(state.buckets)
            state.buckets[cb] = 0
        }
    }
    // finally sync current bucket
    state.currentBucket = currentBucket
}

func (state *slidingWindow) GetTotalEventCount(currentTime time.Time) int {
    state.syncWithCurrentTime(currentTime)
    // find the bucket holding oldest data from up to the full interval ago
    // trailingBucket := nonNegMod((state.currentBucket + 1), len(state.buckets))
    trailingBucket := (state.currentBucket + 1) % len(state.buckets)
    // the corrected total count is the sum of all subinterval counts,
    // but with the trailing subinterval weighted by its overlap with the current (sliding) interval.
    // See comments above for fuller explanation.
    timeSinceStartSec := int(currentTime.Sub(state.startTime).Seconds())
    subintervalRemainingSec := state.subintervalSec - (timeSinceStartSec % state.subintervalSec)
    trailingBucketWeight := float32(subintervalRemainingSec) / float32(state.subintervalSec)
    // we subtract the trailing bucket count and then add its weighted count to get corrected total count
    totalCount := 0
    for i, c := range state.buckets {
        if i != trailingBucket { 
            totalCount += c
        }
    }
    totalCount += int(float32(state.buckets[trailingBucket]) * trailingBucketWeight)
    return totalCount
}

func (state *slidingWindow) RecordEvent(currentTime time.Time) {
    state.syncWithCurrentTime(currentTime)
    state.buckets[state.currentBucket] += 1
    state.lastUpdateTime = currentTime
}

type Limiter struct {
    requestsAllowedInInterval int
    stateMu sync.Mutex
    state *slidingWindow
}

// Returns a new Limiter with allowed rate `requestsAllowedPerSec` and approximate sliding window of length `intervalSec` with resolution `subintervalSec`.
func New(requestsAllowedInInterval int, intervalSec int, subintervalSec int) (limiter *Limiter, err error) {
    // this gives the rate in time.Duration natural units (nanoseconds). to get the rate in the given time unit, we need to multiply by the given unit.
    state, err := newState(intervalSec, subintervalSec)
    if (err != nil) {
        return nil, err
    }
    return &Limiter {
        requestsAllowedInInterval: requestsAllowedInInterval,
        state: state,
    }, nil
}

// Returns whether a new request should be allowed, given the allowed request count per interval. This method is thread-safe.
func (limiter *Limiter) ShouldAllowRequest(key uint64) bool {
    // We need to lock the mutex before doing any reads to ensure we're observing a consistent state.
    // We could use sync.RWMutex to avoid taking an exclusive lock on reads, but then we'd need to spin later to upgrade the lock to exclusive.
    // Also, even "read" methods on our state like GetTotalEventCount() update the current time, so need an exclusive lock.
    limiter.stateMu.Lock()
    defer limiter.stateMu.Unlock()
    currentTime := time.Now()
    requestsInInterval := limiter.state.GetTotalEventCount(currentTime)
    if limiter.requestsAllowedInInterval - requestsInInterval >= 1 {
        limiter.state.RecordEvent(currentTime)
        return true
    }
    return false
}

// For internal testing/debugging only.
func (limiter *Limiter) getRemainingRequestsAllowedInInterval(key uint64) int {
    limiter.stateMu.Lock()
    defer limiter.stateMu.Unlock()
    currentTime := time.Now()
    requestsInInterval := limiter.state.GetTotalEventCount(currentTime)
    return limiter.requestsAllowedInInterval - requestsInInterval
}
