// Package watch follows a chain for token launches and publishes each one. A launch is a pool created against a
// quote token that has received its first liquidity: pools created and never funded cost nothing downstream.
//
// New blocks arrive over a WebSocket subscription; each block range is then read with two calls whatever the number
// of pools in flight: the factories' creation logs, and one Multicall3 of the pending pools' balances. Funding is read
// from state rather than from Mint logs because free RPCs refuse address-less log queries and cap address lists at
// ten, and because state survives a dropped subscription. The cursor and the pending pools are saved after each range,
// so a restart resumes where it stopped.
package watch

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/launch"
)

// Source is what the watcher needs from the chain.
type Source interface {
	// Heads delivers new block numbers; the channel closes when the subscription fails.
	Heads(ctx context.Context) (<-chan uint64, error)
	Head(ctx context.Context) (uint64, error)
	// Created returns the pools created in blocks from..to.
	Created(ctx context.Context, from, to uint64) ([]launch.Pool, error)
	// Funded reports, for each pool, whether it held liquidity at block. The answer must be for that block: an
	// "unfunded" read from a node that is behind would date the launch late.
	Funded(ctx context.Context, pools []launch.Pool, block uint64) ([]bool, error)
	// FirstMint finds the block of a pool's first liquidity in from..to; ok is false when no Mint log is found.
	FirstMint(ctx context.Context, pool launch.Pool, from, to uint64) (block uint64, ok bool, err error)
	BlockTime(ctx context.Context, n uint64) (time.Time, error)
}

// State keeps the watcher's progress.
type State interface {
	// Cursor is the last block fully processed; ok is false on a first start.
	Cursor(ctx context.Context, chain string) (block uint64, ok bool, err error)
	Pending(ctx context.Context, chain string) ([]launch.Pool, error)
	// Advance records, in one transaction, the new cursor, newly pending pools and pools no longer pending.
	Advance(ctx context.Context, chain string, block uint64, added []launch.Pool, removed []common.Address) error
}

// Publisher sends launches downstream.
type Publisher interface {
	Publish(ctx context.Context, launches []launch.Launch) error
}

// Watcher follows one chain.
type Watcher struct {
	Chain string
	src   Source
	state State
	pub   Publisher
	log   *slog.Logger
	now   func() time.Time

	// MaxRange bounds the blocks read in one step; a longer gap (after downtime) is caught up in steps.
	MaxRange uint64
	// Expire drops a pool that has not been funded this long after it was seen.
	Expire time.Duration
	// Poll reads the head over HTTP when the subscription has been quiet this long.
	Poll time.Duration
	// Retry is the wait before a failed step is tried again (twice) before it is left for the next head: a
	// load-balanced RPC's node often trails the head announcement by a moment.
	Retry time.Duration
	// StepTimeout bounds a step's first attempt; later attempts get longer (see stepBudgets). One goroutine reads the
	// heads and runs the steps, so a call that hangs holds up every launch behind it: a stalled RPC connection once
	// made a Base launch 117 s late, with nothing in the logs.
	StepTimeout time.Duration
	// SlowCall is how long an RPC call may take before it is logged.
	SlowCall time.Duration
	// BehindWarn is how many blocks behind the head the watcher may be before it says so.
	BehindWarn int64

	pending map[common.Address]launch.Pool
	// unfunded is the last block at which each pending pool was seen without liquidity. A pool's first Mint can come
	// long before it is tradeable (a v3 position holding only the quote token), so its launch is dated by the first
	// Mint after that block, not the first Mint ever. Kept in memory: after a restart the search starts at creation.
	unfunded map[common.Address]uint64
	cursor   uint64
}

// New returns a watcher for one chain.
func New(chainName string, src Source, state State, pub Publisher, log *slog.Logger) *Watcher {
	log = log.With("chain", chainName)
	const slow = 5 * time.Second
	return &Watcher{
		Chain: chainName, src: timedSource{Source: src, limit: slow, log: log}, state: state, pub: pub, log: log, now: time.Now,
		MaxRange: 500, Expire: 24 * time.Hour, Poll: 30 * time.Second, Retry: time.Second,
		StepTimeout: 20 * time.Second, SlowCall: slow, BehindWarn: 10,
		pending: map[common.Address]launch.Pool{}, unfunded: map[common.Address]uint64{},
	}
}

// stepBudgets multiply StepTimeout for each attempt at a step. The first two catch a connection that hung: the retry
// opens a new one. The last is long because a step can honestly need the time (a catch-up dating many pools through a
// paced archive RPC); a fixed limit would retry such a step forever and never move on.
var stepBudgets = [...]time.Duration{1, 2, 10}

