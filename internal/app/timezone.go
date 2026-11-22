package app

import (
	"context"
	"sync"
	"time"
)

// cachedZone remembers each market's zone for ttl, so /v1/me and the sign-in
// answers (API 0.62.0) do not read the markets table every time. A zone an
// operator changes shows within ttl.
func cachedZone(zone func(ctx context.Context, code string) string, ttl time.Duration, now func() time.Time) func(ctx context.Context, code string) string {
	type entry struct {
		zone string
		at   time.Time
	}
	var mu sync.Mutex
	cache := map[string]entry{}
	return func(ctx context.Context, code string) string {
		mu.Lock()
		e, ok := cache[code]
		mu.Unlock()
		if ok && now().Sub(e.at) < ttl {
			return e.zone
		}
		z := zone(ctx, code)
		if z != "" { // a failed read is not remembered
			mu.Lock()
			cache[code] = entry{zone: z, at: now()}
			mu.Unlock()
		}
		return z
	}
}
