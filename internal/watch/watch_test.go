package watch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/launch"
)

var t0 = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

type fakeSource struct {
	mu      sync.Mutex
	head    uint64
	created map[uint64][]launch.Pool // by block
	funded  map[common.Address]bool
	mint    map[common.Address]uint64
	heads   chan uint64
	ranges  [][2]uint64
}

func (f *fakeSource) Heads(context.Context) (<-chan uint64, error) {
	if f.heads == nil {
		return nil, errors.New("no subscription")
	}
	return f.heads, nil
}

func (f *fakeSource) Head(context.Context) (uint64, error) { return f.head, nil }

func (f *fakeSource) Created(_ context.Context, from, to uint64) ([]launch.Pool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ranges = append(f.ranges, [2]uint64{from, to})
	var out []launch.Pool
	for b := from; b <= to; b++ {
		out = append(out, f.created[b]...)
	}
	return out, nil
}

func (f *fakeSource) Funded(_ context.Context, pools []launch.Pool) ([]bool, error) {
	out := make([]bool, len(pools))
	for i, p := range pools {
		out[i] = f.funded[p.Pool]
	}
	return out, nil
}

func (f *fakeSource) FirstMint(_ context.Context, p launch.Pool, _, _ uint64) (uint64, bool, error) {
	b, ok := f.mint[p.Pool]
	return b, ok, nil
}

func (f *fakeSource) BlockTime(_ context.Context, n uint64) (time.Time, error) {
	return t0.Add(time.Duration(n) * 2 * time.Second), nil
}

type memState struct {
	cursor  map[string]uint64
	pending map[common.Address]launch.Pool
}

func newState() *memState {
	return &memState{cursor: map[string]uint64{}, pending: map[common.Address]launch.Pool{}}
}

func (m *memState) Cursor(_ context.Context, chain string) (uint64, bool, error) {
	b, ok := m.cursor[chain]
	return b, ok, nil
}

func (m *memState) Pending(context.Context, string) ([]launch.Pool, error) {
	return slices.Collect(maps.Values(m.pending)), nil
}

func (m *memState) Advance(_ context.Context, chain string, block uint64, added []launch.Pool, removed []common.Address) error {
	m.cursor[chain] = block
	for _, p := range added {
		m.pending[p.Pool] = p
	}
	for _, a := range removed {
		delete(m.pending, a)
	}
	return nil
}

type fakePub struct {
	mu   sync.Mutex
	got  []launch.Launch
	fail bool
}

func (p *fakePub) Publish(_ context.Context, ls []launch.Launch) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("kafka down")
	}
	p.got = append(p.got, ls...)
	return nil
}

func pool(n byte, block uint64) launch.Pool {
	return launch.Pool{Chain: "base", DEX: "uniswap-v2", Pool: common.BytesToAddress([]byte{0xa0, n}),
		Token: common.BytesToAddress([]byte{0x70, n}), Quote: common.HexToAddress("0x4200000000000000000000000000000000000006"), Block: block}
}

