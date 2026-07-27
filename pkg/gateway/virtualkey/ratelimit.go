package virtualkey

import (
	"fmt"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type RateLimitDimension string

const (
	RateLimitDimensionLLM     RateLimitDimension = "llm"
	RateLimitDimensionMCP     RateLimitDimension = "mcp"
	RateLimitDimensionACP     RateLimitDimension = "acp"
	RateLimitDimensionBuiltin RateLimitDimension = "builtin"
	// RateLimitDimensionAgent is the unified kind=agent ingress bucket. It
	// draws from the same agent rate-limit policy as the runtime-specific
	// dimensions but keys its own bucket, matching the runtime-neutral route.
	// During the internal M4-to-M5 overlap, using both legacy and unified ingress
	// for one runtime can therefore consume twice the policy allowance; M5 removes
	// the legacy ingress instead of retaining that migration-only overlap.
	RateLimitDimensionAgent RateLimitDimension = "agent"
)

type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type Admission struct {
	Allowed           bool
	RetryAfterSeconds int
	RequestsPerMinute int
	Burst             int
}

type limiterKey struct {
	virtualKeyID string
	dimension    RateLimitDimension
}

type limiterBucket struct {
	limiter           *rate.Limiter
	refillRate        rate.Limit
	requestsPerMinute int
	burst             int
}

type limiterRegistry struct {
	mu      sync.RWMutex
	clock   Clock
	buckets map[limiterKey]*limiterBucket
}

func newLimiterRegistry(clock Clock) *limiterRegistry {
	if clock == nil {
		clock = systemClock{}
	}
	return &limiterRegistry{clock: clock, buckets: map[limiterKey]*limiterBucket{}}
}

func (r *limiterRegistry) admit(key VirtualKey, dimension RateLimitDimension) (Admission, error) {
	if key.ID == "" {
		return Admission{}, fmt.Errorf("virtual key id is required")
	}
	// Rate-limit policies are validated at create/update time, so the hot path
	// does not re-validate. A nil rate limit for the matched dimension means the
	// traffic class is unlimited.
	limit, err := rateLimitForDimension(key.RateLimits, dimension)
	if err != nil {
		return Admission{}, err
	}
	if limit == nil {
		return Admission{Allowed: true}, nil
	}

	registryKey := limiterKey{virtualKeyID: key.ID, dimension: dimension}
	r.mu.RLock()
	bucket := r.buckets[registryKey]
	r.mu.RUnlock()
	if bucket == nil || bucket.requestsPerMinute != limit.RequestsPerMinute || bucket.burst != limit.Burst {
		r.mu.Lock()
		bucket = r.buckets[registryKey]
		if bucket == nil || bucket.requestsPerMinute != limit.RequestsPerMinute || bucket.burst != limit.Burst {
			refillRate := rate.Limit(float64(limit.RequestsPerMinute) / 60.0)
			bucket = &limiterBucket{
				limiter:           rate.NewLimiter(refillRate, limit.Burst),
				refillRate:        refillRate,
				requestsPerMinute: limit.RequestsPerMinute,
				burst:             limit.Burst,
			}
			r.buckets[registryKey] = bucket
		}
		r.mu.Unlock()
	}

	now := r.clock.Now()
	result := Admission{
		Allowed:           bucket.limiter.AllowN(now, 1),
		RequestsPerMinute: bucket.requestsPerMinute,
		Burst:             bucket.burst,
	}
	if result.Allowed {
		return result, nil
	}
	tokens := bucket.limiter.TokensAt(now)
	missing := math.Max(0, 1-tokens)
	seconds := missing / float64(bucket.refillRate)
	result.RetryAfterSeconds = max(1, int(math.Ceil(seconds)))
	return result, nil
}

func rateLimitForDimension(limits *VirtualKeyRateLimits, dimension RateLimitDimension) (*RateLimit, error) {
	if limits == nil {
		return nil, nil
	}
	switch dimension {
	case RateLimitDimensionLLM:
		return limits.LLM, nil
	case RateLimitDimensionMCP:
		return limits.MCP, nil
	case RateLimitDimensionACP, RateLimitDimensionBuiltin, RateLimitDimensionAgent:
		return limits.Agent, nil
	default:
		return nil, fmt.Errorf("unsupported rate limit dimension %q", dimension)
	}
}

func (r *limiterRegistry) remove(virtualKeyID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.buckets {
		if key.virtualKeyID == virtualKeyID {
			delete(r.buckets, key)
		}
	}
}

func (r *limiterRegistry) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buckets = map[limiterKey]*limiterBucket{}
}
