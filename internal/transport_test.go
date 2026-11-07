package internal

import (
	"io"
	"net/http"
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
