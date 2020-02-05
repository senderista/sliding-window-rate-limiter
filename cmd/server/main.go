// A simple HTTP server to demonstrate expected usage for the `ratelimit` package.
package main

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"github.com/senderista/sliding-window-rate-limiter/pkg/ratelimit"
	"math/rand"
	"net/http"
)

func hashString(str string) uint64 {
	hash := sha1.Sum([]byte(str))
	return binary.LittleEndian.Uint64(hash[0:8])
}

type state struct {
	rateLimiterMap map[uint64]*ratelimit.Limiter
	redisAddr      string
}

func (state *state) getRateLimiter(resourceHash uint64) (limiter *ratelimit.Limiter, err error) {
	// create new limiter if it's not in the map
	var ok bool
	if limiter, ok = state.rateLimiterMap[resourceHash]; !ok {
		// otherwise create new limiter and add to map:
		// initialize limiter to allow 100 requests/window, with sliding window of 60 seconds and subintervals of 5 seconds.
		limiter, err = ratelimit.New(rand.Uint64(), resourceHash, 100, 60, 5, state.redisAddr)
		if err == nil {
			state.rateLimiterMap[resourceHash] = limiter
		}
	}
	return
}

func (h *state) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	resource := r.URL.Path
	resourceHash := hashString(resource)
	limiter, err := h.getRateLimiter(resourceHash)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if !limiter.ShouldAllowRequest() {
		http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
		return
	}
	fmt.Fprintf(w, "Allowed request for resource %s\n", resource)
}

func main() {
	// initialize handler state
	state := state{
		rateLimiterMap: make(map[uint64]*ratelimit.Limiter),
		redisAddr:      "localhost:6379",
	}
	http.Handle("/", &state)
	http.ListenAndServe(":8090", nil)
}
