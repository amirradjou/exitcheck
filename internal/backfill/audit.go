package backfill

import (
	"context"

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
// token in the range that received liquidity by its end. They are what the live watch should have seen; the range
// must be old enough for the archive RPC to have every block of it.
func FundedPools(ctx context.Context, c *chain.Client, from, to uint64) ([]Funded, error) {
	pools, err := created(ctx, c, from, to)
	if err != nil {
		return nil, err
	}
	var out []Funded
	for _, p := range pools {
		b, ok, err := firstMint(ctx, c, p, p.Block, to)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, Funded{Pool: p, FirstMint: b})
		}
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
