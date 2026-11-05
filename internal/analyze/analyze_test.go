package analyze

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/cache"
	"github.com/amirradjou/rugcheck/internal/check"
	"github.com/amirradjou/rugcheck/internal/launch"
	"github.com/amirradjou/rugcheck/internal/store"
	"github.com/amirradjou/rugcheck/internal/verdict"
)

// memStore mirrors the Postgres store's semantics: one row per launch, one alert per token.
type memStore struct {
	mu       sync.Mutex
	launches map[string]*store.Result
	delivers map[string]int
	alerts   map[string]bool // token → sent
}

func newMem() *memStore {
	return &memStore{launches: map[string]*store.Result{}, delivers: map[string]int{}, alerts: map[string]bool{}}
}

func (m *memStore) Receive(_ context.Context, l launch.Launch) (store.Prior, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delivers[l.ID()]++
	if r := m.launches[l.ID()]; r != nil {
		return store.Prior{Done: true, Level: r.Level, Text: r.Answer}, nil
	}
	return store.Prior{}, nil
}

func (m *memStore) Finish(_ context.Context, id string, r store.Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.launches[id] = &r
	return nil
}

func (m *memStore) ClaimAlert(_ context.Context, chain string, token common.Address, _, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := chain + token.Hex()
	if _, taken := m.alerts[k]; taken {
		return false, nil
	}
	m.alerts[k] = false
	return true, nil
}

func (m *memStore) AlertSent(_ context.Context, chain string, token common.Address, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts[chain+token.Hex()] = true
	return nil
}

func (m *memStore) ReleaseAlert(_ context.Context, chain string, token common.Address) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.alerts[chain+token.Hex()] {
		delete(m.alerts, chain+token.Hex())
	}
	return nil
}

type harness struct {
	a      *Analyzer
	store  *memStore
	checks int
	posts  []string
	level  verdict.Level
	fail   error
	down   bool // the alert channel is unreachable
}