// Run watches until ctx ends. Failures of the subscription or the RPC are logged and retried; only a failure to
// load the saved state stops it.
func (w *Watcher) Run(ctx context.Context) error {
	if err := w.load(ctx); err != nil {
		return err
	}
	backoff := time.Second
	for ctx.Err() == nil {
		heads, err := w.src.Heads(ctx)
		if err != nil {
			w.log.Warn("subscribing to new blocks", "err", err, "retry_in", backoff)
			w.catchUp(ctx) // keep up over HTTP while the subscription is down
			sleep(ctx, backoff)
			backoff = min(2*backoff, time.Minute)
			continue
		}
		started := time.Now()
		w.follow(ctx, heads)
		// A subscription that drops at once (an endpoint accepting and closing) must not become a busy loop.
		if time.Since(started) < time.Minute {
			sleep(ctx, backoff)
			backoff = min(2*backoff, time.Minute)
		} else {
			backoff = time.Second
		}
	}
	return ctx.Err()
}

func (w *Watcher) load(ctx context.Context) error {
	cursor, ok, err := w.state.Cursor(ctx, w.Chain)
	if err != nil {
		return fmt.Errorf("loading the cursor: %w", err)
	}
	pools, err := w.state.Pending(ctx, w.Chain)
	if err != nil {
		return fmt.Errorf("loading pending pools: %w", err)
	}
	for _, p := range pools {
		w.pending[p.Pool] = p
	}
	if !ok {
		// A first start watches from now on: launches before it were never promised.
		head, err := w.src.Head(ctx)
		if err != nil {
			return fmt.Errorf("first start: %w", err)
		}
		cursor = head
		if err := w.state.Advance(ctx, w.Chain, cursor, nil, nil); err != nil {
			return err
		}
	}
	w.cursor = cursor
	w.log.Info("watching", "from_block", cursor+1, "pending", len(w.pending))
	return nil
}

// follow processes the newest head until the subscription ends, polling over HTTP if it goes quiet.
func (w *Watcher) follow(ctx context.Context, heads <-chan uint64) {
	quiet := time.NewTimer(w.Poll)
	defer quiet.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case h, open := <-heads:
			if open {
				h, open = newest(heads, h)
			}
			if h > 0 {
				if behind := int64(h) - int64(w.cursor); behind > w.BehindWarn {
					w.log.Warn("behind the head", "blocks", behind)
				}
				w.advanceTo(ctx, h)
			}
			if !open {
				w.log.Warn("new-block subscription ended; resubscribing")
				return
			}
		case <-quiet.C:
			w.log.Warn("no new block over the subscription", "for", w.Poll)
			w.catchUp(ctx)
		}
		quiet.Reset(w.Poll)
	}
}

// newest drains the heads already waiting and returns the latest, and whether the subscription is still open. Only the
// newest matters, as a step reads a whole range. Processed one by one, a watcher whose steps take just over a block time
// falls further behind with every block and never recovers, however little work there is: on Base, with a block every
// 2 s, that once put detection 65 s behind and rising with no single call slow.
func newest(heads <-chan uint64, h uint64) (uint64, bool) {
	for {
		select {
		case n, open := <-heads:
			if !open {
				return h, false
			}
			h = max(h, n)
		default:
			return h, true
		}
	}
}

func (w *Watcher) catchUp(ctx context.Context) {
	head, err := w.src.Head(ctx)
	if err != nil {
		w.log.Warn("reading the head", "err", err)
		return
	}
	w.advanceTo(ctx, head)
}

// advanceTo processes every block up to head, in steps. A failed step is tried again after Retry, twice, then left
// for the next head.
func (w *Watcher) advanceTo(ctx context.Context, head uint64) {
	for w.cursor < head && ctx.Err() == nil {
		to := min(head, w.cursor+w.MaxRange)
		var err error
		for attempt, budget := range stepBudgets {
			if attempt > 0 {
				sleep(ctx, w.Retry)
			}
			if err = w.boundedStep(ctx, w.cursor+1, to, budget*w.StepTimeout); err == nil || ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			w.log.Warn("processing blocks", "from", w.cursor+1, "to", to, "err", err)
			return
		}
	}
}

func (w *Watcher) boundedStep(ctx context.Context, from, to uint64, budget time.Duration) error {
	start := time.Now()
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	err := w.Step(sctx, from, to)
	if took := time.Since(start); took >= w.SlowCall {
		w.log.Warn("slow step", "from", from, "to", to, "took_s", took.Seconds(), "budget_s", budget.Seconds(), "failed", err != nil)
	}
	return err
}

