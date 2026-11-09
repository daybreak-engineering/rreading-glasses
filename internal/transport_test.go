package internal

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryTransport(t *testing.T) {
	t.Parallel()

	bodies := []string{}
	codes := []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusOK}

	rt := &RetryTransport{
		Base: time.Millisecond,
		RoundTripper: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(b))
			code := codes[len(bodies)-1]
			return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.invalid", strings.NewReader("query"))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []string{"query", "query", "query"}, bodies, "body replayed on each attempt")
}

func TestRetryTransportGivesUp(t *testing.T) {
	t.Parallel()

	calls := 0
	rt := &RetryTransport{
		Attempts: 3,
		Base:     time.Millisecond,
		RoundTripper: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, 3, calls)
}

func TestRetryTransportNoRetryOn4xx(t *testing.T) {
	t.Parallel()

	calls := 0
	rt := &RetryTransport{
		RoundTripper: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, 1, calls)
}

func TestBudgetTransport(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	sent := 0
	bt := &BudgetTransport{
		Daily:      4,
		Background: 0.5,
		now:        func() time.Time { return now },
		RoundTripper: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			sent++
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)

	for i := range 4 {
		assert.Equal(t, i < 2, bt.AllowBackground(), "background after %d requests", i)
		_, err := bt.RoundTrip(req)
		require.NoError(t, err)
		now = now.Add(time.Hour)
	}

	_, err = bt.RoundTrip(req)
	require.ErrorIs(t, err, errBudgetExhausted)
	assert.Equal(t, 4, sent, "request past the cap is not sent")

	// The first request falls out of the 24h window.
	now = now.Add(20 * time.Hour)
	assert.Equal(t, int64(3), bt.Used())
	_, err = bt.RoundTrip(req)
	require.NoError(t, err)
}

func TestBudgetTransportRestore(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	hour := now.Unix() / 3600
	bt := &BudgetTransport{Daily: 10, now: func() time.Time { return now }}

	bt.Restore(map[int64]int64{hour: 3, hour - 23: 2, hour - 24: 99, hour + 1: 99})
	assert.Equal(t, int64(5), bt.Used(), "only the last 24h counts")
	assert.Equal(t, map[int64]int64{hour: 3, hour - 23: 2}, bt.Snapshot())

	now = now.Add(time.Hour)
	assert.Equal(t, map[int64]int64{hour: 3}, bt.Snapshot(), "older hours fall out of the window")
	assert.Equal(t, int64(3), bt.Used())
}

func TestBudgetTransportUsesUpstreamCount(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(time.Hour).Unix()
	remaining := "49000"
	headers := func() http.Header {
		h := http.Header{}
		h.Set("X-Ratelimit-Daily-Limit", "50000")
		h.Set("X-Ratelimit-Daily-Remaining", remaining)
		h.Set("X-Ratelimit-Daily-Reset", strconv.FormatInt(reset, 10))
		return h
	}
	bt := &BudgetTransport{
		Daily: 1500,
		now:   func() time.Time { return now },
		RoundTripper: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: headers()}, nil
		}),
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)

	bt.Restore(map[int64]int64{now.Unix() / 3600: 7})
	assert.Equal(t, int64(7), bt.Used(), "saved counts until the upstream reports")

	_, err = bt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), bt.Used(), "the upstream's figure includes other users of the key")

	// A request in flight counts until its response arrives.
	assert.True(t, bt.take())
	assert.Equal(t, int64(1001), bt.Used())
	bt.observe(headers())
	assert.Equal(t, int64(1000), bt.Used())

	// Out-of-order responses never lower usage within a day.
	remaining = "49500"
	bt.observe(headers())
	assert.Equal(t, int64(1000), bt.Used())

	// The cap applies to the upstream's count.
	remaining = "48500"
	bt.observe(headers())
	_, err = bt.RoundTrip(req)
	require.ErrorIs(t, err, errBudgetExhausted)

	// Responses without the headers change nothing.
	bt.observe(http.Header{})
	assert.Equal(t, int64(1500), bt.Used())

	// A new upstream day starts over.
	now = now.Add(2 * time.Hour)
	assert.Zero(t, bt.Used())
	reset = now.Add(24 * time.Hour).Unix()
	remaining = "49990"
	_, err = bt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, int64(10), bt.Used())
}
