package internal

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type budgetFunc func() bool

func (f budgetFunc) AllowBackground() bool { return f() }

// expire stores an already-expired entry; the memory cache reports TTL 0.
func expire(t *testing.T, c cache[[]byte], key string, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	c.Set(t.Context(), key, b, 0)
	return b
}

func TestStaleBookRevalidates(t *testing.T) {
	t.Parallel()
	getter := NewMockgetter(gomock.NewController(t))
	cache := newMemoryCache()
	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)

	old := expire(t, cache, BookKey(1), workResource{ForeignID: 10, Title: "old"})
	fresh, err := json.Marshal(workResource{ForeignID: 10, Title: "new"})
	require.NoError(t, err)
	getter.EXPECT().GetBook(gomock.Any(), int64(1), gomock.Any()).Return(fresh, int64(0), int64(0), nil)

	got, ttl, err := ctrl.GetBook(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, old, got, "stale value served without waiting")
	assert.Equal(t, _staleTTL, ttl)

	assert.Eventually(t, func() bool {
		b, ttl, _ := cache.GetWithTTL(t.Context(), BookKey(1))
		return ttl > 0 && string(b) == string(fresh)
	}, 5*time.Second, 10*time.Millisecond)
}

func TestStaleBookKeptWhenGoneUpstream(t *testing.T) {
	t.Parallel()
	getter := NewMockgetter(gomock.NewController(t))
	cache := newMemoryCache()
	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)

	old := expire(t, cache, BookKey(1), workResource{ForeignID: 10})
	getter.EXPECT().GetBook(gomock.Any(), int64(1), gomock.Any()).Return(nil, int64(0), int64(0), errNotFound)

	_, _, err = ctrl.GetBook(t.Context(), 1)
	require.NoError(t, err)

	assert.Eventually(t, func() bool {
		b, ttl, _ := cache.GetWithTTL(t.Context(), BookKey(1))
		return ttl > 0 && string(b) == string(old)
	}, 5*time.Second, 10*time.Millisecond)
}

func TestStaleSkipsRevalidateWhenBudgetLow(t *testing.T) {
	t.Parallel()
	getter := NewMockgetter(gomock.NewController(t)) // Any upstream call fails the test.
	cache := newMemoryCache()
	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)
	ctrl.SetBudget(budgetFunc(func() bool { return false }))

	old := expire(t, cache, WorkKey(1), workResource{ForeignID: 1})
	got, _, err := ctrl.GetWork(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, old, got)
	time.Sleep(100 * time.Millisecond)
}

func TestAuthorRefreshKeepsOldWorks(t *testing.T) {
	t.Parallel()
	getter := NewMockgetter(gomock.NewController(t))
	cache := newMemoryCache()
	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)
	go ctrl.Run(t.Context())

	authorID := int64(100)
	old := expire(t, cache, AuthorKey(authorID), AuthorResource{
		ForeignID: authorID,
		Works:     []workResource{{ForeignID: 1}, {ForeignID: 2}},
	})
	fresh, err := json.Marshal(AuthorResource{ForeignID: authorID, Name: "renamed", Works: []workResource{{ForeignID: 3}}})
	require.NoError(t, err)

	getter.EXPECT().GetAuthor(gomock.Any(), authorID).Return(fresh, nil)
	getter.EXPECT().GetAuthorBooks(gomock.Any(), authorID).Return(iter.Seq2[int64, error](func(func(int64, error) bool) {}))

	got, _, err := ctrl.GetAuthor(t.Context(), authorID)
	require.NoError(t, err)
	assert.Equal(t, old, got)

	var author AuthorResource
	assert.Eventually(t, func() bool {
		b, ttl, _ := cache.GetWithTTL(t.Context(), AuthorKey(authorID))
		return ttl > 0 && json.Unmarshal(b, &author) == nil && author.Name == "renamed"
	}, 5*time.Second, 10*time.Millisecond)

	ids := []int64{}
	for _, w := range author.Works {
		ids = append(ids, w.ForeignID)
	}
	assert.Equal(t, []int64{1, 2, 3}, ids)
}

type countingPersister struct {
	nopersist
	deleted atomic.Int32
}

func (p *countingPersister) Delete(context.Context, int64) error {
	p.deleted.Add(1)
	return nil
}

func TestRefreshAuthorFailureKeepsSnapshot(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		err     error
		deleted int32
	}{
		{name: "complete", deleted: 1},
		{name: "failed page", err: errors.New("429"), deleted: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getter := NewMockgetter(gomock.NewController(t))
			p := &countingPersister{}
			ctrl, err := NewController(newMemoryCache(), getter, p, nil)
			require.NoError(t, err)
			go ctrl.Run(t.Context())

			getter.EXPECT().GetAuthorBooks(gomock.Any(), int64(100)).Return(iter.Seq2[int64, error](func(yield func(int64, error) bool) {
				if tt.err != nil {
					yield(0, tt.err)
				}
			}))

			ctrl.metrics.refreshWaitingAdd(1) // As Run does when dispatching.
			ctrl.refreshAuthor(t.Context(), 100, nil)
			waitForDenorm(ctrl)
			assert.Equal(t, tt.deleted, p.deleted.Load())
		})
	}
}

func TestRefreshAuthorDeferredWhenBudgetLow(t *testing.T) {
	t.Parallel()
	getter := NewMockgetter(gomock.NewController(t)) // GetAuthorBooks must not be called.
	p := &countingPersister{}
	ctrl, err := NewController(newMemoryCache(), getter, p, nil)
	require.NoError(t, err)
	ctrl.SetBudget(budgetFunc(func() bool { return false }))

	ctrl.refreshAuthor(t.Context(), 100, nil)
	assert.Equal(t, int32(0), p.deleted.Load())
}
