package ratelimit

import (
    "testing"
    "time"
)

func tryRequest(t *testing.T, limiter *Limiter, startTime time.Time) {
    secondsElapsed := time.Now().Sub(startTime).Seconds()
    remainingRequestsAllowed := limiter.getRemainingRequestsAllowedInInterval(0)
    t.Logf("Bucket state after %.0f seconds: %s", secondsElapsed, limiter.state.dump())
    t.Logf("remaining requests allowed after %.0f seconds: %d", secondsElapsed, remainingRequestsAllowed)
    if !limiter.ShouldAllowRequest(0) {
         t.Errorf("rate limiter throttled us after %.0f seconds", secondsElapsed)
    } else {
        t.Logf("rate limiter allowed request after %.0f seconds", secondsElapsed)
    }
}

func TestOneClient(t *testing.T) {
    // initialize limiter to allow 100 requests/minute, with sliding window of 1 minute and and subintervals of 10 seconds.
    // limiter, _ := New(100, 60, 10)
    limiter, _ := New(100, 20, 5)
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

// var tests = []struct {
//     a, b int
//     want int
// }{
//     {0, 1, 0},
//     {1, 0, 0},
//     {2, -2, -2},
//     {0, -1, -1},
//     {-1, 0, -1},
// }

// for _, tt := range tests {
//     testname := fmt.Sprintf("%d,%d", tt.a, tt.b)
//     t.Run(testname, func(t *testing.T) {
//         ans := IntMin(tt.a, tt.b)
//         if ans != tt.want {
//             t.Errorf("got %d, want %d", ans, tt.want)
//         }
//     })
// }
