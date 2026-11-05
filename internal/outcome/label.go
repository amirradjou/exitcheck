package outcome

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/amirradjou/exitcheck/internal/chain"
)

// Launch identifies a launch to label.
type Launch struct {
	ID                        string
	DEX                       string
	Pool, Token, Quote        common.Address
	CreatedBlock, FundedBlock uint64
	FundedAt                  time.Time
}

// Labeller reads outcomes on one chain.
type Labeller struct {
	c     *chain.Client
	feeTo map[string]common.Address // by v2 DEX name, read once
}

// NewLabeller returns a labeller for the chain c serves.
func NewLabeller(c *chain.Client) *Labeller {
	return &Labeller{c: c}
}

// Label reads a launch's outcome from its pool's creation to horizon after its first liquidity. ok is false when
// the horizon has not ended at the chain's head yet. end is the horizon's last block.
func (lb *Labeller) Label(ctx context.Context, l Launch, horizon time.Duration) (o Outcome, end uint64, ok bool, err error) {
	end, ok, err = lb.c.BlockAt(ctx, l.FundedAt.Add(horizon), l.FundedBlock)
	if err != nil || !ok {
		return o, 0, false, err
	}
	p := Pool{DEX: l.DEX, Token: l.Token, Quote: l.Quote}
	if l.DEX == "uniswap-v2" {
		if p.FeeTo, err = lb.feeCollector(ctx, l.DEX); err != nil {
			return o, 0, false, err
		}
	}
	logs, err := lb.logs(ctx, l.Pool, l.CreatedBlock, end)
	if err != nil {
		return o, 0, false, fmt.Errorf("pool logs: %w", err)
	}
	o = Read(p, logs)
	if o.Pulled {
		if o.PulledAt, err = lb.c.BlockTime(ctx, o.PulledBlock); err != nil {
			return o, 0, false, err
		}
	}
	return o, end, true, nil
}

// feeCollector reads a v2 factory's feeTo.
func (lb *Labeller) feeCollector(ctx context.Context, dex string) (common.Address, error) {
	if a, ok := lb.feeTo[dex]; ok {
		return a, nil
	}
	for _, d := range lb.c.Net.V2 {
		if d.Name != dex {
			continue
		}
		res, err := lb.c.Multicall(ctx, []chain.Call{{To: d.Factory, Data: common.FromHex("0x017e7e58")}}) // feeTo()
		if err != nil {
			return common.Address{}, fmt.Errorf("%s feeTo: %w", dex, err)
		}
		var a common.Address
		if len(res) == 1 && res[0].OK && len(res[0].Data) >= 32 {
			a = common.BytesToAddress(res[0].Data[:32])
		}
		if lb.feeTo == nil {
			lb.feeTo = map[string]common.Address{}
		}
		lb.feeTo[dex] = a
		return a, nil
	}
	return common.Address{}, fmt.Errorf("no v2 DEX %q on %s", dex, lb.c.Net.Name)
}

// logs reads a pool's events in one query from the latest-state RPC, falling back to the archive RPC's windowed scan
// when it refuses (old or long ranges).
func (lb *Labeller) logs(ctx context.Context, pool common.Address, from, to uint64) ([]types.Log, error) {
	q := ethereum.FilterQuery{Addresses: []common.Address{pool}, Topics: [][]common.Hash{Topics}}
	fq := q
	fq.FromBlock, fq.ToBlock = new(big.Int).SetUint64(from), new(big.Int).SetUint64(to)
	if logs, err := lb.c.Eth.FilterLogs(ctx, fq); err == nil {
		return logs, nil
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var out []types.Log
	err := lb.c.ScanLogs(ctx, chain.LogScan{Query: q, From: from, To: to}, func(logs []types.Log) error {
		out = append(out, logs...)
		return nil
	})
	if err != nil {
		return nil, chain.StripURL(err)
	}
	return out, nil
}
