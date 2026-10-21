package github

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// _defaultRateLimitWait is how long to wait after a rate limit
	// when GitHub doesn't say how long.
	// GitHub recommends waiting at least a minute.
	// It doubles with every retry of the same request.
	_defaultRateLimitWait = time.Minute

	// _maxRateLimitWait is the longest a request waits for a rate limit
	// before giving up.
	// Primary rate limits may take up to an hour to reset;
	// blocking that long without a word is worse than failing.
	_maxRateLimitWait = 5 * time.Minute

	// _maxRateLimitRetries is how many times a request is retried
	// after being rate limited.
	_maxRateLimitRetries = 3
)

// GatewayOptions customizes how a [Gateway] paces its requests.
type GatewayOptions struct {
	// MutationInterval is the minimum time between the starts of
	// consecutive requests that change data on GitHub:
	// GraphQL mutations and REST requests other than GET.
	// The interval is shared by all concurrent operations on the Gateway.
	//
	// GitHub recommends at least a second between such requests
	// to stay clear of its secondary rate limits.
	// Zero disables pacing.
	MutationInterval time.Duration

	// OnRateLimit, if set, is called when GitHub rate limits a request,
	// before waiting to retry it.
	OnRateLimit func(wait time.Duration, err error)
}

// rateLimitError marks an error as GitHub rate limiting the request.
type rateLimitError struct {
	// wait is how long GitHub asked us to wait, or zero if it didn't say.
	wait time.Duration
	err  error
}

func (e *rateLimitError) Error() string { return e.err.Error() }
func (e *rateLimitError) Unwrap() error { return e.err }

// throttle spaces out mutating requests,
// and holds them back while GitHub is rate limiting us.
type throttle struct {
	interval time.Duration

	mu   sync.Mutex
	next time.Time // earliest start of the next mutating request

	// Replaced in tests.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func newThrottle(interval time.Duration) *throttle {
	return &throttle{
		interval: interval,
		now:      time.Now,
		sleep:    sleepContext,
	}
}

// wait blocks until a mutating request may start.
//
// Each caller reserves its own slot,
// so concurrent callers are spaced out rather than released together.
func (t *throttle) wait(ctx context.Context) error {
	t.mu.Lock()
	now := t.now()
	start := now
	if t.next.After(start) {
		start = t.next
	}
	if t.interval > 0 {
		t.next = start.Add(t.interval)
	}
	t.mu.Unlock()

	if delay := start.Sub(now); delay > 0 {
		return t.sleep(ctx, delay)
	}
	return nil
}

// pause holds back mutating requests until the given time.
func (t *throttle) pause(until time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if until.After(t.next) {
		t.next = until
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// withRateLimit runs one attempt of a request,
// pacing it if it's mutating,
// and retries it if GitHub rate limits it.
//
// attempt must send the request again from scratch every time it's called,
// and report rate limits as [rateLimitError].
func (c *Gateway) withRateLimit(ctx context.Context, mutating bool, attempt func() error) error {
	for retry := 0; ; retry++ {
		if mutating {
			if err := c.throttle.wait(ctx); err != nil {
				return err
			}
		}

		err := attempt()
		var rateErr *rateLimitError
		if !errors.As(err, &rateErr) {
			return err
		}

		wait := rateErr.wait
		if wait <= 0 {
			wait = _defaultRateLimitWait << retry
		}
		if retry >= _maxRateLimitRetries || wait > _maxRateLimitWait {
			return rateErr.err
		}

		// Other mutations would most likely be rejected too.
		c.throttle.pause(c.throttle.now().Add(wait))
		if c.onRateLimit != nil {
			c.onRateLimit(wait, rateErr.err)
		}
		if err := c.throttle.sleep(ctx, wait); err != nil {
			return errors.Join(rateErr.err, err)
		}
	}
}

// httpRateLimit reports whether an unsuccessful HTTP response
// is GitHub rate limiting the request,
// and if so, how long GitHub asked us to wait (zero if it didn't say).
//
// See https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api#exceeding-the-rate-limit.
func (c *Gateway) httpRateLimit(res *http.Response, diagnostic string) (time.Duration, bool) {
	if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}

	if wait, ok := c.rateLimitHeaderWait(res.Header); ok {
		return wait, true
	}

	// A 403 is usually just a missing permission.
	if res.StatusCode == http.StatusForbidden &&
		!strings.Contains(strings.ToLower(diagnostic), "rate limit") {
		return 0, false
	}
	return 0, true
}

// graphQLRateLimit reports whether a GraphQL error response
// is GitHub rate limiting the request,
// and if so, how long GitHub asked us to wait (zero if it didn't say).
func (c *Gateway) graphQLRateLimit(header http.Header, errs graphQLError) (time.Duration, bool) {
	limited := false
	for _, err := range errs {
		// RATE_LIMITED is the primary rate limit.
		// Secondary limits on creating content
		// are reported as UNPROCESSABLE with this message.
		if err.Type == "RATE_LIMITED" ||
			strings.Contains(err.Message, "submitted too quickly") ||
			strings.Contains(strings.ToLower(err.Message), "secondary rate limit") {
			limited = true
			break
		}
	}
	if !limited {
		return 0, false
	}

	wait, _ := c.rateLimitHeaderWait(header)
	return wait, true
}

// rateLimitHeaderWait reports how long GitHub's response headers
// ask us to wait before retrying.
func (c *Gateway) rateLimitHeaderWait(header http.Header) (time.Duration, bool) {
	if secs, err := strconv.Atoi(header.Get("Retry-After")); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}

	if header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return max(time.Unix(reset, 0).Sub(c.throttle.now()), time.Second), true
		}
	}

	return 0, false
}
