package chain

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// Call is one read in a Multicall3 batch.
type Call struct {
	To   common.Address
	Data []byte
}

// Result is the outcome of one Call. OK is false when the call reverted.
type Result struct {
	OK   bool
	Data []byte
}

var multicallABI = MustABI(`[{"name":"aggregate3","type":"function","stateMutability":"payable",
  "inputs":[{"name":"calls","type":"tuple[]","components":[
    {"name":"target","type":"address"},{"name":"allowFailure","type":"bool"},{"name":"callData","type":"bytes"}]}],
  "outputs":[{"name":"returnData","type":"tuple[]","components":[
    {"name":"success","type":"bool"},{"name":"returnData","type":"bytes"}]}]}]`)

type mcCall struct {
	Target       common.Address
	AllowFailure bool
	CallData     []byte
}

type mcResult struct {
	Success    bool
	ReturnData []byte
}

// Multicall runs many reads in one eth_call through Multicall3. A reverting call does not fail the batch; it comes
// back with OK false. Large batches are split so a single eth_call stays under RPC gas caps.
func (c *Client) Multicall(ctx context.Context, calls []Call) ([]Result, error) {
	return c.multicall(ctx, calls, nil)
}

// MulticallAt is Multicall at a given block. Behind a load balancer, "latest" can come from a node a block behind
// the one that announced the head; pinning the block makes such a node fail instead of answering for older state.
func (c *Client) MulticallAt(ctx context.Context, calls []Call, block uint64) ([]Result, error) {
	return c.multicall(ctx, calls, new(big.Int).SetUint64(block))
}

// ArchiveMulticallAt is MulticallAt on the archive RPC, at its pace: for blocks the latest-state RPC no longer keeps.
func (c *Client) ArchiveMulticallAt(ctx context.Context, calls []Call, block uint64) ([]Result, error) {
	at := new(big.Int).SetUint64(block)
	return c.multicallWith(ctx, calls, func(data []byte) ([]byte, error) {
		var out []byte
		err := c.archiveDo(ctx, 1, func() (err error) {
			out, err = c.archiveEth.CallContract(ctx, ethereum.CallMsg{To: &Multicall3, Data: data}, at)
			return err
		})
		return out, err
	})
}

func (c *Client) multicall(ctx context.Context, calls []Call, block *big.Int) ([]Result, error) {
	return c.multicallWith(ctx, calls, func(data []byte) ([]byte, error) { return c.callAt(ctx, Multicall3, data, block) })
}

func (c *Client) multicallWith(ctx context.Context, calls []Call, call func(data []byte) ([]byte, error)) ([]Result, error) {
	out := make([]Result, 0, len(calls))
	const batch = 150
	for start := 0; start < len(calls); start += batch {
		end := min(start+batch, len(calls))
		args := make([]mcCall, 0, end-start)
		for _, cl := range calls[start:end] {
			args = append(args, mcCall{Target: cl.To, AllowFailure: true, CallData: cl.Data})
		}
		data, err := multicallABI.Pack("aggregate3", args)
		if err != nil {
			return nil, err
		}
		raw, err := call(data)
		if err != nil {
			return nil, fmt.Errorf("multicall: %w", err)
		}
		vals, err := multicallABI.Unpack("aggregate3", raw)
		if err != nil {
			return nil, fmt.Errorf("multicall: decode: %w", err)
		}
		res := *abi.ConvertType(vals[0], new([]mcResult)).(*[]mcResult)
		for _, r := range res {
			out = append(out, Result{OK: r.Success, Data: r.ReturnData})
		}
	}
	return out, nil
}

// MustABI parses a JSON ABI and panics on error; for ABIs written into the source.
func MustABI(js string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(js))
	if err != nil {
		panic(err)
	}
	return a
}
