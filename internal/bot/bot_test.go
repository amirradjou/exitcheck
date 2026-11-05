package bot

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
	"github.com/amirradjou/rugcheck/internal/telegram"
	"github.com/amirradjou/rugcheck/internal/verdict"
)

const pepe = "0x6982508145454Ce325dDbE47a25d4ec3d2311933"

// fakeTG records what the bot sends.
type fakeTG struct {
	mu   sync.Mutex
	sent []string
	// shown is every text the chat saw, sent or edited, in order.
	shown []string
	next  int64
}

func (f *fakeTG) Updates(context.Context, int64, time.Duration) ([]telegram.Update, error) {
	return nil, nil
}

func (f *fakeTG) Send(_ context.Context, _ int64, text string, _ int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	f.shown = append(f.shown, text)
	f.next++
	return f.next, nil
}

func (f *fakeTG) Edit(_ context.Context, _, _ int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shown = append(f.shown, text)
	return nil
}

func (f *fakeTG) Typing(context.Context, int64) error { return nil }

func (f *fakeTG) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shown[len(f.shown)-1]
}

func report(chain string, token common.Address) *check.Report {
	return &check.Report{
		Chain: chain, Address: token, Name: "Pepe", Symbol: "PEPE", Block: 1, CheckedAt: time.Now(),
		Verdict: verdict.Verdict{Level: verdict.Clear, Headline: "No red flags found",
			Reasons: []verdict.Reason{{Level: verdict.Clear, Text: "A test sale worked."}}},
	}
}

type harness struct {
	bot     *Bot
	tg      *fakeTG
	checks  int
	locates int
	served  []Served
}

