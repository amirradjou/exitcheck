// Package backfill checks past launches as they stood at their block, for M4's training set. A check reads "latest"
// everywhere, so instead of rewriting it to pin every read, each launch is checked unchanged against an Anvil fork of
// its block: there, the block is the latest state. Explorers are taken offline, because they only know the present.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/amirradjou/exitcheck/internal/anvil"
	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/check"
	"github.com/amirradjou/exitcheck/internal/explorer"
	"github.com/amirradjou/exitcheck/internal/launch"
	"github.com/amirradjou/exitcheck/internal/store"
)

// CheckAt checks token as it stood at block, on an Anvil fork of the archive RPC upstream (paced to computeUnits per
// second; 0 for none). The facts read the fork as the latest state, and the simulation forks the fork, so neither
// sees anything after block.
func CheckAt(ctx context.Context, net chain.Network, upstream string, computeUnits int, token common.Address, block uint64) (*check.Report, error) {
	if os.Getenv("EXITCHECK_RPC_"+strings.ToUpper(net.Name)) != "" || os.Getenv("EXITCHECK_ARCHIVE_RPC_"+strings.ToUpper(net.Name)) != "" {
		// The simulation forks the network's endpoints, which an override would point away from the fork.
		return nil, fmt.Errorf("unset EXITCHECK_RPC_%[1]s and EXITCHECK_ARCHIVE_RPC_%[1]s for a backfill", strings.ToUpper(net.Name))
	}
	node, err := anvil.Start(ctx, anvil.Fork{URL: upstream, Block: block, ComputeUnits: computeUnits})
	if err != nil {
		return nil, fmt.Errorf("fork of block %d: %w", block, chain.StripURL(err))
	}
	defer node.Close()
	net.RPC, net.ArchiveRPC, net.ArchiveRate, net.WS, net.ForkFromArchive = node.URL, node.URL, 0, "", false
	c, err := chain.DialURLs(ctx, net, node.URL, node.URL, 0)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	r, err := check.Run(ctx, c, explorer.Offline(), token)
	if err != nil {
		return nil, err
	}
	if r.Block != block {
		return nil, fmt.Errorf("the fork answered for block %d, not %d", r.Block, block)
	}
	return r, nil
}

// Scan finds the launches of pools created between two blocks: pools against a quote token (as the live watch takes
// them) that became tradeable, by the watch's own rule, in the block that created them. That is how most launches
// happen (206 of the 210 Base launches and 42 of the 55 Ethereum ones on the live watch's measured day); pools funded
// later are left out.
func Scan(ctx context.Context, c *chain.Client, from, to uint64) ([]store.Backfill, error) {
	pools, err := created(ctx, c, from, to)
	if err != nil {
		return nil, err
	}
	times := map[uint64]time.Time{}
	var out []store.Backfill
	for _, p := range pools {
		_, funded, err := firstMint(ctx, c, p, p.Block, p.Block)
		if err != nil {
			return nil, err
		}
		if !funded {
			continue
		}
		// A v2 or Aerodrome Mint deposits both sides. A v3 one can be the quote token alone: nothing to buy yet.
		if p.DEX == "uniswap-v3" {
			ok, err := Tradeable(ctx, c, p, p.Block)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		t, ok := times[p.Block]
		if !ok {
			if t, err = c.BlockTime(ctx, p.Block); err != nil {
				return nil, err
			}
			times[p.Block] = t
		}
		out = append(out, store.Backfill{
			Launch:     launch.Launch{Pool: p, FundedBlock: p.Block, FundedAt: t, DetectedAt: t},
			CheckBlock: p.Block, Source: "scan",
		})
	}
	return out, nil
}

// created lists the pools created against a quote token between two blocks, as the live watch takes them.
func created(ctx context.Context, c *chain.Client, from, to uint64) ([]launch.Pool, error) {
	q := ethereum.FilterQuery{Topics: [][]common.Hash{{launch.TopicV2, launch.TopicV3, launch.TopicAerodrome}}}
	for a := range launch.Factories(c.Net) {
		q.Addresses = append(q.Addresses, a)
	}
	var pools []launch.Pool
	err := c.ScanLogs(ctx, chain.LogScan{Query: q, From: from, To: to}, func(logs []types.Log) error {
		for _, l := range logs {
			p, ok, err := launch.Decode(c.Net, l)
			if err != nil {
				return err
			}
			if ok && !l.Removed {
				pools = append(pools, p)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("creation logs: %w", chain.StripURL(err))
	}
	return pools, nil
}

var errFound = errors.New("found")

// firstMint finds a pool's first Mint log between two blocks; ok is false if there is none.
func firstMint(ctx context.Context, c *chain.Client, p launch.Pool, from, to uint64) (block uint64, ok bool, err error) {
	q := ethereum.FilterQuery{Addresses: []common.Address{p.Pool}, Topics: [][]common.Hash{{launch.TopicMintV2, launch.TopicMintV3}}}
	err = c.ScanLogs(ctx, chain.LogScan{Query: q, From: from, To: to}, func(logs []types.Log) error {
		if len(logs) > 0 {
			block, ok = logs[0].BlockNumber, true
			return errFound
		}
		return nil
	})
	if err != nil && !errors.Is(err, errFound) {
		return 0, false, fmt.Errorf("mint logs of %s: %w", p.Pool, chain.StripURL(err))
	}
	return block, ok, nil
}
