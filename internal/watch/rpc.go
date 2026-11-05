package watch

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/launch"
)

// RPC is the Source for a real chain: new heads over its WebSocket endpoint, everything else over the client.
type RPC struct {
	c         *chain.Client
	ws        string
	factories []common.Address
}

// NewRPC returns the source for c's network.
func NewRPC(c *chain.Client) *RPC {
	r := &RPC{c: c, ws: c.Net.WSEndpoint()}
	for f := range launch.Factories(c.Net) {
		r.factories = append(r.factories, f)
	}
	return r
}

// Heads subscribes to new heads. Only the block number is decoded: a full header decode would tie the watch to
// every chain's header format.
func (r *RPC) Heads(ctx context.Context) (<-chan uint64, error) {
	cl, err := rpc.DialContext(ctx, r.ws)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", chain.Host(r.ws), chain.StripURL(err))
	}
	type head struct {
		Number hexutil.Uint64 `json:"number"`
	}
	hs := make(chan head, 16)
	sub, err := cl.EthSubscribe(ctx, hs, "newHeads")
	if err != nil {
		cl.Close()
		return nil, fmt.Errorf("%s: eth_subscribe: %w", chain.Host(r.ws), chain.StripURL(err))
	}
	out := make(chan uint64, 16)
	go func() {
		defer close(out)
		defer cl.Close()
		defer sub.Unsubscribe()
		for {
			select {
			case h := <-hs:
				select {
				case out <- uint64(h.Number):
				case <-ctx.Done():
					return
				}
			case <-sub.Err():
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Head reads the latest block number.
func (r *RPC) Head(ctx context.Context) (uint64, error) {
	return r.c.Head(ctx)
}

// liveRange is how many of a range's most recent blocks are read from the latest-state RPC; older blocks go to the
// archive RPC's windowed scan.
const liveRange = 100

// Created reads the factories' creation logs.
func (r *RPC) Created(ctx context.Context, from, to uint64) ([]launch.Pool, error) {
	q := ethereum.FilterQuery{
		Addresses: r.factories,
		Topics:    [][]common.Hash{{launch.TopicV2, launch.TopicV3, launch.TopicAerodrome}},
	}
	logs, err := r.logs(ctx, q, from, to, 0)
	if err != nil {
		return nil, err
	}
	var pools []launch.Pool
	for _, l := range logs {
		p, ok, err := launch.Decode(r.c.Net, l)
		if err != nil {
			return nil, err
		}
		if ok && !l.Removed {
			pools = append(pools, p)
		}
	}
	return pools, nil
}

// logs reads a range's logs in block order. The most recent liveRange blocks come from the latest-state RPC alone:
// it errors for a block it does not have yet, and the step is retried at the next head. The archive RPC is only
// asked for older blocks: it may answer an empty list instead of an error for a block it has not reached (mevblocker
// does), and a creation log lost that way is a launch lost for good, the cursor having moved past it. limit stops
// after that many logs (0: no limit).
func (r *RPC) logs(ctx context.Context, q ethereum.FilterQuery, from, to uint64, limit int) ([]types.Log, error) {
	var out []types.Log
	recent := from
	if to-from >= liveRange {
		recent = to - liveRange + 1
		errEnough := errors.New("enough")
		err := r.c.ScanLogs(ctx, chain.LogScan{Query: q, From: from, To: recent - 1, MaxRequests: 100}, func(logs []types.Log) error {
			out = append(out, logs...)
			if limit > 0 && len(out) >= limit {
				return errEnough
			}
			return nil
		})
		if errors.Is(err, errEnough) {
			return out, nil
		}
		if err != nil {
			return nil, chain.StripURL(err)
		}
	}
	q.FromBlock, q.ToBlock = new(big.Int).SetUint64(recent), new(big.Int).SetUint64(to)
	logs, err := r.c.Eth.FilterLogs(ctx, q)
	if err != nil {
		return nil, chain.StripURL(err)
	}
	return append(out, logs...), nil
}

var balanceOf = common.FromHex("0x70a08231")

// Funded reads, in one Multicall3 at block, how much of the token and of the quote each pool holds. A v2 or Aerodrome
// pair needs both sides; a v3 pool can be funded with the token alone (single-sided liquidity) and is tradeable once
// it holds the token. A node that does not have the block yet usually does a second later, so the read is retried
// briefly before the step gives up until the next head.
func (r *RPC) Funded(ctx context.Context, pools []launch.Pool, block uint64) ([]bool, error) {
	calls := make([]chain.Call, 0, 2*len(pools))
	for _, p := range pools {
		arg := common.LeftPadBytes(p.Pool.Bytes(), 32)
		calls = append(calls,
			chain.Call{To: p.Token, Data: append(append([]byte{}, balanceOf...), arg...)},
			chain.Call{To: p.Quote, Data: append(append([]byte{}, balanceOf...), arg...)})
	}
	var res []chain.Result
	var err error
	for attempt := range 4 {
		if res, err = r.c.MulticallAt(ctx, calls, block); err == nil || ctx.Err() != nil {
			break
		}
		if attempt < 3 {
			time.Sleep(time.Second)
		}
	}
	if err != nil {
		return nil, chain.StripURL(err)
	}
	positive := func(x chain.Result) bool {
		return x.OK && len(x.Data) >= 32 && new(big.Int).SetBytes(x.Data[:32]).Sign() > 0
	}
	out := make([]bool, len(pools))
	for i, p := range pools {
		out[i] = launch.Tradeable(p.DEX, positive(res[2*i]), positive(res[2*i+1]))
	}
	return out, nil
}

// FirstMint finds the pool's first Mint log.
func (r *RPC) FirstMint(ctx context.Context, p launch.Pool, from, to uint64) (uint64, bool, error) {
	q := ethereum.FilterQuery{
		Addresses: []common.Address{p.Pool},
		Topics:    [][]common.Hash{{launch.TopicMintV2, launch.TopicMintV3}},
	}
	logs, err := r.logs(ctx, q, from, to, 1)
	if err != nil {
		return 0, false, err
	}
	if len(logs) == 0 {
		return 0, false, nil
	}
	return logs[0].BlockNumber, true, nil
}

// BlockTime reads a block's timestamp, from the latest-state RPC for recent blocks.
func (r *RPC) BlockTime(ctx context.Context, n uint64) (time.Time, error) {
	var h struct {
		Time hexutil.Uint64 `json:"timestamp"`
	}
	if err := r.c.RPC.CallContext(ctx, &h, "eth_getBlockByNumber", hexutil.EncodeUint64(n), false); err == nil && h.Time > 0 {
		return time.Unix(int64(h.Time), 0).UTC(), nil
	}
	return r.c.BlockTime(ctx, n)
}
