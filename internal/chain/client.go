package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Client is a connection to one network: a latest-state RPC for calls and an archive RPC for old blocks and logs.
// Every call retries transient provider errors (rate limits, overloaded gateways); the archive RPC is also paced
// when its provider needs that.
type Client struct {
	Net Network
	RPC *rpc.Client
	Eth *ethclient.Client

	archive    *rpc.Client
	archiveEth *ethclient.Client
	pace       *pacer
}

// Dial connects to the network's RPCs and checks that they serve the expected chain.
func Dial(ctx context.Context, net Network) (*Client, error) {
	latest, archive, rate := net.Endpoints()
	return DialURLs(ctx, net, latest, archive, rate)
}

// DialURLs connects to explicit endpoints as if they served net. archiveRate paces the archive endpoint in requests
// per second; 0 means no pacing.
func DialURLs(ctx context.Context, net Network, latest, archive string, archiveRate float64) (*Client, error) {
	r, err := dialChecked(ctx, net, latest)
	if err != nil {
		return nil, err
	}
	c := &Client{Net: net, RPC: r, Eth: ethclient.NewClient(r), archive: r, pace: newPacer(archiveRate)}
	if archive != latest {
		if c.archive, err = dialChecked(ctx, net, archive); err != nil {
			r.Close()
			return nil, err
		}
	}
	c.archiveEth = ethclient.NewClient(c.archive)
	return c, nil
}

func dialChecked(ctx context.Context, net Network, endpoint string) (*rpc.Client, error) {
	r, err := rpc.DialContext(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", Host(endpoint), StripURL(err))
	}
	var id hexutil.Big
	if err := retry(ctx, func() error { return r.CallContext(ctx, &id, "eth_chainId") }); err != nil {
		r.Close()
		return nil, fmt.Errorf("%s: eth_chainId: %w", Host(endpoint), err)
	}
	if got := id.ToInt().Int64(); got != net.ChainID {
		r.Close()
		return nil, fmt.Errorf("%s serves chain %d, want %s (%d)", Host(endpoint), got, net.Name, net.ChainID)
	}
	return r, nil
}

// Close closes the connections.
func (c *Client) Close() {
	if c.archive != c.RPC {
		c.archive.Close()
	}
	c.RPC.Close()
}

// Head returns the latest block number.
func (c *Client) Head(ctx context.Context) (uint64, error) {
	var n uint64
	err := retry(ctx, func() (err error) { n, err = c.Eth.BlockNumber(ctx); return err })
	return n, err
}

// SafeHead returns the latest block both endpoints have: archive queries pinned to a block the archive provider has
// not seen yet would fail.
func (c *Client) SafeHead(ctx context.Context) (uint64, error) {
	latest, err := c.Head(ctx)
	if err != nil || c.archive == c.RPC {
		return latest, err
	}
	var archive uint64
	err = c.archiveDo(ctx, 1, func() (err error) { archive, err = c.archiveEth.BlockNumber(ctx); return err })
	if err != nil {
		return 0, err
	}
	return min(latest, archive), nil
}

// BlockTime returns the timestamp of block n.
func (c *Client) BlockTime(ctx context.Context, n uint64) (time.Time, error) {
	var h *types.Header
	err := c.archiveDo(ctx, 1, func() (err error) {
		h, err = c.archiveEth.HeaderByNumber(ctx, new(big.Int).SetUint64(n))
		return err
	})
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(h.Time), 0).UTC(), nil
}

// archiveDo runs fn against the archive RPC: paced for n requests, retried on transient errors.
func (c *Client) archiveDo(ctx context.Context, n int, fn func() error) error {
	return retry(ctx, func() error {
		if err := c.pace.wait(ctx, n); err != nil {
			return err
		}
		return fn()
	})
}

// archiveBatch sends a JSON-RPC batch to the archive RPC.
func (c *Client) archiveBatch(ctx context.Context, elems []rpc.BatchElem) error {
	return c.archiveDo(ctx, len(elems), func() error { return batchOnce(ctx, c.archive, elems) })
}

// pacer spaces requests evenly. A nil pacer does not wait.
type pacer struct {
	mu    sync.Mutex
	next  time.Time
	every time.Duration
}

func newPacer(perSecond float64) *pacer {
	if perSecond <= 0 {
		return nil
	}
	return &pacer{every: time.Duration(float64(time.Second) / perSecond)}
}

