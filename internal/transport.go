package internal

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
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

// errBudgetExhausted is returned instead of sending a request once the daily
// budget is spent.
var errBudgetExhausted = errors.Join(statusErr(http.StatusTooManyRequests), errors.New("upstream daily request budget exhausted"))

// BudgetTransport caps requests over a rolling 24h window, counted in hourly
// buckets. Requests past the cap fail without being sent. Counts live in memory;
// PersistBudget saves them so a restart doesn't reset the budget.
type BudgetTransport struct {
	http.RoundTripper
	Daily      int64   // Cap per 24h; 0 disables the cap.
	Background float64 // Share of Daily that background work may use; defaults to 0.8.

	mu     sync.Mutex
	counts [24]int64
	hours  [24]int64 // Unix hour each bucket was last used for.
	now    func() time.Time
	warned bool
}

// RoundTrip implements http.RoundTripper.
func (t *BudgetTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.take() {
		return nil, errBudgetExhausted
	}
	return t.RoundTripper.RoundTrip(r)
}

func (t *BudgetTransport) take() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	used := t.usedLocked()
	if t.Daily > 0 && used >= t.Daily {
		if !t.warned {
			t.warned = true
			Log(context.Background()).Error("upstream daily request budget exhausted", "daily", t.Daily)
		}
		return false
	}
	t.warned = false
	h := t.hour()
	t.counts[h%24]++
	return true
}

// Used returns requests sent in the last 24h.
func (t *BudgetTransport) Used() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.usedLocked()
}

// AllowBackground reports whether background work may still send requests.
func (t *BudgetTransport) AllowBackground() bool {
	if t.Daily <= 0 {
		return true
	}
	share := cmp.Or(t.Background, 0.8)
	return float64(t.Used()) < float64(t.Daily)*share
}

// Snapshot returns request counts per Unix hour, for the last 24h.
func (t *BudgetTransport) Snapshot() map[int64]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.hour()
	out := make(map[int64]int64, len(t.counts))
	for i, c := range t.counts {
		if c > 0 && h-t.hours[i] < 24 {
			out[t.hours[i]] = c
		}
	}
	return out
}

// Restore adds counts from an earlier run. Hours outside the window are ignored.
func (t *BudgetTransport) Restore(counts map[int64]int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.hour()
	for h, c := range counts {
		if h > now || now-h >= 24 {
			continue
		}
		if t.hours[h%24] != h {
			t.hours[h%24] = h
			t.counts[h%24] = 0
		}
		t.counts[h%24] += c
	}
}

func (t *BudgetTransport) hour() int64 {
	now := time.Now
	if t.now != nil {
		now = t.now
	}
	h := now().Unix() / 3600
	if t.hours[h%24] != h {
		t.hours[h%24] = h
		t.counts[h%24] = 0
	}
	return h
}

func (t *BudgetTransport) usedLocked() int64 {
	h := t.hour()
	var sum int64
	for i := range t.counts {
		if h-t.hours[i] < 24 {
			sum += t.counts[i]
		}
	}
	return sum
}
