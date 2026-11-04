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
