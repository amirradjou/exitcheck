// Package backfill checks past launches as they stood at their block, for M4's training set. A check reads "latest"
// everywhere, so instead of rewriting it to pin every read, each launch is checked unchanged against an Anvil fork of
// its block: there, the block is the latest state. Explorers are taken offline, because they only know the present.
package backfill

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/amirradjou/rugcheck/internal/anvil"
	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/check"
	"github.com/amirradjou/rugcheck/internal/explorer"
	"github.com/amirradjou/rugcheck/internal/launch"
	"github.com/amirradjou/rugcheck/internal/store"
)

// CheckAt checks token as it stood at block, on an Anvil fork of the archive RPC upstream (paced to computeUnits per
// second; 0 for none). The facts read the fork as the latest state, and the simulation forks the fork, so neither
// sees anything after block.
func CheckAt(ctx context.Context, net chain.Network, upstream string, computeUnits int, token common.Address, block uint64) (*check.Report, error) {
	if os.Getenv("RUGCHECK_RPC_"+strings.ToUpper(net.Name)) != "" || os.Getenv("RUGCHECK_ARCHIVE_RPC_"+strings.ToUpper(net.Name)) != "" {
		// The simulation forks the network's endpoints, which an override would point away from the fork.
		return nil, fmt.Errorf("unset RUGCHECK_RPC_%[1]s and RUGCHECK_ARCHIVE_RPC_%[1]s for a backfill", strings.ToUpper(net.Name))
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
// them) that got their first liquidity in the block that created them. That is how most launches happen (206 of the
// 210 Base launches and 42 of the 55 Ethereum ones on the live watch's measured day); pools funded later are left out.
func Scan(ctx context.Context, c *chain.Client, from, to uint64) ([]store.Backfill, error) {
	factories := launch.Factories(c.Net)
	q := ethereum.FilterQuery{Topics: [][]common.Hash{{launch.TopicV2, launch.TopicV3, launch.TopicAerodrome}}}
	for a := range factories {
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
	times := map[uint64]time.Time{}
	var out []store.Backfill
	for _, p := range pools {
		funded, err := mintedIn(ctx, c, p)
		if err != nil {
			return nil, err
		}
		if !funded {
			continue
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

// mintedIn reports whether a pool received liquidity in the block that created it.
func mintedIn(ctx context.Context, c *chain.Client, p launch.Pool) (bool, error) {
	q := ethereum.FilterQuery{Addresses: []common.Address{p.Pool}, Topics: [][]common.Hash{{launch.TopicMintV2, launch.TopicMintV3}}}
	found := false
	err := c.ScanLogs(ctx, chain.LogScan{Query: q, From: p.Block, To: p.Block}, func(logs []types.Log) error {
		found = found || len(logs) > 0
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("mint logs of %s: %w", p.Pool, chain.StripURL(err))
	}
	return found, nil
}
