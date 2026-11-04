package internal

import (
	"cmp"
	"io"
	"net/http"
	"strconv"
	"time"
)

// throttledTransport rate limits requests.
type throttledTransport struct {
	http.RoundTripper
	ticker *time.Ticker
}

func (t throttledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-t.ticker.C:
		// allowed
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}

	return t.RoundTripper.RoundTrip(r)
}

// ScopedTransport restricts requests to a particular host.
type ScopedTransport struct {
	Host string
	http.RoundTripper
}

// RoundTrip forces the request to stick to the given host, so redirects can't
// send us elsewhere. Helpful to ensuring credentials don't leak to other
// domains.
func (t ScopedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme = "https"
	r.URL.Host = t.Host
	return t.RoundTripper.RoundTrip(r)
}

// HeaderTransport adds a header to all requests. Best used with a
// scopedTransport.
type HeaderTransport struct {
	Key   string
	Value string
	http.RoundTripper
}

// RoundTrip always sets the header on the request.
func (t *HeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Add(t.Key, t.Value)
	return t.RoundTripper.RoundTrip(r)
}

// RetryTransport retries 429 and 5XX responses with exponential backoff,
// honoring Retry-After when the upstream sends it. Without it a single
// rate-limited batch fails every query in it, which silently empties author
// refreshes.
type RetryTransport struct {
	http.RoundTripper
	Attempts int           // Total tries including the first; defaults to 5 (~30s of backoff).
	Base     time.Duration // First backoff; doubles each retry. Defaults to 2s.
	Max      time.Duration // Backoff cap. Defaults to 30s.
}

// RoundTrip implements http.RoundTripper.
func (t *RetryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	attempts := cmp.Or(t.Attempts, 5)
	wait := cmp.Or(t.Base, 2*time.Second)
	maxWait := cmp.Or(t.Max, 30*time.Second)

	for i := 1; ; i++ {
		resp, err := t.RoundTripper.RoundTrip(r)
		if err != nil || i >= attempts || !retryable(resp.StatusCode) {
			return resp, err
		}
		if r.Body != nil && r.GetBody == nil {
			return resp, nil // Body can't be replayed.
		}

		delay := min(wait, maxWait)
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
			delay = min(time.Duration(s)*time.Second, maxWait)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		Log(r.Context()).Warn("retrying upstream request", "status", resp.StatusCode, "attempt", i, "delay", delay.String())

		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		wait *= 2

		if r.GetBody != nil {
			body, err := r.GetBody()
			if err != nil {
				return nil, err
			}
			r = r.Clone(r.Context())
			r.Body = body
		}
	}
}

func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// errorProxyTransport returns a non-nil statusErr for all response codes 400
// and above so we can return a response with the same code.
type errorProxyTransport struct {
	http.RoundTripper
}

// RoundTrip wraps upstream 4XX and 5XX errors such that they are returned
// directly to the client.
func (t errorProxyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, statusErr(resp.StatusCode)
	}
	return resp, nil
}
