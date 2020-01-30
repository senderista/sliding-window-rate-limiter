package ratelimit

import (
    "fmt"
    "errors"
    "sync"
    "time"
)

type slidingWindow struct {
    intervalSec int64
    subintervalSec int64
    lastSyncTime time.Time
    currentBucket int64
    trailingBucketCount int
    buckets []int
}

// Returns a new sliding window state with total interval length `intervalSec` and subintervals of length `subintervalSec`.
// We assume all methods on this type are protected by a mutex in the owner.
func newState(intervalSec int64, subintervalSec int64) (state *slidingWindow, err error) {
    // the window subinterval needs to evenly divide the interval, since we have an integral number of buckets
    if intervalSec % subintervalSec != 0 {
        return nil, errors.New("subinterval must evenly divide interval")
    }
    // We use an extra counter in order to store a full interval of data when the current time is not on the subinterval boundary.
    // Note that we estimate the count in this "trailing bucket" by weighting it by the trailing subinterval's adjusted length:
    // e.g. for a 1-minute interval with a single subinterval, we would use 2 buckets, one to represent the current minute and
    // the other to represent the previous minute. If we are only 30s into the current minute, we estimate the total count as
    // the count in the current minute plus half the count in the previous minute. If we are 45s into the current minute, we
    // estimate the total count as the count in the current minute plus 0.25 times the count in the previous minute, etc.
    // Since this approximation assumes that all counts in the trailing subinterval are uniformly distributed over time, it can
    // lead to both undercounting and overcounting, but is more accurate overall.
    numBuckets := (intervalSec / subintervalSec)
    currentTime := time.Now()
    return &slidingWindow{
        intervalSec: intervalSec,
        subintervalSec: subintervalSec,
        lastSyncTime: currentTime,
        currentBucket: (currentTime.Unix() % intervalSec) / subintervalSec,
        trailingBucketCount: 0,
        buckets: make([]int, numBuckets),
    }, nil
}

// For internal/debugging use only.
func (state *slidingWindow) dump() string {
    return fmt.Sprintf("Current bucket: %d\n", state.currentBucket) +
        fmt.Sprintf("Buckets: %d\n", state.buckets) +
        fmt.Sprintf("Trailing bucket count: %d\n", state.trailingBucketCount)
}

func (state *slidingWindow) syncWithCurrentTime(currentTime time.Time) {
    // index of the bucket corresponding to the current subinterval offset
    currentBucket := (currentTime.Unix() % state.intervalSec) / state.subintervalSec
    // if last update was > intervalSec ago, then zero out all buckets, otherwise zero out buckets for any subintervals
    // that have elapsed since the last sync.
    timeSinceLastSyncSec := int64(currentTime.Sub(state.lastSyncTime).Seconds())
    // if the full interval has elapsed, we need to zero out all buckets, including the trailing bucket count.
    if timeSinceLastSyncSec >= state.intervalSec {
        // zero out all buckets
        for i, _ := range state.buckets {
            state.buckets[i] = 0
        }
        state.trailingBucketCount = 0
    } else {
        cb := state.currentBucket
        // save the previous current bucket count in the trailing bucket count, unless we're still in the same subinterval
        if cb != currentBucket || timeSinceLastSyncSec > state.subintervalSec {
            state.trailingBucketCount = state.buckets[currentBucket]
        }
        // if we've wrapped all the way around to the previous synced bucket, but the full interval hasn't elapsed, we need to
        // zero out all buckets, so prime the bucket-zeroing loop condition below.
        if cb == currentBucket && timeSinceLastSyncSec > state.subintervalSec {
            cb += 1
        }
        // expire old buckets in order until we reach the current bucket index
        for cb != currentBucket {
            cb = (cb + 1) % int64(len(state.buckets))
            state.buckets[cb] = 0
        }
    }
    // finally sync current bucket
    state.currentBucket = currentBucket
    // update sync time
    state.lastSyncTime = currentTime
}

func (state *slidingWindow) GetTotalEventCount(currentTime time.Time) int {
    state.syncWithCurrentTime(currentTime)
    // the corrected total count is the sum of all subinterval counts,
    // but with the trailing subinterval weighted by its overlap with the current (sliding) interval.
    // See comments above for fuller explanation.
    subintervalRemainingSec := state.subintervalSec - (currentTime.Unix() % state.subintervalSec)
    trailingBucketWeight := float32(subintervalRemainingSec) / float32(state.subintervalSec)
    // we add the weighted trailing bucket count to the counts of all buckets to get corrected total count
    totalCount := 0
    for _, c := range state.buckets {
        totalCount += c
    }
    totalCount += int(float32(state.trailingBucketCount) * trailingBucketWeight)
    return totalCount
}

func (state *slidingWindow) RecordEvent(currentTime time.Time) {
    state.syncWithCurrentTime(currentTime)
    state.buckets[state.currentBucket] += 1
}

type Limiter struct {
    requestsAllowedInInterval int
    stateMu sync.Mutex
    state *slidingWindow
}

// Returns a new Limiter with allowed rate `requestsAllowedPerSec` and approximate sliding window of length `intervalSec` with resolution `subintervalSec`.
func New(requestsAllowedInInterval int, intervalSec int64, subintervalSec int64) (limiter *Limiter, err error) {
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
    // Also, even "read" methods on our state like GetTotalEventCount() update the current time, so we need an exclusive lock anyway.
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
