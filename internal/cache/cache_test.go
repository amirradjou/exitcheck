package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

type answer struct {
	Text string `json:"text"`
}

func newCache(t *testing.T) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := New("redis://"+mr.Addr(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func TestFetchCachesAndExpires(t *testing.T) {
	c, mr := newCache(t)
	ctx := context.Background()
	calls := 0
	compute := func(context.Context) (answer, time.Duration, error) { calls++; return answer{"verdict"}, 0, nil }

	if v, hit, err := Fetch(ctx, c, "k", compute); err != nil || hit || v.Text != "verdict" {
		t.Fatalf("first fetch = %v, %v, %v", v, hit, err)
	}
	if v, hit, err := Fetch(ctx, c, "k", compute); err != nil || !hit || v.Text != "verdict" || calls != 1 {
		t.Fatalf("second fetch = %v, hit %v, %v, computed %d times", v, hit, err, calls)
	}
	mr.FastForward(2 * time.Minute)
	if _, hit, _ := Fetch(ctx, c, "k", compute); hit || calls != 2 {
		t.Errorf("entry outlived its TTL (hit %v, computed %d times)", hit, calls)
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	c, _ := newCache(t)
	fail := func(context.Context) (answer, time.Duration, error) { return answer{}, 0, errors.New("rpc down") }
	if _, _, err := Fetch(context.Background(), c, "k", fail); err == nil {
		t.Fatal("error swallowed")
	}
	ok := func(context.Context) (answer, time.Duration, error) { return answer{"fine"}, 0, nil }
	if v, hit, err := Fetch(context.Background(), c, "k", ok); err != nil || hit || v.Text != "fine" {
		t.Errorf("after an error: %v, hit %v, %v", v, hit, err)
	}
}

func TestConcurrentRequestsShareOneComputation(t *testing.T) {
	c, _ := newCache(t)
	var calls atomic.Int32
	release := make(chan struct{})
	compute := func(context.Context) (answer, time.Duration, error) {
		calls.Add(1)
		<-release
		return answer{"shared"}, 0, nil
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if v, _, err := Fetch(context.Background(), c, "k", compute); err != nil || v.Text != "shared" {
				t.Errorf("fetch = %v, %v", v, err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond) // let all five join before the computation finishes
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("computed %d times, want 1", n)
	}
}

func TestRedisDownStillAnswers(t *testing.T) {
	c, mr := newCache(t)
	var errs []error
	c.OnError = func(err error) { errs = append(errs, err) }
	mr.Close()
	compute := func(context.Context) (answer, time.Duration, error) { return answer{"computed"}, 0, nil }
	start := time.Now()
	v, hit, err := Fetch(context.Background(), c, "k", compute)
	if err != nil || hit || v.Text != "computed" {
		t.Fatalf("with Redis down: %v, hit %v, %v", v, hit, err)
	}
	if len(errs) != 1 {
		t.Errorf("%d failures reported; after the first, Redis should be skipped until the cooldown ends", len(errs))
	}
	for range 5 {
		_, _, _ = Fetch(context.Background(), c, "k", compute)
	}
	if len(errs) != 1 || time.Since(start) > time.Second {
		t.Errorf("Redis retried during its cooldown: %d failures in %v", len(errs), time.Since(start))
	}
}

func TestNegativeTTLIsNotCached(t *testing.T) {
	c, _ := newCache(t)
	calls := 0
	partial := func(context.Context) (answer, time.Duration, error) { calls++; return answer{"partial"}, -1, nil }
	for range 2 {
		if _, hit, _ := Fetch(context.Background(), c, "k", partial); hit {
			t.Error("an answer marked not to cache came from the cache")
		}
	}
	if calls != 2 {
		t.Errorf("computed %d times, want 2", calls)
	}
}

func TestCallerStopsWaitingWhenItsContextEnds(t *testing.T) {
	c, _ := newCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	stuck := func(ctx context.Context) (answer, time.Duration, error) {
		<-ctx.Done()
		return answer{}, 0, ctx.Err()
	}
	done := make(chan error)
	go func() { _, _, err := Fetch(ctx, c, "k", stuck); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Fetch kept waiting after its context was cancelled")
	}
}

func TestNilCacheComputes(t *testing.T) {
	v, hit, err := Fetch(context.Background(), (*Cache)(nil), "k", func(context.Context) (answer, time.Duration, error) { return answer{"x"}, 0, nil })
	if err != nil || hit || v.Text != "x" {
		t.Errorf("nil cache: %v, %v, %v", v, hit, err)
	}
}
