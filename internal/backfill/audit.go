package backfill

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/launch"
)

// Funded is a pool that received liquidity: a launch.
type Funded struct {
	launch.Pool
	FirstMint uint64
}

// FundedPools lists the launches between two blocks, as the factories' logs tell them: pools created against a quote
// token in the range that became tradeable by its end, by the live watch's own rule (launch.Tradeable), read at the
// range's end or, for a pool emptied before then, at its first Mint. They are what the live watch should have seen;
// the range must be old enough for the archive RPC to have every block of it.
func FundedPools(ctx context.Context, c *chain.Client, from, to uint64) ([]Funded, error) {
	pools, err := created(ctx, c, from, to)
	if err != nil {
		return nil, err
	}
	var minted []Funded
	for _, p := range pools {
		b, ok, err := firstMint(ctx, c, p, p.Block, to)
		if err != nil {
			return nil, err
		}
		if ok {
			minted = append(minted, Funded{Pool: p, FirstMint: b})
		}
	}
	atEnd, err := tradeable(ctx, c, minted, to)
	if err != nil {
		return nil, err
	}
	var out []Funded
	for i, f := range minted {
		if !atEnd[i] {
			then, err := tradeable(ctx, c, []Funded{f}, f.FirstMint)
			if err != nil {
				return nil, err
			}
			if !then[0] {
				continue // a position of the quote token alone: nothing to buy
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// tradeable reads, in one archive Multicall3 at block, whether each pool held what launch.Tradeable asks for.
func tradeable(ctx context.Context, c *chain.Client, pools []Funded, block uint64) ([]bool, error) {
	calls := make([]chain.Call, 0, 2*len(pools))
	for _, p := range pools {
		arg := common.LeftPadBytes(p.Pool.Pool.Bytes(), 32)
		calls = append(calls,
			chain.Call{To: p.Token, Data: append(common.FromHex("0x70a08231"), arg...)},
			chain.Call{To: p.Quote, Data: append(common.FromHex("0x70a08231"), arg...)})
	}
	res, err := c.ArchiveMulticallAt(ctx, calls, block)
	if err != nil {
		return nil, fmt.Errorf("balances at block %d: %w", block, chain.StripURL(err))
	}
	holds := func(r chain.Result) bool {
		return r.OK && len(r.Data) >= 32 && new(big.Int).SetBytes(r.Data[:32]).Sign() > 0
	}
	out := make([]bool, len(pools))
	for i, p := range pools {
		out[i] = launch.Tradeable(p.DEX, holds(res[2*i]), holds(res[2*i+1]))
	}
	return out, nil
}

// Missed returns the funded pools the live watch did not record.
func Missed(funded []Funded, seen map[common.Address]bool) []Funded {
	var out []Funded
	for _, f := range funded {
		if !seen[f.Pool.Pool] {
			out = append(out, f)
		}
	}
	return out
}
