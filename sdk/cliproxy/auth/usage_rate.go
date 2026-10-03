package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
)

// All usage callers share these limits, including explicit management refreshes.
const (
	UsageMinFetchInterval = 3 * time.Minute
	UsageFailureBackoff   = 15 * time.Minute
	UsageAncillaryTTL     = 24 * time.Hour
	UsageNearExpiryWindow = 30 * time.Minute
	UsageClaude429Initial = 20 * time.Minute
	UsageClaude429Second  = 40 * time.Minute
	UsageCodex429Initial  = 5 * time.Minute
	UsageCodex429Second   = 15 * time.Minute
	Usage429Maximum       = 60 * time.Minute
)

// UsageHTTPError retains just the safe status/retry metadata, never a response
// body or URL. Ancillary endpoint 429s are carried in UsageFetchResult.RateLimit.
type UsageHTTPError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *UsageHTTPError) Error() string { return fmt.Sprintf("upstream status %d", e.StatusCode) }

type UsageFetchOptions struct {
	Now              time.Time
	SkipProfile      bool
	SkipSubscription bool
}
type usageFetchOptionsKey struct{}

func UsageFetchOptionsFromContext(ctx context.Context) UsageFetchOptions {
	if options, ok := ctx.Value(usageFetchOptionsKey{}).(UsageFetchOptions); ok {
		return options
	}
	return UsageFetchOptions{Now: time.Now()}
}

func usageFetchOptions(entry *usageEntry, now time.Time) UsageFetchOptions {
	fresh := func(name string) bool {
		return entry != nil && len(entry.Raw[name]) > 0 && !entry.rawFetchedAt[name].IsZero() && now.Before(entry.rawFetchedAt[name].Add(UsageAncillaryTTL))
	}
	return UsageFetchOptions{Now: now, SkipProfile: fresh("profile"), SkipSubscription: fresh("subscription")}
}

func usageTokenHash(auth *Auth) [32]byte {
	token, _ := auth.Metadata["access_token"].(string)
	return sha256.Sum256([]byte(token))
}

func usageFetchBlocked(entry *usageEntry, auth *Auth, now time.Time) bool {
	return entry != nil && (now.Before(entry.NextFetchAt) ||
		(entry.waitForToken && entry.failedTokenHash == usageTokenHash(auth)))
}

func applyUsageFetchRate(entry *usageEntry, auth *Auth, result UsageFetchResult, errFetch error, now time.Time, retryDelay time.Duration) {
	limit := result.RateLimit
	var httpError *UsageHTTPError
	if errors.As(errFetch, &httpError) && httpError.StatusCode == 429 {
		limit = httpError
	}
	entry.retryAt = time.Time{}
	entry.CooldownUntil = time.Time{}
	entry.waitForToken = false
	entry.NextFetchAt = entry.lastFetchStartedAt.Add(UsageMinFetchInterval)
	switch {
	case limit != nil:
		if expiry, ok := parseTimeValue(auth.Metadata["expired"]); strings.EqualFold(auth.Provider, "claude") && ok && expiry.Before(now) {
			entry.waitForToken = true
			entry.failedTokenHash = usageTokenHash(auth)
			entry.LastError = "usage authentication expired; waiting for token refresh"
			break
		}
		ladder := []time.Duration{UsageCodex429Initial, UsageCodex429Second, Usage429Maximum}
		if strings.EqualFold(auth.Provider, "claude") {
			ladder = []time.Duration{UsageClaude429Initial, UsageClaude429Second, Usage429Maximum}
		}
		delay := min(max(ladder[min(entry.rateLimitLevel, len(ladder)-1)], limit.RetryAfter), Usage429Maximum)
		entry.rateLimitLevel = min(entry.rateLimitLevel+1, len(ladder)-1)
		entry.CooldownUntil = now.Add(delay)
		entry.retryAt = entry.CooldownUntil
	case errFetch != nil || result.LastError != "":
		entry.retryAt = now.Add(retryDelay)
	default:
		entry.rateLimitLevel = 0
		entry.resetRetryExpiry = time.Time{}
	}
	if entry.retryAt.After(entry.NextFetchAt) {
		entry.NextFetchAt = entry.retryAt
	}
}
