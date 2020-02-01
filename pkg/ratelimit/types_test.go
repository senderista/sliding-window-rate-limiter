package ratelimit

import (
    "os"
    "testing"
    "time"
)

func tryRequest(t *testing.T, limiter *Limiter, startTime time.Time) {
    secondsElapsed := time.Now().Sub(startTime).Seconds()
    remainingRequestsAllowed := limiter.getRemainingRequestsAllowedInInterval()
    t.Logf("Bucket state after %.0f seconds: %s", secondsElapsed, limiter.state.dump())
    t.Logf("remaining requests allowed after %.0f seconds: %d", secondsElapsed, remainingRequestsAllowed)
    if !limiter.ShouldAllowRequest() {
         t.Errorf("rate limiter throttled us after %.0f seconds", secondsElapsed)
    } else {
        t.Logf("rate limiter allowed request after %.0f seconds", secondsElapsed)
    }
}

func TestOneClient(t *testing.T) {
    // initialize limiter to allow 100 requests/minute, with sliding window of 1 minute and and subintervals of 10 seconds.
    // (clientKey, requestsAllowedInInterval int, intervalSec int64, subintervalSec int64, syncDBHostPort string)
    limiter, err := New(0, 100, 20, 5, "localhost:6379")
    // limiter, err := New(0, 100, 20, 5, "")
    if err != nil {
        t.Error(err)
        os.Exit(1)
    }
    startTime := time.Now()
    // for i := 0; i < 100; i++ {
    for i := 0; i < 100; i++ {
        tryRequest(t, limiter, startTime)
        time.Sleep(time.Millisecond * 100)
    }
    time.Sleep(time.Second * 4)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 3)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 2)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 1)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 1)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 1)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 1)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 1)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 1)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 10)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 1)
    tryRequest(t, limiter, startTime)
    time.Sleep(time.Second * 21)
    tryRequest(t, limiter, startTime)
}
