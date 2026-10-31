package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

type rpcReq struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// fakeRPC serves JSON-RPC (single and batch requests) from handle, answering eth_chainId itself.
func fakeRPC(t *testing.T, net Network, handle func(method string, params []json.RawMessage) (any, *rpcErr)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		answer := func(q rpcReq) map[string]any {
			out := map[string]any{"jsonrpc": "2.0", "id": q.ID}
			if q.Method == "eth_chainId" {
				out["result"] = hexutil.EncodeUint64(uint64(net.ChainID))
				return out
			}
			res, e := handle(q.Method, q.Params)
			if e != nil {
				out["error"] = e
			} else {
				out["result"] = res
			}
			return out
		}
		var batch []rpcReq
		if json.Unmarshal(body, &batch) == nil {
			var outs []map[string]any
			for _, q := range batch {
				outs = append(outs, answer(q))
			}
			_ = json.NewEncoder(w).Encode(outs)
			return
		}
		var q rpcReq
		if err := json.Unmarshal(body, &q); err != nil {
			t.Errorf("bad request: %s", body)
			return
		}
		_ = json.NewEncoder(w).Encode(answer(q))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func blockParam(t *testing.T, raw json.RawMessage) uint64 {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("block param %s: %v", raw, err)
	}
	n, err := hexutil.DecodeUint64(s)
	if err != nil {
		t.Fatalf("block param %s: %v", s, err)
	}
	return n
}