// wait blocks until n more requests may be sent.
func (p *pacer) wait(ctx context.Context, n int) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	start := time.Now()
	if p.next.After(start) {
		start = p.next
	}
	p.next = start.Add(time.Duration(n) * p.every)
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Until(start)):
		return nil
	}
}

// Code returns the runtime bytecode at addr (latest block).
func (c *Client) Code(ctx context.Context, addr common.Address) ([]byte, error) {
	var code []byte
	err := retry(ctx, func() (err error) { code, err = c.Eth.CodeAt(ctx, addr, nil); return err })
	return code, err
}

// Codes returns the bytecode of each address, in batches of JSON-RPC requests.
func (c *Client) Codes(ctx context.Context, addrs []common.Address) ([][]byte, error) {
	out := make([][]byte, len(addrs))
	const batch = 50
	for start := 0; start < len(addrs); start += batch {
		end := min(start+batch, len(addrs))
		res := make([]hexutil.Bytes, end-start)
		elems := make([]rpc.BatchElem, end-start)
		for i := range elems {
			elems[i] = rpc.BatchElem{Method: "eth_getCode", Args: []any{addrs[start+i], "latest"}, Result: &res[i]}
		}
		if err := c.batch(ctx, elems); err != nil {
			return nil, err
		}
		for i, e := range elems {
			// A failed element must not read as "no code": that would turn contracts into wallets.
			if e.Error != nil {
				return nil, fmt.Errorf("eth_getCode %s: %w", addrs[start+i], e.Error)
			}
			out[start+i] = res[i]
		}
	}
	return out, nil
}

// Storage reads one storage slot (latest block).
func (c *Client) Storage(ctx context.Context, addr common.Address, slot common.Hash) (common.Hash, error) {
	var v []byte
	err := retry(ctx, func() (err error) { v, err = c.Eth.StorageAt(ctx, addr, slot, nil); return err })
	return common.BytesToHash(v), err
}

// Storages reads several storage slots of one contract in a single batch (latest block).
func (c *Client) Storages(ctx context.Context, addr common.Address, slots []common.Hash) ([]common.Hash, error) {
	res := make([]common.Hash, len(slots))
	elems := make([]rpc.BatchElem, len(slots))
	for i, s := range slots {
		elems[i] = rpc.BatchElem{Method: "eth_getStorageAt", Args: []any{addr, s, "latest"}, Result: &res[i]}
	}
	if err := c.batch(ctx, elems); err != nil {
		return nil, err
	}
	for i, e := range elems {
		if e.Error != nil {
			return nil, fmt.Errorf("eth_getStorageAt %s: %w", slots[i], e.Error)
		}
	}
	return res, nil
}

// Nonce returns the transaction count of addr at the latest block.
func (c *Client) Nonce(ctx context.Context, addr common.Address) (uint64, error) {
	var n uint64
	err := retry(ctx, func() (err error) { n, err = c.Eth.NonceAt(ctx, addr, nil); return err })
	return n, err
}

// Balance returns the ETH balance of addr at the latest block.
func (c *Client) Balance(ctx context.Context, addr common.Address) (*big.Int, error) {
	var b *big.Int
	err := retry(ctx, func() (err error) { b, err = c.Eth.BalanceAt(ctx, addr, nil); return err })
	return b, err
}

// CallFrom runs eth_call as from (the zero address means no sender) against the latest block. A revert comes back
// as an error.
func (c *Client) CallFrom(ctx context.Context, from, to common.Address, data []byte) ([]byte, error) {
	msg := ethereum.CallMsg{From: from, To: &to, Data: data}
	var out []byte
	err := retry(ctx, func() (err error) { out, err = c.Eth.CallContract(ctx, msg, nil); return err })
	return out, err
}

// SimResult is the outcome of one call in a simulated block.
type SimResult struct {
	OK         bool
	GasUsed    uint64
	ReturnData []byte
	Logs       int // events the call emitted
}

