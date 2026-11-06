package main

// The ENG-1504 connection-resilience switch for daytona-proxy (contract
// gates.md §2, row PROXY_RESILIENCE; budgets.md §2.7).
//
// PROXY_RESILIENCE is a Render env var. It defaults to off, and off is
// exactly today's behaviour: no resolve retry, no stale-if-error window,
// no negative cache, no shared transport. Accepted "on" values,
// case-insensitive: 1, true, on. Everything else — 0, false, off, empty,
// unset, garbage — is off.
//
// The U24 track (t24c) imports this reader and applies the budgets.md
// §2.7 constants below; nothing reads them yet, so shipping this file
// changes no behaviour. resolve() never retries the Interfaces gateway
// path either way (bodies cannot be replayed).

import (
	"os"
	"strings"
	"time"
)

// resilienceEnabled reads PROXY_RESILIENCE. A raw value may be passed for
// tests; when empty the process environment decides.
func resilienceEnabled(raw ...string) bool {
	value := ""
	if len(raw) > 0 {
		value = raw[0]
	} else {
		value = os.Getenv("PROXY_RESILIENCE")
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "on":
		return true
	default:
		return false
	}
}

// Budgets.md §2.7 constants, frozen by the contract.

// resolveRetries is how often resolve() retries unsent failures, 502, 503
// and 504 at most, inside the existing 15 s context.
const resolveRetries = 2

// staleIfErrorWindow is how long the last good resolution is served past
// its TTL while MAS answers 5xx or times out.
const staleIfErrorWindow = 60 * time.Second

// negativeCacheTTL is how long 404 and 410 resolutions are cached.
const negativeCacheTTL = 10 * time.Second

// maxIdleConnsPerHost shapes the shared transport (budgets.md §2.7:
// ≈ 64 per host, shared by apiClient, p.proxy and p.gateway).
const maxIdleConnsPerHost = 64
