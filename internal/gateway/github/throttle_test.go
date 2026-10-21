package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock is a clock that only advances when something sleeps on it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	frozen bool // if set, sleeping doesn't advance the clock
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	if !c.frozen {
		c.now = c.now.Add(d)
	}
	return nil
}

func newThrottledTestGateway(
	t *testing.T,
	clock *fakeClock,
	opts *GatewayOptions,
	transport http.RoundTripper,
) *Gateway {
	t.Helper()
	gateway, err := NewGateway(
		"https://api.github.com",
		&http.Client{Transport: transport},
		tokenSourceFunc(func(context.Context) (string, error) {
			return "token", nil
		}),
		opts,
	)
	require.NoError(t, err)
	gateway.throttle.now = clock.Now
	gateway.throttle.sleep = clock.Sleep
	return gateway
}

func okResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestGateway_mutationInterval(t *testing.T) {
	clock := newFakeClock()
	var sentAt []time.Time
	gateway := newThrottledTestGateway(t, clock, &GatewayOptions{
		MutationInterval: time.Second,
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		sentAt = append(sentAt, clock.Now())
		return okResponse(`{"data": {}}`), nil
	}))

	ctx := t.Context()
	var result struct{}
	require.NoError(t, gateway.executeGQL(ctx, "mutation{a}", nil, &result))
	require.NoError(t, gateway.executeGQL(ctx, "query{b}", nil, &result))
	require.NoError(t, gateway.executeGQL(ctx, "mutation{c}", nil, &result))
	require.NoError(t, gateway.postREST(ctx, []string{"d"}, struct{}{}, nil))
	require.NoError(t, gateway.getREST(ctx, []string{"e"}, nil))

	start := sentAt[0]
	offsets := make([]time.Duration, len(sentAt))
	for i, at := range sentAt {
		offsets[i] = at.Sub(start)
	}
	// Reads are never delayed; writes are a second apart.
	assert.Equal(t, []time.Duration{0, 0, time.Second, 2 * time.Second, 2 * time.Second}, offsets)
}

func TestGateway_mutationInterval_concurrent(t *testing.T) {
	// Freeze the clock so that all goroutines see the same "now",
	// as if they all started at once.
	clock := newFakeClock()
	clock.frozen = true
	gateway := newThrottledTestGateway(t, clock, &GatewayOptions{
		MutationInterval: time.Second,
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return okResponse(`{"data": {}}`), nil
	}))

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			var result struct{}
			assert.NoError(t, gateway.executeGQL(t.Context(), "mutation{a}", nil, &result))
		})
	}
	wg.Wait()

	// Every goroutine reserves its own slot instead of all going at once.
	// The first one doesn't wait.
	assert.ElementsMatch(t, []time.Duration{
		time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second,
	}, clock.sleeps)
}

func TestGateway_rateLimit_graphQLSubmittedTooQuickly(t *testing.T) {
	clock := newFakeClock()
	var waits []time.Duration
	attempts := 0
	gateway := newThrottledTestGateway(t, clock, &GatewayOptions{
		OnRateLimit: func(wait time.Duration, err error) {
			assert.ErrorContains(t, err, "was submitted too quickly")
			waits = append(waits, wait)
		},
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return okResponse(`{"errors": [{
				"type": "UNPROCESSABLE",
				"path": ["addComment"],
				"message": "was submitted too quickly"
			}]}`), nil
		}
		return okResponse(`{"data": {"addComment": {"id": "C_1"}}}`), nil
	}))

	var result struct {
		AddComment struct {
			ID string `json:"id"`
		} `json:"addComment"`
	}
	require.NoError(t, gateway.executeGQL(t.Context(), "mutation{addComment}", nil, &result))
	assert.Equal(t, "C_1", result.AddComment.ID)
	assert.Equal(t, 3, attempts)
	// Without a hint from GitHub, wait a minute and back off exponentially.
	assert.Equal(t, []time.Duration{time.Minute, 2 * time.Minute}, waits)
}

func TestGateway_rateLimit_holdsBackOtherMutations(t *testing.T) {
	clock := newFakeClock()
	var sentAt []time.Time
	gateway := newThrottledTestGateway(t, clock, nil, roundTripFunc(func(*http.Request) (*http.Response, error) {
		sentAt = append(sentAt, clock.Now())
		if len(sentAt) == 1 {
			return okResponse(`{"errors": [{"type": "UNPROCESSABLE", "message": "was submitted too quickly"}]}`), nil
		}
		return okResponse(`{"data": {}}`), nil
	}))

	ctx := t.Context()
	var result struct{}
	require.NoError(t, gateway.executeGQL(ctx, "mutation{a}", nil, &result))

	// Make it look like another goroutine sends a mutation
	// while the first one is waiting:
	// it must not go out before the wait has passed.
	gateway.throttle.pause(clock.Now().Add(30 * time.Second))
	require.NoError(t, gateway.executeGQL(ctx, "mutation{b}", nil, &result))
	require.NoError(t, gateway.executeGQL(ctx, "query{c}", nil, &result))

	require.Len(t, sentAt, 4)
	assert.Equal(t, time.Minute, sentAt[1].Sub(sentAt[0]))
	assert.Equal(t, 30*time.Second, sentAt[2].Sub(sentAt[1]))
	assert.Equal(t, time.Duration(0), sentAt[3].Sub(sentAt[2]))
}