func newHarness(t *testing.T, level verdict.Level) *harness {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := cache.New("redis://"+mr.Addr(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	h := &harness{store: newMem(), level: level}
	checker := func(_ context.Context, chain string, token common.Address) (*check.Report, error) {
		h.checks++
		if h.fail != nil {
			return nil, h.fail
		}
		return &check.Report{Chain: chain, Address: token, Name: "Tok", CheckedAt: time.Now(),
			Verdict: verdict.Verdict{Level: h.level, Headline: "headline", Complete: true}}, nil
	}
	alert := func(_ context.Context, text string) error {
		if h.down {
			return errors.New("telegram down")
		}
		h.posts = append(h.posts, text)
		return nil
	}
	h.a = New(h.store, checker, c, alert, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.a.Retry = time.Millisecond
	return h
}

func lnch(pool, token byte) launch.Launch {
	return launch.Launch{
		Pool: launch.Pool{Chain: "base", DEX: "uniswap-v2", Pool: common.BytesToAddress([]byte{0xa0, pool}),
			Token: common.BytesToAddress([]byte{0x70, token}), Quote: common.HexToAddress("0x4200000000000000000000000000000000000006")},
		FundedBlock: 10, FundedAt: time.Now().Add(-5 * time.Second), DetectedAt: time.Now(),
	}
}

func TestScamIsStoredAndAlertedOnce(t *testing.T) {
	h := newHarness(t, verdict.Scam)
	ctx := context.Background()
	l := lnch(1, 1)
	if err := h.a.Handle(ctx, l); err != nil {
		t.Fatal(err)
	}
	if r := h.store.launches[l.ID()]; r == nil || r.Level != "scam" || r.Error != "" {
		t.Fatalf("stored %+v", r)
	}
	if len(h.posts) != 1 || !strings.Contains(h.posts[0], "New launch on base") {
		t.Fatalf("posts %q", h.posts)
	}

	// Kafka redelivers the same launch (a crash before the commit): counted, not checked or alerted again.
	if err := h.a.Handle(ctx, l); err != nil {
		t.Fatal(err)
	}
	if h.checks != 1 || len(h.posts) != 1 || h.store.delivers[l.ID()] != 2 {
		t.Errorf("redelivery: %d checks, %d posts, %d deliveries", h.checks, len(h.posts), h.store.delivers[l.ID()])
	}
}

func TestSecondPoolOfATokenUsesTheCacheAndDoesNotRealert(t *testing.T) {
	h := newHarness(t, verdict.HighRisk)
	ctx := context.Background()
	if err := h.a.Handle(ctx, lnch(1, 7)); err != nil {
		t.Fatal(err)
	}
	second := lnch(2, 7)
	if err := h.a.Handle(ctx, second); err != nil {
		t.Fatal(err)
	}
	if h.checks != 1 || !h.store.launches[second.ID()].FromCache {
		t.Errorf("%d checks; second pool from cache: %v", h.checks, h.store.launches[second.ID()].FromCache)
	}
	// The check that ran keeps its full report; the cached answer has none to keep.
	if r := h.store.launches[lnch(1, 7).ID()].Report; !strings.Contains(string(r), `"name":"Tok"`) {
		t.Errorf("first pool's report = %s", r)
	}
	if r := h.store.launches[second.ID()].Report; r != nil {
		t.Errorf("cached pool's report = %s, want none", r)
	}
	if len(h.posts) != 1 {
		t.Errorf("%d alerts for one token", len(h.posts))
	}
}

func TestClearLaunchesAreNotAlerted(t *testing.T) {
	h := newHarness(t, verdict.Caution)
	if err := h.a.Handle(context.Background(), lnch(1, 1)); err != nil {
		t.Fatal(err)
	}
	if len(h.posts) != 0 {
		t.Errorf("alerted about a caution-level launch: %q", h.posts)
	}
}

func TestFailingChecksAreStoredNotRetriedForever(t *testing.T) {
	h := newHarness(t, verdict.Scam)
	h.fail = errors.New("rpc down")
	l := lnch(1, 1)
	if err := h.a.Handle(context.Background(), l); err != nil {
		t.Fatalf("a failing check blocked the stream: %v", err)
	}
	if h.checks != h.a.Attempts {
		t.Errorf("%d attempts, want %d", h.checks, h.a.Attempts)
	}
	if r := h.store.launches[l.ID()]; r == nil || r.Error == "" || len(h.posts) != 0 {
		t.Errorf("stored %+v, posts %d", r, len(h.posts))
	}
}

func TestAlertIsRetriedUntilPostedOnce(t *testing.T) {
	h := newHarness(t, verdict.Scam)
	h.down = true
	l := lnch(1, 1)
	if err := h.a.Handle(context.Background(), l); err == nil {
		t.Fatal("an unposted alert was acknowledged")
	}
	h.down = false
	if err := h.a.Handle(context.Background(), l); err != nil { // the stream's retry
		t.Fatal(err)
	}
	if len(h.posts) != 1 || h.checks != 1 {
		t.Errorf("%d posts, %d checks", len(h.posts), h.checks)
	}
}

func TestALaunchReadAfterAnOutageIsStoredButNotPosted(t *testing.T) {
	h := newHarness(t, verdict.Scam)
	ctx := context.Background()
	old := lnch(1, 1)
	old.FundedAt = time.Now().Add(-2 * time.Hour)
	if err := h.a.Handle(ctx, old); err != nil {
		t.Fatal(err)
	}
	if r := h.store.launches[old.ID()]; r == nil || r.Level != "scam" {
		t.Fatalf("the old launch should still be stored: %+v", r)
	}
	if len(h.posts) != 0 {
		t.Errorf("posted an alert about a two-hour-old launch: %q", h.posts)
	}
	fresh := lnch(2, 2)
	if err := h.a.Handle(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if len(h.posts) != 1 {
		t.Errorf("a fresh launch after the outage should post: %q", h.posts)
	}
}