func newWatcher(src *fakeSource, st *memState, pub *fakePub) *Watcher {
	w := New("base", src, st, pub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.now = func() time.Time { return t0.Add(time.Hour) }
	return w
}

func TestCreatedAndFundedInOneStep(t *testing.T) {
	p := pool(1, 101)
	src := &fakeSource{created: map[uint64][]launch.Pool{101: {p}}, funded: map[common.Address]bool{p.Pool: true}, mint: map[common.Address]uint64{p.Pool: 101}}
	st, pub := newState(), &fakePub{}
	w := newWatcher(src, st, pub)
	if err := w.Step(context.Background(), 100, 102); err != nil {
		t.Fatal(err)
	}
	if len(pub.got) != 1 || pub.got[0].FundedBlock != 101 || !pub.got[0].FundedAt.Equal(t0.Add(202*time.Second)) {
		t.Fatalf("published %+v", pub.got)
	}
	if len(st.pending) != 0 || st.cursor["base"] != 102 {
		t.Errorf("pending %v, cursor %d", st.pending, st.cursor["base"])
	}
}

func TestFundedLaterIsDatedFromItsFirstMint(t *testing.T) {
	p := pool(2, 101)
	src := &fakeSource{created: map[uint64][]launch.Pool{101: {p}}, funded: map[common.Address]bool{}, mint: map[common.Address]uint64{}}
	st, pub := newState(), &fakePub{}
	w := newWatcher(src, st, pub)
	ctx := context.Background()
	if err := w.Step(ctx, 100, 105); err != nil {
		t.Fatal(err)
	}
	if len(pub.got) != 0 || len(st.pending) != 1 {
		t.Fatalf("unfunded pool: published %d, pending %d", len(pub.got), len(st.pending))
	}
	src.funded[p.Pool], src.mint[p.Pool] = true, 108
	if err := w.Step(ctx, 106, 110); err != nil {
		t.Fatal(err)
	}
	if len(pub.got) != 1 || pub.got[0].FundedBlock != 108 {
		t.Fatalf("published %+v", pub.got)
	}
	if len(st.pending) != 0 {
		t.Error("a launched pool stayed pending")
	}
}

func TestFailedPublishChangesNothing(t *testing.T) {
	p := pool(3, 101)
	src := &fakeSource{created: map[uint64][]launch.Pool{101: {p}}, funded: map[common.Address]bool{p.Pool: true}, mint: map[common.Address]uint64{}}
	st, pub := newState(), &fakePub{fail: true}
	st.cursor["base"] = 100
	w := newWatcher(src, st, pub)
	w.cursor = 100
	if err := w.Step(context.Background(), 101, 101); err == nil {
		t.Fatal("publish failure swallowed")
	}
	if st.cursor["base"] != 100 || w.cursor != 100 || len(w.pending) != 0 {
		t.Fatalf("state moved after a failed publish: cursor %d/%d, pending %d", st.cursor["base"], w.cursor, len(w.pending))
	}
	pub.fail = false
	w.advanceTo(context.Background(), 101)
	if len(pub.got) != 1 || pub.got[0].FundedBlock != 101 {
		t.Errorf("retry published %+v (no Mint log found: dated at the detecting block)", pub.got)
	}
}

func TestUnfundedPoolsExpire(t *testing.T) {
	old := pool(4, 1)
	old.SeenAt = t0.Add(-48 * time.Hour)
	src := &fakeSource{funded: map[common.Address]bool{}}
	st, pub := newState(), &fakePub{}
	st.pending[old.Pool] = old
	st.cursor["base"] = 10
	w := newWatcher(src, st, pub)
	if err := w.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Step(context.Background(), 11, 12); err != nil {
		t.Fatal(err)
	}
	if len(st.pending) != 0 || len(w.pending) != 0 {
		t.Error("a pool unfunded for two days is still pending")
	}
}

func TestRestartResumesAndFirstStartBeginsAtHead(t *testing.T) {
	src := &fakeSource{head: 500, funded: map[common.Address]bool{}}
	st := newState()
	w := newWatcher(src, st, &fakePub{})
	if err := w.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.cursor != 500 || st.cursor["base"] != 500 {
		t.Errorf("first start at %d, want the head (500)", w.cursor)
	}

	p := pool(5, 400)
	st.pending[p.Pool] = p
	st.cursor["base"] = 450
	w = newWatcher(src, st, &fakePub{})
	if err := w.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.cursor != 450 || len(w.pending) != 1 {
		t.Errorf("restart: cursor %d, pending %d", w.cursor, len(w.pending))
	}
}

func TestLongGapsAreReadInSteps(t *testing.T) {
	src := &fakeSource{funded: map[common.Address]bool{}}
	st := newState()
	w := newWatcher(src, st, &fakePub{})
	w.MaxRange, w.cursor = 100, 1000
	w.advanceTo(context.Background(), 1250)
	want := [][2]uint64{{1001, 1100}, {1101, 1200}, {1201, 1250}}
	if !slices.Equal(src.ranges, want) || w.cursor != 1250 {
		t.Errorf("ranges %v, cursor %d", src.ranges, w.cursor)
	}
}

func TestRunFollowsHeadsAndSurvivesAClosedSubscription(t *testing.T) {
	p := pool(6, 21)
	src := &fakeSource{head: 20, heads: make(chan uint64, 4), created: map[uint64][]launch.Pool{21: {p}},
		funded: map[common.Address]bool{p.Pool: true}, mint: map[common.Address]uint64{p.Pool: 21}}
	st, pub := newState(), &fakePub{}
	w := newWatcher(src, st, pub)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- w.Run(ctx) }()
	src.heads <- 21
	close(src.heads) // the subscription drops; Run must resubscribe rather than return
	deadline := time.After(2 * time.Second)
	for {
		pub.mu.Lock()
		n := len(pub.got)
		pub.mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("launch not published")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v", err)
	}
}