func TestScanLogsShrinksTheWindowAndCoversEveryBlockOnce(t *testing.T) {
	net := Ethereum
	net.LogWindow = 1000
	const limit = 300 // the fake provider rejects larger ranges
	var mu sync.Mutex
	seen := map[uint64]int{}
	url := fakeRPC(t, net, func(method string, params []json.RawMessage) (any, *rpcErr) {
		var q struct{ FromBlock, ToBlock string }
		_ = json.Unmarshal(params[0], &q)
		from, _ := hexutil.DecodeUint64(q.FromBlock)
		to, _ := hexutil.DecodeUint64(q.ToBlock)
		if to-from+1 > limit {
			return nil, &rpcErr{-32602, "block range too large"}
		}
		mu.Lock()
		for b := from; b <= to; b++ {
			seen[b]++
		}
		mu.Unlock()
		return []types.Log{}, nil
	})
	c, err := DialURLs(context.Background(), net, url, url, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.ScanLogs(context.Background(), LogScan{From: 100, To: 2099}, func([]types.Log) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for b := uint64(100); b <= 2099; b++ {
		if seen[b] != 1 {
			t.Fatalf("block %d scanned %d times", b, seen[b])
		}
	}
	if len(seen) != 2000 {
		t.Fatalf("scanned %d blocks, want 2000", len(seen))
	}
}

func TestScanLogsRefusesARangeOverBudgetUpFront(t *testing.T) {
	net := Ethereum
	net.LogWindow = 100
	calls := 0
	url := fakeRPC(t, net, func(string, []json.RawMessage) (any, *rpcErr) { calls++; return []types.Log{}, nil })
	c, err := DialURLs(context.Background(), net, url, url, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.ScanLogs(context.Background(), LogScan{From: 0, To: 10_000, MaxRequests: 50, Query: ethereum.FilterQuery{}},
		func([]types.Log) error { return nil })
	if !errors.Is(err, ErrBudget) || calls != 0 {
		t.Fatalf("err = %v after %d calls; want ErrBudget before any call", err, calls)
	}
}

func TestCreationBlockFindsTheFirstBlockWithANonce(t *testing.T) {
	const created, head = 12_345_678, 26_000_000
	for _, rate := range []float64{0, 1000} { // batched k-ary search, then paced binary search
		net := Ethereum
		requests := 0
		url := fakeRPC(t, net, func(method string, params []json.RawMessage) (any, *rpcErr) {
			requests++
			if method != "eth_getTransactionCount" {
				return nil, &rpcErr{-32601, "unexpected " + method}
			}
			if blockParam(t, params[1]) >= created {
				return "0x1", nil
			}
			return "0x0", nil
		})
		c, err := DialURLs(context.Background(), net, url, url, rate)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.CreationBlock(context.Background(), [20]byte{1}, head)
		c.Close()
		if err != nil || got != created {
			t.Fatalf("rate %v: CreationBlock = %d, %v; want %d", rate, got, err, created)
		}
		t.Logf("rate %v: %d requests", rate, requests)
	}
}

func TestPacerSpacesRequests(t *testing.T) {
	p := newPacer(200) // 5ms apart
	start := time.Now()
	for range 3 {
		if err := p.wait(context.Background(), 2); err != nil {
			t.Fatal(err)
		}
	}
	// Three waits of two requests each: the third may start after 4 slots (20ms).
	if d := time.Since(start); d < 20*time.Millisecond {
		t.Errorf("3 paced waits took %s, want at least 20ms", d)
	}
	if newPacer(0) != nil {
		t.Error("rate 0 should mean no pacer")
	}
	var nilPacer *pacer
	if err := nilPacer.wait(context.Background(), 100); err != nil {
		t.Error(err)
	}
}

func TestIsTransient(t *testing.T) {
	for _, msg := range []string{
		"429 Too Many Requests", "rate limit exceeded", "Capacity exceeded",
		`504 Gateway Timeout: {"title":"Error 504: Gateway time-out"}`, "error code -32603: service temporarily unavailable",
		"read tcp: connection reset by peer",
	} {
		if !isTransient(errors.New(msg)) {
			t.Errorf("%q not seen as transient", msg)
		}
	}
	if isTransient(errors.New("execution reverted")) {
		t.Error("revert seen as transient")
	}
	if !isTransient(rpc.HTTPError{StatusCode: 503}) {
		t.Error("HTTP 503 not seen as transient")
	}
}

func TestStripURLKeepsOnlyTheHost(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://base-mainnet.g.alchemy.com/v2/SECRETKEY", Err: context.DeadlineExceeded}
	got := StripURL(fmt.Errorf("holders: %w", err))
	if s := got.Error(); strings.Contains(s, "SECRETKEY") || !strings.Contains(s, "https://base-mainnet.g.alchemy.com") {
		t.Errorf("stripURL = %q", s)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Error("stripURL lost the underlying error")
	}
}

func TestCreatorNeedsABirthEventForFactoryCreations(t *testing.T) {
	token := common.HexToAddress("0x00000000000000000000000000000000000000c0")
	factory := common.HexToAddress("0x00000000000000000000000000000000000000fa")
	scammer := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	stranger := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	approval := crypto.Keccak256Hash([]byte("Approval(address,address,uint256)"))
	receipt := func(tx byte, from common.Address, topics ...common.Hash) map[string]any {
		return map[string]any{
			"transactionHash": common.Hash{tx}, "from": from, "to": factory, "contractAddress": nil,
			"logs": []map[string]any{{"address": token, "topics": topics}},
		}
	}
	for _, c := range []struct {
		name     string
		receipts []map[string]any
		want     common.Address
	}{
		// The first transaction to make the contract log minted from the zero address: that is the creation.
		{"birth event", []map[string]any{
			receipt(1, scammer, transferTopic, zeroAddressTopic, common.BytesToHash(scammer[:])),
			receipt(2, stranger, approval, common.BytesToHash(stranger[:])),
		}, scammer},
		// Only a later interaction logged: no deployer rather than the stranger.
		{"no birth event", []map[string]any{receipt(1, stranger, approval, common.BytesToHash(stranger[:]))}, common.Address{}},
		// A mint from the zero address after another transaction already touched the contract is not its birth.
		{"birth-looking event later in the block", []map[string]any{
			receipt(1, stranger, approval, common.BytesToHash(stranger[:])),
			receipt(2, stranger, transferTopic, zeroAddressTopic, common.BytesToHash(stranger[:])),
		}, common.Address{}},
	} {
		url := fakeRPC(t, Ethereum, func(method string, _ []json.RawMessage) (any, *rpcErr) { return c.receipts, nil })
		cl, err := DialURLs(context.Background(), Ethereum, url, url, 0)
		if err != nil {
			t.Fatal(err)
		}
		cr, err := cl.Creator(context.Background(), token, 1)
		cl.Close()
		got := common.Address{}
		if err == nil {
			got = cr.Deployer
		}
		if got != c.want {
			t.Errorf("%s: deployer = %s (err %v), want %s", c.name, got, err, c.want)
		}
	}
}

func TestBlockAtFindsTheLastBlockBeforeATime(t *testing.T) {
	// 12 s slots with every 37th one missed: block n's time drifts from a straight line.
	const head = 2_000_000
	genesis := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	timeOf := func(n uint64) time.Time { return genesis.Add(time.Duration(12*(n+n/37)) * time.Second) }
	var reads int
	url := fakeRPC(t, Ethereum, func(method string, params []json.RawMessage) (any, *rpcErr) {
		switch method {
		case "eth_blockNumber":
			return hexutil.EncodeUint64(head), nil
		case "eth_getBlockByNumber":
			reads++
			n := blockParam(t, params[0])
			return &types.Header{Number: new(big.Int).SetUint64(n), Time: uint64(timeOf(n).Unix()), Difficulty: new(big.Int)}, nil
		}
		return nil, &rpcErr{-32601, "unexpected " + method}
	})
	c, err := DialURLs(context.Background(), Ethereum, url, url, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, c2 := range []struct {
		target uint64
		after  uint64
	}{{1_234_567, 1_234_000}, {1_999_998, 0}, {37 * 1000, 900_000}, {1_500_000, 1_500_000}} {
		reads = 0
		// A moment just after the target block, before the next one.
		got, ok, err := c.BlockAt(context.Background(), timeOf(c2.target).Add(5*time.Second), c2.after)
		if err != nil || !ok || got != c2.target {
			t.Errorf("BlockAt(time of %d +5s, after %d) = %d, %v, %v", c2.target, c2.after, got, ok, err)
		}
		if reads > 60 {
			t.Errorf("BlockAt(%d) took %d header reads", c2.target, reads)
		}
	}
	if _, ok, err := c.BlockAt(context.Background(), timeOf(head).Add(time.Hour), 0); ok || err != nil {
		t.Errorf("a time after the head: ok %v, err %v", ok, err)
	}
}

func TestBlockTimeAsksTheUnpacedRPCFirst(t *testing.T) {
	header := func(n uint64) *types.Header {
		return &types.Header{Number: new(big.Int).SetUint64(n), Time: 1_700_000_000 + n, Difficulty: new(big.Int)}
	}
	var archiveReads int
	latest := fakeRPC(t, Base, func(method string, params []json.RawMessage) (any, *rpcErr) {
		if n := blockParam(t, params[0]); n < 100 {
			return header(n), nil
		}
		return nil, nil // a block the node has not seen yet
	})
	archive := fakeRPC(t, Base, func(method string, params []json.RawMessage) (any, *rpcErr) {
		archiveReads++
		return header(blockParam(t, params[0])), nil
	})
	c, err := DialURLs(context.Background(), Base, latest, archive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, err := c.BlockTime(context.Background(), 42); err != nil || got.Unix() != 1_700_000_042 || archiveReads != 0 {
		t.Errorf("block 42: %v, %v, %d archive reads", got, err, archiveReads)
	}
	if got, err := c.BlockTime(context.Background(), 150); err != nil || got.Unix() != 1_700_000_150 || archiveReads != 1 {
		t.Errorf("block 150: %v, %v, %d archive reads", got, err, archiveReads)
	}
}