func newHarness(t *testing.T, chains []string, checkErr error) *harness {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := cache.New("redis://"+mr.Addr(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	h := &harness{tg: &fakeTG{}}
	checker := func(_ context.Context, chain string, token common.Address) (*check.Report, error) {
		h.checks++
		if checkErr != nil {
			return nil, checkErr
		}
		return report(chain, token), nil
	}
	locate := func(context.Context, common.Address) ([]string, error) { h.locates++; return chains, nil }
	h.bot = New(h.tg, checker, locate, c, Config{PerChat: 2, Window: time.Minute},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.bot.Served = func(s Served) { h.served = append(h.served, s) }
	return h
}

func msg(text string) *telegram.Message {
	return &telegram.Message{ID: 1, Chat: telegram.Chat{ID: 7, Type: "private"}, Text: text}
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		text, chain string
		ok          bool
	}{
		{pepe, "", true},
		{"is " + pepe + " safe on base?", "base", true},
		{"/check " + pepe + " ETH", "ethereum", true},
		{"BASE: " + strings.ToLower(pepe), "base", true},
		{"hello", "", false},
		{"0x123", "", false},
	} {
		token, chain, ok := parse(c.text)
		if ok != c.ok || chain != c.chain || (ok && token != common.HexToAddress(pepe)) {
			t.Errorf("parse(%q) = %s, %q, %v", c.text, token, chain, ok)
		}
	}
}

func TestAnswersThenServesFromCache(t *testing.T) {
	h := newHarness(t, []string{"ethereum"}, nil)
	ctx := context.Background()

	h.bot.Handle(ctx, msg(pepe))
	if len(h.tg.sent) != 1 || !strings.HasPrefix(h.tg.sent[0], "Checking") {
		t.Fatalf("no progress message: %q", h.tg.sent)
	}
	if got := h.tg.last(); !strings.Contains(got, "No red flags found") || !strings.Contains(got, "Pepe (PEPE) on Ethereum") {
		t.Errorf("answer:\n%s", got)
	}

	h.bot.Handle(ctx, msg(pepe))
	if h.checks != 1 || h.locates != 1 {
		t.Errorf("checked %d times and located %d times; the second request should come from the cache", h.checks, h.locates)
	}
	if got := h.tg.last(); !strings.Contains(got, "Cached answer") {
		t.Errorf("cached answer not marked:\n%s", got)
	}
	if len(h.served) != 2 || h.served[0].Cached || !h.served[1].Cached {
		t.Errorf("served = %+v", h.served)
	}
}

func TestOneCheckPerChatAtATime(t *testing.T) {
	h := newHarness(t, []string{"ethereum"}, nil)
	if !h.bot.claim(7) {
		t.Fatal("claim failed on an idle chat")
	}
	h.bot.Handle(context.Background(), msg(pepe))
	if h.checks != 0 || !strings.Contains(h.tg.last(), "Still checking") {
		t.Errorf("a second check started while one was in flight: %q", h.tg.last())
	}
	h.bot.release(7)
}

func TestPerChatAllowance(t *testing.T) {
	h := newHarness(t, []string{"ethereum"}, nil)
	now := time.Unix(1_800_000_000, 0)
	h.bot.now = func() time.Time { return now }
	for i, addr := range []string{"0x1111111111111111111111111111111111111111", "0x2222222222222222222222222222222222222222", "0x3333333333333333333333333333333333333333"} {
		h.bot.Handle(context.Background(), msg(addr))
		if i == 2 && !strings.Contains(h.tg.last(), "a lot of checks") {
			t.Errorf("third uncached check within the window allowed: %q", h.tg.last())
		}
	}
	if h.checks != 2 {
		t.Errorf("%d checks ran, want 2", h.checks)
	}
	now = now.Add(2 * time.Minute)
	h.bot.Handle(context.Background(), msg("0x3333333333333333333333333333333333333333"))
	if h.checks != 3 {
		t.Error("allowance did not refill after the window")
	}
}

func TestBothChainsAndNoContract(t *testing.T) {
	h := newHarness(t, []string{"ethereum", "base"}, nil)
	h.bot.Handle(context.Background(), msg(pepe))
	if got := h.tg.last(); !strings.Contains(got, "also a contract on base") {
		t.Errorf("no note about the other chain:\n%s", got)
	}

	h = newHarness(t, nil, nil)
	h.bot.Handle(context.Background(), msg(pepe))
	if got := h.tg.last(); !strings.Contains(got, "no contract") || h.checks != 0 {
		t.Errorf("missing contract: %q (checks %d)", got, h.checks)
	}
}

func TestFailuresDoNotLeakInternals(t *testing.T) {
	h := newHarness(t, []string{"ethereum"}, errors.New("dial https://key@rpc.example/v2/SECRET: refused"))
	h.bot.Handle(context.Background(), msg(pepe))
	got := h.tg.last()
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "went wrong") {
		t.Errorf("failure message: %q", got)
	}
	if len(h.served) != 0 {
		t.Error("a failure counted as served")
	}

	h = newHarness(t, []string{"ethereum"}, context.DeadlineExceeded)
	h.bot.Handle(context.Background(), msg(pepe))
	if !strings.Contains(h.tg.last(), "took too long") {
		t.Errorf("timeout message: %q", h.tg.last())
	}
}

func TestHelpOnlyWhenAddressed(t *testing.T) {
	h := newHarness(t, []string{"ethereum"}, nil)
	h.bot.Handle(context.Background(), msg("hi"))
	if len(h.tg.sent) != 1 || !strings.Contains(h.tg.sent[0], "Send me a token address") {
		t.Errorf("no help in a private chat: %q", h.tg.sent)
	}
	group := &telegram.Message{ID: 2, Chat: telegram.Chat{ID: 9, Type: "group"}, Text: "gm everyone"}
	h.bot.Handle(context.Background(), group)
	if len(h.tg.sent) != 1 {
		t.Error("the bot answered small talk in a group")
	}
}

func TestShutdownReachesRunningAndQueuedChecks(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := cache.New("redis://"+mr.Addr(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var mu sync.Mutex
	started := 0
	running := make(chan struct{}, 2)
	checker := func(ctx context.Context, _ string, _ common.Address) (*check.Report, error) {
		mu.Lock()
		started++
		mu.Unlock()
		running <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	locate := func(context.Context, common.Address) ([]string, error) { return []string{"ethereum"}, nil }
	b := New(&fakeTG{}, checker, locate, c, Config{Concurrency: 1, Timeout: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i, addr := range []string{"0x1111111111111111111111111111111111111111", "0x2222222222222222222222222222222222222222"} {
		m := &telegram.Message{ID: 1, Chat: telegram.Chat{ID: int64(i + 1), Type: "private"}, Text: addr}
		wg.Go(func() { b.Handle(ctx, m) })
		if i == 0 {
			<-running // the first check holds the only slot; the second will queue
		}
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handlers still running two seconds after shutdown")
	}
	mu.Lock()
	defer mu.Unlock()
	if started != 1 {
		t.Errorf("%d checks started; the queued one must not start after shutdown", started)
	}
}

// explainHarness answers with a verdict that has something to explain.
func explainHarness(t *testing.T, explain func(context.Context, *check.Report) string) *harness {
	t.Helper()
	h := newHarness(t, []string{"ethereum"}, nil)
	h.bot.check = func(_ context.Context, chain string, token common.Address) (*check.Report, error) {
		h.checks++
		r := report(chain, token)
		r.Verdict = verdict.Verdict{Level: verdict.HighRisk, Headline: "High risk", Complete: true,
			Reasons: []verdict.Reason{{Level: verdict.HighRisk, Text: "The owner can mint."}}}
		return r, nil
	}
	h.bot.cfg.Explain = explain
	return h
}

func TestVerdictGoesOutBeforeItsExplanation(t *testing.T) {
	var shownWhenExplaining string
	var h *harness
	h = explainHarness(t, func(context.Context, *check.Report) string {
		shownWhenExplaining = h.tg.last()
		return "Why, in plain words: the owner can mint."
	})
	ctx := context.Background()
	h.bot.Handle(ctx, msg(pepe))
	if !strings.Contains(shownWhenExplaining, "High risk") {
		t.Errorf("the chat was not shown the verdict before the model ran: %q", shownWhenExplaining)
	}
	if got := h.tg.last(); !strings.Contains(got, "High risk") || !strings.Contains(got, "Why, in plain words") {
		t.Errorf("final answer:\n%s", got)
	}
	h.bot.Handle(ctx, msg(pepe))
	if got := h.tg.last(); h.checks != 1 || !strings.Contains(got, "Why, in plain words") || !strings.Contains(got, "Cached answer") {
		t.Errorf("the cached answer should carry the explanation (%d checks):\n%s", h.checks, got)
	}
}

func TestAModelDoesNotHoldBackAnotherChatAskingAboutTheSameToken(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	h := explainHarness(t, func(context.Context, *check.Report) string {
		close(started)
		<-finish
		return "Why, in plain words: the owner can mint."
	})
	first := make(chan struct{})
	go func() {
		defer close(first)
		h.bot.Handle(context.Background(), msg(pepe))
	}()
	<-started // the first request has its verdict and is waiting on the model

	other := msg(pepe)
	other.Chat.ID = 8
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.bot.Handle(context.Background(), other)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(finish)
		t.Fatal("a second chat waited for the model")
	}
	if got := h.tg.last(); !strings.Contains(got, "High risk") {
		t.Errorf("the second chat got:\n%s", got)
	}
	close(finish)
	<-first
}

func TestVerdictSurvivesAModelThatFailsOrIsSlow(t *testing.T) {
	h := explainHarness(t, func(ctx context.Context, _ *check.Report) string {
		<-ctx.Done() // a model that never answers, until the bot's budget ends
		return ""
	})
	h.bot.cfg.ExplainTimeout = time.Millisecond
	h.bot.Handle(context.Background(), msg(pepe))
	if got := h.tg.last(); !strings.Contains(got, "High risk") || strings.Contains(got, "Something went wrong") {
		t.Errorf("answer:\n%s", got)
	}
}

func TestClearVerdictIsNotExplained(t *testing.T) {
	h := newHarness(t, []string{"ethereum"}, nil)
	h.bot.cfg.Explain = func(context.Context, *check.Report) string {
		t.Error("a clear verdict was put to the model")
		return ""
	}
	h.bot.Handle(context.Background(), msg(pepe))
}
