package ratelimit

import (
    "sync"
    "testing"
    "time"
)

func tryRequest(t *testing.T, limiter *Limiter, startTime time.Time) bool {
    secondsElapsed := time.Now().Sub(startTime).Seconds()
    remainingRequestsAllowed := limiter.getRemainingRequestsAllowedInInterval()
    t.Logf("Limiter %d: bucket state after %.0f seconds: %s", limiter.limiterKey, secondsElapsed, limiter.state.dump())
    t.Logf("Limiter %d: remaining requests allowed after %.0f seconds: %d", limiter.limiterKey, secondsElapsed, remainingRequestsAllowed)
    allowed := limiter.ShouldAllowRequest()
    if allowed {
        t.Logf("Limiter %d: rate limiter allowed request after %.0f seconds", limiter.limiterKey, secondsElapsed)
    } else {
        t.Logf("Limiter %d: rate limiter throttled us after %.0f seconds", limiter.limiterKey, secondsElapsed)
    }
    return allowed
}

func doTest(t *testing.T, limiter *Limiter, startTime time.Time, numClients int, wg *sync.WaitGroup) {
    if wg != nil {
        defer wg.Done()
    }
    // we want `requestsAllowedInInterval` requests to be delivered uniformly within twice the subinterval.
    totalRequestBurst := limiter.requestsAllowedInInterval
    localRequestBurst := totalRequestBurst / numClients
    timeBurstSec := int(limiter.state.subintervalSec) * 2
    waitTimeMillis := (numClients * (timeBurstSec * 1000)) / totalRequestBurst
    t.Logf("Limiter %d: localRequestBurst: %d waitTimeMillis: %d", limiter.limiterKey, localRequestBurst, waitTimeMillis)
    for i := 0; i < localRequestBurst; i++ {
        // all these requests are made within 10 seconds, so they should be allowed.
        if !tryRequest(t, limiter, startTime) {
            t.Errorf("Limiter %d: rate limiter incorrectly throttled us", limiter.limiterKey)
        }
        time.Sleep(time.Millisecond * time.Duration(waitTimeMillis))
    }
    // give distributed sync a chance to catch up
    time.Sleep(time.Second * time.Duration(limiter.state.subintervalSec))
    // only 3 * subintervalSec have elapsed, but we've made 100 requests, so we should be throttled
    if tryRequest(t, limiter, startTime) {
        t.Errorf("Limiter %d: rate limiter should have throttled us", limiter.limiterKey)
    }
    time.Sleep(time.Second * time.Duration(limiter.state.intervalSec))
    // a full window has elapsed, so we should allow requests
    if !tryRequest(t, limiter, startTime) {
        t.Errorf("Limiter %d: rate limiter incorrectly throttled us", limiter.limiterKey)
    }
}

func testNConcurrentClients(t *testing.T, numClients int) {
    // we assume that you already have redis running on port 6379. launching redis can be part of a test script preceding "go test".
    // initialize limiter to allow 100 requests/window, with sliding window of 60 seconds and subintervals of 5 seconds.
    // (limiterKey, resourceKey, requestsAllowedInInterval int, intervalSec int64, subintervalSec int64, syncDBHostPort string)
    startTime := time.Now()
    t.Logf("Beginning test of %d concurrent clients at time %s\n", numClients, startTime.String())
    var wg sync.WaitGroup
    wg.Add(numClients)
    for i := 0; i < numClients; i++ {
        limiter, err := New(uint64(i + 1), 0, 100, 60, 5, "localhost:6379")
        if err != nil {
            t.Fatal(err)
        }
        go doTest(t, limiter, startTime, numClients, &wg)
    }
    wg.Wait()
}

func TestConcurrentClients(t *testing.T) {
    for n := 1; n <= 5; n++ {
        testNConcurrentClients(t, n)
    }
}
