# Feature request: a token-bucket rate limiter package

We need a small, dependency-free Go package `ratelimit` (module path
`github.com/acme/ratelimit`) that other services can import.

- `New(rate float64, burst int) *Limiter`: `rate` is tokens added per second,
  `burst` is the bucket capacity.
- `(*Limiter) Allow() bool`: takes one token if available, otherwise returns false
  without blocking.
- `(*Limiter) AllowN(n int) bool` for batches.
- Safe for concurrent use from many goroutines.
- Time must be injectable so tests are deterministic (no sleeping in tests).
- Standard library only. Tests included, run with `go test ./...`.