// Simulate runs each sequence of calls with eth_simulateV1 on top of the latest block. Calls in one sequence see
// each other's state changes; separate sequences are independent. Signatures, nonces and balances are not checked.
func (c *Client) Simulate(ctx context.Context, seqs [][]ethereum.CallMsg) ([][]SimResult, error) {
	type call struct {
		From common.Address  `json:"from"`
		To   *common.Address `json:"to"`
		Data hexutil.Bytes   `json:"data"`
	}
	type block struct {
		Calls []struct {
			Status     hexutil.Uint64    `json:"status"`
			GasUsed    hexutil.Uint64    `json:"gasUsed"`
			ReturnData hexutil.Bytes     `json:"returnData"`
			Logs       []json.RawMessage `json:"logs"`
		} `json:"calls"`
	}
	out := make([][]SimResult, len(seqs))
	const batch = 25
	for start := 0; start < len(seqs); start += batch {
		end := min(start+batch, len(seqs))
		res := make([][]block, end-start)
		elems := make([]rpc.BatchElem, end-start)
		for i, seq := range seqs[start:end] {
			calls := make([]call, len(seq))
			for j, m := range seq {
				calls[j] = call{From: m.From, To: m.To, Data: m.Data}
			}
			opts := map[string]any{"blockStateCalls": []map[string]any{{"calls": calls}}, "validation": false}
			elems[i] = rpc.BatchElem{Method: "eth_simulateV1", Args: []any{opts, "latest"}, Result: &res[i]}
		}
		if err := c.batch(ctx, elems); err != nil {
			return nil, err
		}
		for i, e := range elems {
			if e.Error != nil {
				return nil, fmt.Errorf("eth_simulateV1: %w", e.Error)
			}
			if len(res[i]) != 1 || len(res[i][0].Calls) != len(seqs[start+i]) {
				return nil, errors.New("eth_simulateV1: unexpected result shape")
			}
			for _, cl := range res[i][0].Calls {
				out[start+i] = append(out[start+i], SimResult{
					OK: cl.Status == 1, GasUsed: uint64(cl.GasUsed), ReturnData: cl.ReturnData, Logs: len(cl.Logs),
				})
			}
		}
	}
	return out, nil
}

// batch sends a JSON-RPC batch to the latest-state RPC.
func (c *Client) batch(ctx context.Context, elems []rpc.BatchElem) error {
	return retry(ctx, func() error { return batchOnce(ctx, c.RPC, elems) })
}

// batchOnce sends a batch and reports a transient error on any element as an error, so retry resends the batch.
func batchOnce(ctx context.Context, r *rpc.Client, elems []rpc.BatchElem) error {
	for i := range elems {
		elems[i].Error = nil
	}
	if err := r.BatchCallContext(ctx, elems); err != nil {
		return err
	}
	for _, e := range elems {
		if e.Error != nil && isTransient(e.Error) {
			return e.Error
		}
	}
	return nil
}

// retry runs fn until it succeeds, fails with an error that is not transient, or runs out of attempts.
// Errors leave it with endpoint URLs cut down to their host (see stripURL).
func retry(ctx context.Context, fn func() error) error {
	delay := 500 * time.Millisecond
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || !isTransient(err) || attempt == 5 {
			return StripURL(err)
		}
		select {
		case <-ctx.Done():
			return StripURL(errors.Join(err, ctx.Err()))
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// StripURL rewrites a transport error so it names the endpoint's host only. net/http quotes the full URL, and
// keyed providers (Alchemy, Infura) put the API key in the path; these errors end up in reports.
func StripURL(err error) error {
	var ue *url.Error
	if err == nil || !errors.As(err, &ue) {
		return err
	}
	return fmt.Errorf("%s %s: %w", ue.Op, Host(ue.URL), ue.Err)
}

// Host returns the scheme and host of an endpoint URL, without path or query (where keys live).
func Host(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "(endpoint)"
	}
	return u.Scheme + "://" + u.Host
}

// isTransient reports whether an error is the provider's momentary trouble (rate limits, overloaded gateways,
// dropped connections) rather than an answer, so the request is worth sending again.
func isTransient(err error) bool {
	var httpErr rpc.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case 429, 502, 503, 504:
			return true
		}
	}
	s := strings.ToLower(err.Error())
	for _, sign := range []string{
		"429", "rate limit", "too many requests", "capacity exceeded",
		"502", "503", "504", "bad gateway", "gateway time", "service unavailable", "temporarily unavailable",
		"connection reset", "unexpected eof",
	} {
		if strings.Contains(s, sign) {
			return true
		}
	}
	return false
}