func TestGateway_rateLimit_restRetryAfter(t *testing.T) {
	clock := newFakeClock()
	var waits []time.Duration
	attempts := 0
	gateway := newThrottledTestGateway(t, clock, &GatewayOptions{
		OnRateLimit: func(wait time.Duration, _ error) { waits = append(waits, wait) },
	}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		// The body must be sent again on every attempt.
		assert.JSONEq(t, `{"body": "hi"}`, string(body))
		if attempts == 1 {
			res := okResponse(`{"message": "You have exceeded a secondary rate limit."}`)
			res.StatusCode = http.StatusForbidden
			res.Status = "403 Forbidden"
			res.Header.Set("Retry-After", "7")
			return res, nil
		}
		return okResponse(`{}`), nil
	}))

	require.NoError(t, gateway.postREST(t.Context(), []string{"comments"}, map[string]string{"body": "hi"}, nil))
	assert.Equal(t, 2, attempts)
	assert.Equal(t, []time.Duration{7 * time.Second}, waits)
}

func TestGateway_rateLimit_primaryReset(t *testing.T) {
	clock := newFakeClock()
	var waits []time.Duration
	attempts := 0
	gateway := newThrottledTestGateway(t, clock, &GatewayOptions{
		OnRateLimit: func(wait time.Duration, _ error) { waits = append(waits, wait) },
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			res := okResponse(`{"errors": [{"type": "RATE_LIMITED", "message": "API rate limit exceeded"}]}`)
			res.Header.Set("X-RateLimit-Remaining", "0")
			res.Header.Set("X-RateLimit-Reset", "1792584090") // 2026-10-21T12:01:30Z
			return res, nil
		}
		return okResponse(`{"data": {}}`), nil
	}))

	var result struct{}
	require.NoError(t, gateway.executeGQL(t.Context(), "query{a}", nil, &result))
	assert.Equal(t, []time.Duration{90 * time.Second}, waits)
}

func TestGateway_rateLimit_givesUp(t *testing.T) {
	clock := newFakeClock()
	attempts := 0
	gateway := newThrottledTestGateway(t, clock, nil, roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return okResponse(`{"errors": [{"type": "UNPROCESSABLE", "message": "was submitted too quickly"}]}`), nil
	}))

	var result struct{}
	err := gateway.executeGQL(t.Context(), "mutation{a}", nil, &result)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnprocessable)
	assert.EqualError(t, err, "UNPROCESSABLE: was submitted too quickly")
	assert.Equal(t, _maxRateLimitRetries+1, attempts)
}

func TestGateway_rateLimit_waitTooLong(t *testing.T) {
	clock := newFakeClock()
	attempts := 0
	gateway := newThrottledTestGateway(t, clock, nil, roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		res := okResponse(`{"message": "API rate limit exceeded"}`)
		res.StatusCode = http.StatusForbidden
		res.Status = "403 Forbidden"
		res.Header.Set("X-RateLimit-Remaining", "0")
		res.Header.Set("X-RateLimit-Reset", "1792587600") // an hour later
		return res, nil
	}))

	err := gateway.getREST(t.Context(), []string{"a"}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForbidden)
	assert.Equal(t, 1, attempts, "should not wait an hour")
	assert.Empty(t, clock.sleeps)
}

func TestGateway_rateLimit_plainForbiddenNotRetried(t *testing.T) {
	clock := newFakeClock()
	attempts := 0
	gateway := newThrottledTestGateway(t, clock, nil, roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		res := okResponse(`{"message": "Resource not accessible by integration"}`)
		res.StatusCode = http.StatusForbidden
		res.Status = "403 Forbidden"
		return res, nil
	}))

	err := gateway.postREST(t.Context(), []string{"a"}, struct{}{}, nil)
	assert.ErrorIs(t, err, ErrForbidden)
	assert.Equal(t, 1, attempts)
}

func TestGateway_rateLimit_contextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	gateway, err := NewGateway(
		"https://api.github.com",
		&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			cancel()
			return okResponse(`{"errors": [{"type": "UNPROCESSABLE", "message": "was submitted too quickly"}]}`), nil
		})},
		tokenSourceFunc(func(context.Context) (string, error) { return "token", nil }),
		nil,
	)
	require.NoError(t, err)

	var result struct{}
	err = gateway.executeGQL(ctx, "mutation{a}", nil, &result)
	assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
}
