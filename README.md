# A Distributed Rate Limiter in Go

The package `ratelimit` in this repository implements a distributed rate limiter as a Go client library communicating with a shared Redis instance, which allows the various clients to synchronize their state and converge on a decision to allow a request. The library is completely agnostic as to the nature of the request: each `ratelimit.Limiter` instance is parameterized by a 64-bit `resourceKey`, which in practice is expected to be a hash of the resource's identifying information (e.g., a URL). All state is scoped to this `resourceKey`, and only one resource is supported per `Limiter` object. It's expected that clients will maintain a map of resources to `Limiter` objects and use the map to delegate an incoming request decision to the appropriate `Limiter` instance for that request's resource.

## Sample Usage
```
import "github.com/senderista/sliding-window-rate-limiter/ratelimit"

resourceHashToLimiter := make(map[uint64]ratelimit.Limiter)
urlHash := hashString64(urlPath)
// this constructs a Limiter which allows up to 100 requests in a 60-second interval, with 5-second resolution, using a Redis instance at localhost:6379.
// if the last host/port argument is an empty string, the Limiter will use only local state.
limiter, err := ratelimit.New(0, urlHash, 100, 60, 5, "localhost:6379")
if err != nil {
    log.Fatal("Error constructing rate limiter: %v", err)
}
resourceHashToLimiter[urlHash] = limiter

resourceHash := request.GetResourceHash()
allowed := resourceHashToLimiter[resourceHash].ShouldAllowRequest()
if allowed {
    log.Println("rate limiter allowed request")
} else {
    log.Println("rate limiter throttled request")
}
```

## Testing

Run `go test` from `sliding-window-rate-limiter/pkg/ratelimit`.

## Design Decisions

### Rate limiting algorithm

There are 3 basic approaches to scalable rate limiting in common use: token bucket, rate estimation, and sliding window approximations (we do not consider exact but unscalable approaches like storing an ordered set of request timestamps). This implementation follows the last of these approaches, since the first two don't satisfy the required semantics of allowing up to the maximum allowed requests within the given interval, regardless of the timing of those requests within the interval. A sliding window with a fixed interval can be approximated by maintaining a circular buffer of subintervals with associated counts and obtaining the estimated count within the interval by summing over all those subintervals. A slightly more accurate approximation is obtained by additionally maintaining a "trailing window" of counts from the previous version of the current subinterval, and weighting its count in the total sum by its overlap with the current sliding window. This "trailing window" approximation can both undercount and overcount events, but gives a more accurate expected result.

### Synchronization algorithm

The next fundamental design decision is how to synchronize event counts between distributed clients of the rate limiter. The simplest and most accurate approach is to just synchronously read and write all counts to shared state in a centralized data store, but this puts synchronous network communication (and any latency in the data store itself) on the critical path of a request. We could avoid this latency by performing all reads and writes to the data store asynchronously, with all synchronous reads and writes only performed against local state, and the shared state periodically synchronized. This, however, introduces considerable complexity in synchronizing these asynchronous reads and writes across distributed clients in order to ensure they reach consistent decisions. For example, we might accumulate all allowed request counts for the current subinterval in a local delta, and at the beginning of the next subinterval, first increment the shared state for the previous subinterval by our delta, and then apply the fully merged deltas from all other clients to our local state for the previous subinterval. But that requires us to know when all other clients have applied their deltas, so we can wait for their changes before applying them to our local state. The options for solving this problem without distributed coordination (which we can rule out immediately due to complexity and unscalability) are unappealing: we could wait some preset amount of time into the next subinterval before syncing other clients' changes, or we could maintain a distributed counter for each subinterval which each client would decrement on syncing its changes, etc. A far simpler alternative which retains the advantage of keeping synchronous network communication off the critical path of *unsuccessful* requests is this: synchronously *write* to shared state, but *read* from shared state only asynchronously, when syncing local state with shared state. The advantage of this is that since we have to read state for all requests, but only have to write state for successful requests, we can avoid any network communication for unsuccessful requests. Since unsuccessful requests are by definition above the ability of our system to gracefully handle (and often reflect malicious or abusive behavior), they will be the scalability bottleneck of any rate limiting system. Here is the specific algorithm: at the beginning of each subinterval, we read the shared count for the previous subinterval (from the centralized data store), and replace our local count for the previous subinterval by the shared count (note that we replace the local count rather than incrementing it by some delta, since the local count is already included in the shared count). During the current subinterval, each time we decide to allow a request, we synchronously increment both our local count for the current subinterval and the shared count. Thus our local count for the current subinterval is always an underestimate and is not reconciled with the other clients until the next subinterval boundary. But if the subintervals are small enough, this should be compatible with desired accuracy, and seems a reasonable tradeoff when it is crucial to avoid synchronous network communication on unsuccessful requests. The above algorithm assumes synchronized clocks across clients, of course (so that subinterval boundaries coincide), but at the 1-second resolution of our implementation this seems reasonable, at least in modern datacenter environments.

### Scalability

Since the rate limiter's state for each resource consumes only a constant amount of space, this algorithm can be considered scalable in terms of the space required per request. Is it scalable in time per request? Again, since it requires only local reads for unsuccessful requests, it does scale for unsuccessful requests (assuming that memory bandwidth can keep up). Since the rate of successful requests is bounded by definition, the fact that the algorithm issues synchronous network writes for each successful request does not really affect its scalability. The scalability of the whole distributed system, of course, is limited by the scalability of the central data store. Writes to Redis can be transparently scaled by sharding using Redis Cluster, while reads (which only occur periodically in this implementation) can be scaled by adding read replicas.

### Persistence

Since the state associated with any request expires after the configured interval has passed, there is little point in persisting shared state, even though Redis is capable of doing so. Persistent shared state would be useful only if the data store had typical outages much shorter than the configured interval, which is not the case for our 1-minute interval.

## Deployment

In order to realize the advantages mentioned above (avoiding network communication on unsuccessful requests), this implementation is a client library, not a language-agnostic RPC-exposing service, so it can only be invoked by in-process code in the same language (Go). The assumption is that the code handling requests we wish to limit is also written in Go and can directly consume this library. If that were not the case, we could still avoid network communication by exposing, say, a Unix socket and a language-agnostic RPC protocol like gRPC to local clients, and running this library in its own process, similar to "sidecar proxies" like Envoy or linkerd.

## Future work

For interoperability with other languages and runtimes, this library could be repackaged as a local proxy service, as described above. If scalability of the centralized data store (Redis) became an issue, other horizontally scalable in-memory key-value stores could be investigated. It might also be possible to use Redis's pub/sub system to notify clients when all counts for a given subinterval had been received, say by each client performing an atomic decrement on a counter for that subinterval and posting a notification message when the counter reached zero. This would allow shared-state writes as well as reads to be fully asynchronous (and only occur once per subinterval).
