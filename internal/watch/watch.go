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
	// Funded reports, for each pool, whether it now holds liquidity.
	Funded(ctx context.Context, pools []launch.Pool) ([]bool, error)
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

	pending map[common.Address]launch.Pool
	cursor  uint64
}

// New returns a watcher for one chain.
func New(chainName string, src Source, state State, pub Publisher, log *slog.Logger) *Watcher {
	return &Watcher{
		Chain: chainName, src: src, state: state, pub: pub, log: log.With("chain", chainName), now: time.Now,
		MaxRange: 500, Expire: 24 * time.Hour, Poll: 30 * time.Second,
		pending: map[common.Address]launch.Pool{},
	}
}

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

// follow processes heads until the subscription ends, polling over HTTP if it goes quiet.
func (w *Watcher) follow(ctx context.Context, heads <-chan uint64) {
	quiet := time.NewTimer(w.Poll)
	defer quiet.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case h, open := <-heads:
			if !open {
				w.log.Warn("new-block subscription ended; resubscribing")
				return
			}
			w.advanceTo(ctx, h)
		case <-quiet.C:
			w.log.Warn("no new block over the subscription", "for", w.Poll)
			w.catchUp(ctx)
		}
		quiet.Reset(w.Poll)
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

// advanceTo processes every block up to head, in steps; a failed step is retried at the next head.
func (w *Watcher) advanceTo(ctx context.Context, head uint64) {
	for w.cursor < head && ctx.Err() == nil {
		to := min(head, w.cursor+w.MaxRange)
		if err := w.Step(ctx, w.cursor+1, to); err != nil {
			w.log.Warn("processing blocks", "from", w.cursor+1, "to", to, "err", err)
			return
		}
	}
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
	funded, err := w.src.Funded(ctx, candidates)
	if err != nil {
		return fmt.Errorf("pool balances: %w", err)
	}

	var launches []launch.Launch
	var added []launch.Pool
	var removed []common.Address
	for i, p := range candidates {
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
	return w.commit(ctx, to, added, removed)
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
	}
	return nil
}

// launch dates a funded pool's first liquidity, so lag is measured from when trading became possible, not from
// when the watcher noticed.
func (w *Watcher) launch(ctx context.Context, p launch.Pool, to uint64) (launch.Launch, error) {
	l := launch.Launch{Pool: p, FundedBlock: to, DetectedAt: w.now()}
	if b, ok, err := w.src.FirstMint(ctx, p, p.Block, to); err != nil {
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
