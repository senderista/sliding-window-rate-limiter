// Package ratelimit provides a sliding window approximation for counting requests and deciding if new requests should be allowed.
// It uses a Redis shared instance to synchronize request counts and throttling decisions among distributed clients.
package ratelimit

import (
	"errors"
	"fmt"
	"github.com/go-redis/redis/v7"
	"log"
	"strconv"
	"sync"
	"time"
)

// this assumes that `subintervalSec` divides `intervalSec`
func getCurrentSubinterval(currentTime time.Time, intervalSec int64, subintervalSec int64) int64 {
	return (currentTime.Unix() % intervalSec) / subintervalSec
}

type slidingWindow struct {
	mu                  sync.Mutex
	intervalSec         int64
	subintervalSec      int64
	lastSyncTime        time.Time
	currentBucket       int64
	trailingBucketCount int
	buckets             []int
}

// Returns a new sliding window state with total interval length `intervalSec` and subintervals of length `subintervalSec`.
// We assume all methods on this type are protected by a mutex in the owner.
func newState(intervalSec int64, subintervalSec int64) (state *slidingWindow, err error) {
	// the window subinterval needs to evenly divide the interval, since we have an integral number of buckets
	if intervalSec%subintervalSec != 0 {
		err = errors.New("subinterval must evenly divide interval")
		return
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
	state = &slidingWindow{
		intervalSec:         intervalSec,
		subintervalSec:      subintervalSec,
		lastSyncTime:        currentTime,
		currentBucket:       getCurrentSubinterval(currentTime, intervalSec, subintervalSec),
		trailingBucketCount: 0,
		buckets:             make([]int, numBuckets),
	}
	return
}

// For internal/debugging use only.
func (state *slidingWindow) dump() string {
	return fmt.Sprintf("Current bucket: %d\n", state.currentBucket) +
		fmt.Sprintf("Buckets: %d\n", state.buckets) +
		fmt.Sprintf("Trailing bucket count: %d\n", state.trailingBucketCount)
}

func (state *slidingWindow) syncWithCurrentTime(currentTime time.Time) {
	// index of the bucket corresponding to the current subinterval offset
	currentBucket := getCurrentSubinterval(currentTime, state.intervalSec, state.subintervalSec)
	// if last update was > intervalSec ago, then zero out all buckets, otherwise zero out buckets for any subintervals
	// that have elapsed since the last sync.
	timeSinceLastSyncSec := int64(currentTime.Sub(state.lastSyncTime).Seconds())
	// if the full interval has elapsed, we need to zero out all buckets, including the trailing bucket count.
	if timeSinceLastSyncSec >= state.intervalSec {
		for i, _ := range state.buckets {
			state.buckets[i] = 0
		}
		state.trailingBucketCount = 0
	} else {
		cb := state.currentBucket
		// FIXME: all this special-case code before the loop is pretty hacky
		// save the previous current bucket count in the trailing bucket count, unless we're still in the same subinterval
		if cb != currentBucket || timeSinceLastSyncSec > state.subintervalSec {
			state.trailingBucketCount = state.buckets[currentBucket]
		}
		// if we've wrapped all the way around to the previous synced bucket, but the full interval hasn't elapsed, we need to
		// zero out all buckets (but not the trailing bucket count), so prime the bucket-zeroing loop condition below.
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

func (state *slidingWindow) GetTotalEventCountInCurrentInterval(currentTime time.Time) int {
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

func (state *slidingWindow) IncrementEventCount(currentTime time.Time, eventCount int) {
	state.syncWithCurrentTime(currentTime)
	state.buckets[state.currentBucket] += eventCount
}

func (state *slidingWindow) GetEventCountForSubinterval(currentTime time.Time, subinterval int64) int {
	state.syncWithCurrentTime(currentTime)
	return state.buckets[subinterval]
}

func (state *slidingWindow) ReplaceEventCountForSubinterval(currentTime time.Time, subinterval int64, eventCount int) {
	state.syncWithCurrentTime(currentTime)
	state.buckets[subinterval] = eventCount
}

type Limiter struct {
	limiterKey                uint64
	resourceKey               uint64
	requestsAllowedInInterval int
	state                     *slidingWindow
	syncManager               *syncManager
}

// Returns a new `Limiter` with allowed rate `requestsAllowedPerSec` and approximate sliding window of length `intervalSec` with resolution `subintervalSec`.
// `limiterKey` is intended to be a unique identifier for the returned `Limiter` (possibly for logging), while `resourceKey` is intended to be
// a 64-bit hash value of some uniquely identifying resource info (such as a URL path), which can be used as a key in a map to find the appropriate `Limiter`
// instance for a request for the resource. `syncDBHostPort` is the address of the shared Redis instance (can be empty for a local `Limiter`).
func New(limiterKey uint64, resourceKey uint64, requestsAllowedInInterval int, intervalSec int64, subintervalSec int64, syncDBHostPort string) (limiter *Limiter, err error) {
	state, err := newState(intervalSec, subintervalSec)
	if err != nil {
		return
	}
	var syncManager *syncManager = nil
	if syncDBHostPort != "" {
		syncManager, err = newSyncManager(limiterKey, resourceKey, state, syncDBHostPort)
		if err != nil {
			return
		}
	}
	limiter = &Limiter{
		limiterKey:                limiterKey,
		resourceKey:               resourceKey,
		requestsAllowedInInterval: requestsAllowedInInterval,
		state:                     state,
		syncManager:               syncManager,
	}
	return
}

// Returns whether a new request should be allowed, given the allowed request count per interval. This method is thread-safe.
func (limiter *Limiter) ShouldAllowRequest() bool {
	// We need to lock the mutex before doing any reads to ensure we're observing a consistent state.
	// We could use sync.RWMutex to avoid taking an exclusive lock on reads, but then we'd need to spin later to upgrade the lock to exclusive.
	// Also, even "read" methods on our state like GetTotalEventCountForInterval() update the current time, so we need an exclusive lock anyway.
	limiter.state.mu.Lock()
	defer limiter.state.mu.Unlock()
	currentTime := time.Now()
	requestsInInterval := limiter.state.GetTotalEventCountInCurrentInterval(currentTime)
	if limiter.requestsAllowedInInterval-requestsInInterval >= 1 {
		limiter.state.IncrementEventCount(currentTime, 1)
		if limiter.syncManager != nil {
			limiter.syncManager.IncrementEventCount(currentTime, 1)
		}
		return true
	}
	return false
}

// For internal testing/debugging only.
func (limiter *Limiter) getRemainingRequestsAllowedInInterval() int {
	limiter.state.mu.Lock()
	defer limiter.state.mu.Unlock()
	currentTime := time.Now()
	requestsInInterval := limiter.state.GetTotalEventCountInCurrentInterval(currentTime)
	return limiter.requestsAllowedInInterval - requestsInInterval
}

type syncManager struct {
	limiterKey  uint64
	resourceKey uint64
	state       *slidingWindow
	redisClient *redis.Client
}

func newSyncManager(limiterKey uint64, resourceKey uint64, state *slidingWindow, dbHostPort string) (mgr *syncManager, err error) {
	// establish the connection to redis
	client := redis.NewClient(&redis.Options{
		Addr:     dbHostPort,
		Password: "", // no password set
		DB:       0,  // use default DB
	})
	_, err = client.Ping().Result()
	if err != nil {
		return
	}
	// instantiate new syncManager, then kick off state update goroutine before returning it
	mgr = &syncManager{
		limiterKey:  limiterKey,
		resourceKey: resourceKey,
		state:       state,
		redisClient: client,
	}
	mgr.initializeTimer()
	return
}

func (syncManager *syncManager) initializeTimer() {
	// set up the timer that syncs counts from the previous subinterval at the beginning of each subinterval.
	// since we need to synchronize subintervals across clients, wait to start timer until we're on a subinterval boundary.
	currentTime := time.Now()
	// calculate next subinterval boundary, in higher resolution (milliseconds) than seconds so that synchronization errors across clients don't compound
	currentTimeSec := currentTime.Unix()
	currentTimeMillis := currentTime.UnixNano() / 1000000
	millisUntilNextSec := 1000 - (currentTimeMillis % 1000)
	nextTimeSec := currentTimeSec + 1
	subintervalRemainingAfterNextSec := syncManager.state.subintervalSec - (nextTimeSec % syncManager.state.subintervalSec)
	millisUntilNextSubinterval := millisUntilNextSec + (subintervalRemainingAfterNextSec * 1000)
	// the ticker won't produce its first tick until its time interval has elapsed, but that should be OK.
	ticker := time.NewTicker(time.Duration(syncManager.state.subintervalSec) * time.Second)
	go func() {
		// sleep until we're on subinterval boundary
		time.Sleep(time.Duration(millisUntilNextSubinterval) * time.Millisecond)
		// if we're not dropping ticks on the floor, we should be able to assume that this is the actual current time.
		for currentTime := range ticker.C {
			// sync is best-effort, so log errors but don't propagate them
			_, err := syncManager.syncSharedEventCounts(currentTime)
			if err != nil {
				log.Printf("Sync error: %v\n", err)
			}
		}
	}()
}

// Increments the shared count in Redis for the current subinterval.
func (syncManager *syncManager) IncrementEventCount(currentTime time.Time, increment int) (success bool, err error) {
	success = true
	// calculate current subinterval
	subinterval := getCurrentSubinterval(currentTime, syncManager.state.intervalSec, syncManager.state.subintervalSec)
	redisKey := fmt.Sprintf("%d:bucket:%d", syncManager.resourceKey, subinterval)
	// calculate absolute time of end of current subinterval
	subintervalRemainingSec := syncManager.state.subintervalSec - (currentTime.Unix() % syncManager.state.subintervalSec)
	subintervalEndSec := currentTime.Unix() + subintervalRemainingSec
	// the assumption is that each client syncs the shared counts for a subinterval at the beginning of the next subinterval,
	// so we only need to persist the counts until the end of the next subinterval.
	expireTimeSec := subintervalEndSec + syncManager.state.subintervalSec
	pipe := syncManager.redisClient.Pipeline()
	pipe.IncrBy(redisKey, int64(increment))
	pipe.ExpireAt(redisKey, time.Unix(expireTimeSec, 0))
	_, err = pipe.Exec()
	if err != nil {
		success = false
	}
	return
}

// Increments the shared count in Redis for the current subinterval.
func (syncManager *syncManager) GetSharedEventCountForSubinterval(subinterval int64) (count int, err error) {
	// calculate current subinterval
	redisKey := fmt.Sprintf("%d:bucket:%d", syncManager.resourceKey, subinterval)
	val, err := syncManager.redisClient.Get(redisKey).Result()
	// nil error from Redis just means the key was never incremented after it expired, so we set it to 0.
	if err != nil {
		count = 0
		if err == redis.Nil {
			err = nil
		}
		return
	}
	count, err = strconv.Atoi(val)
	return
}

// Run at the beginning of each subinterval to sync event counts for the last subinterval from other clients.
func (syncManager *syncManager) syncSharedEventCounts(currentTime time.Time) (count int, err error) {
	// This is executed in a goroutine that runs asynchronously with the main thread, so we need to make state access threadsafe.
	syncManager.state.mu.Lock()
	defer syncManager.state.mu.Unlock()
	currentSubinterval := getCurrentSubinterval(currentTime, syncManager.state.intervalSec, syncManager.state.subintervalSec)
	// we don't use modulo because it could give a negative result
	var previousSubinterval int64
	if currentSubinterval == 0 {
		previousSubinterval = int64(len(syncManager.state.buckets) - 1)
	} else {
		previousSubinterval = int64(currentSubinterval - 1)
	}
	count, err = syncManager.GetSharedEventCountForSubinterval(previousSubinterval)
	if err != nil {
		return
	}
	previousSubintervalCount := syncManager.state.GetEventCountForSubinterval(currentTime, previousSubinterval)
	// all events in the previous subinterval (including our own) should have been returned by GetSharedEventCountForSubinterval(),
	// but for robustness we handle cases where that didn't happen for some reason.
	if count > previousSubintervalCount {
		// note that shared event count should include our local event count, so we replace rather than increment the local count
		syncManager.state.ReplaceEventCountForSubinterval(currentTime, previousSubinterval, count)
	}
	return
}