// Step processes blocks from..to: records new pools, publishes those now funded, and saves the cursor. It changes
// nothing in memory or in the state unless everything succeeds, so a failed step can simply be run again.
func (w *Watcher) Step(ctx context.Context, from, to uint64) error {
	created, err := w.src.Created(ctx, from, to)
	if err != nil {
		return fmt.Errorf("creation logs: %w", err)
	}
	now := w.now()
	candidates := make([]launch.Pool, 0, len(w.pending)+len(created))
	for _, p := range w.pending {
		candidates = append(candidates, p)
	}
	fresh := map[common.Address]bool{}
	for _, p := range created {
		if _, dup := w.pending[p.Pool]; dup || fresh[p.Pool] {
			continue
		}
		p.SeenAt = now
		fresh[p.Pool] = true
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return w.commit(ctx, to, nil, nil)
	}
	funded, err := w.src.Funded(ctx, candidates, to)
	if err != nil {
		return fmt.Errorf("pool balances: %w", err)
	}

	var launches []launch.Launch
	var added []launch.Pool
	var removed []common.Address
	var unfundedNow []common.Address
	for i, p := range candidates {
		if !funded[i] {
			unfundedNow = append(unfundedNow, p.Pool)
		}
		switch {
		case funded[i]:
			l, err := w.launch(ctx, p, to)
			if err != nil {
				return err
			}
			launches = append(launches, l)
			if !fresh[p.Pool] {
				removed = append(removed, p.Pool)
			}
		case fresh[p.Pool]:
			added = append(added, p)
		case now.Sub(p.SeenAt) > w.Expire:
			removed = append(removed, p.Pool)
		}
	}
	if len(launches) > 0 {
		// Published before the cursor moves: a crash in between re-publishes (the analyzers drop the duplicate),
		// never loses a launch.
		if err := w.pub.Publish(ctx, launches); err != nil {
			return fmt.Errorf("publishing %d launches: %w", len(launches), err)
		}
		for _, l := range launches {
			w.log.Info("launch", "dex", l.DEX, "token", l.Token, "pool", l.Pool.Pool, "funded_block", l.FundedBlock,
				"detect_s", l.DetectedAt.Sub(l.FundedAt).Seconds())
		}
	}
	if err := w.commit(ctx, to, added, removed); err != nil {
		return err
	}
	for _, a := range unfundedNow {
		if _, still := w.pending[a]; still {
			w.unfunded[a] = to
		}
	}
	return nil
}

func (w *Watcher) commit(ctx context.Context, to uint64, added []launch.Pool, removed []common.Address) error {
	if err := w.state.Advance(ctx, w.Chain, to, added, removed); err != nil {
		return fmt.Errorf("saving progress: %w", err)
	}
	w.cursor = to
	for _, p := range added {
		w.pending[p.Pool] = p
	}
	for _, a := range removed {
		delete(w.pending, a)
		delete(w.unfunded, a)
	}
	return nil
}

// launch dates a funded pool's first liquidity, so lag is measured from when trading became possible, not from
// when the watcher noticed.
func (w *Watcher) launch(ctx context.Context, p launch.Pool, to uint64) (launch.Launch, error) {
	l := launch.Launch{Pool: p, FundedBlock: to, DetectedAt: w.now()}
	from := p.Block
	if b, seen := w.unfunded[p.Pool]; seen {
		from = b + 1
	}
	if b, ok, err := w.src.FirstMint(ctx, p, from, to); err != nil {
		w.log.Warn("dating first liquidity; using the current block", "pool", p.Pool, "err", err)
	} else if ok {
		l.FundedBlock = b
	}
	t, err := w.src.BlockTime(ctx, l.FundedBlock)
	if err != nil {
		return l, fmt.Errorf("time of block %d: %w", l.FundedBlock, err)
	}
	l.FundedAt = t
	return l, nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// timedSource logs the source's slow calls by name. A step's total says a launch was late; this says which call did it.
type timedSource struct {
	Source
	limit time.Duration
	log   *slog.Logger
}

func (t timedSource) took(call string, start time.Time) {
	if d := time.Since(start); d >= t.limit {
		t.log.Warn("slow RPC call", "call", call, "took_s", d.Seconds())
	}
}

func (t timedSource) Head(ctx context.Context) (uint64, error) {
	defer t.took("head", time.Now())
	return t.Source.Head(ctx)
}

func (t timedSource) Created(ctx context.Context, from, to uint64) ([]launch.Pool, error) {
	defer t.took("creation logs", time.Now())
	return t.Source.Created(ctx, from, to)
}

func (t timedSource) Funded(ctx context.Context, pools []launch.Pool, block uint64) ([]bool, error) {
	defer t.took("pool balances", time.Now())
	return t.Source.Funded(ctx, pools, block)
}

func (t timedSource) FirstMint(ctx context.Context, p launch.Pool, from, to uint64) (uint64, bool, error) {
	defer t.took("first mint", time.Now())
	return t.Source.FirstMint(ctx, p, from, to)
}

func (t timedSource) BlockTime(ctx context.Context, block uint64) (time.Time, error) {
	defer t.took("block time", time.Now())
	return t.Source.BlockTime(ctx, block)
}
