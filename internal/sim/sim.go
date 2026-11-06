// Package sim buys and then sells a token on a local fork of the chain, the way a new buyer would, and reports
// whether the sale goes through and what the token took on the way in and out. Nothing is sent to the real network.
package sim

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/amirradjou/exitcheck/internal/anvil"
	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/facts"
)

// Verdicts. The first four are flags: a buyer would lose money or be stuck.
const (
	Honeypot        = "honeypot"         // selling reverts, or the token keeps most of a sale
	SellLimited     = "sell-limited"     // only part of a position can be sold at once
	TransferBlocked = "transfer-blocked" // tokens cannot be moved to another wallet
	HighTax         = "high-tax"         // the token takes 10% or more of a buy, sell or transfer
	OK              = "ok"
	CannotBuy       = "cannot-buy" // buying reverts: trading closed, or new buyers blocked
	NoPool          = "no-pool"    // no pool with liquidity to trade against
)

// BlindSpot is stated with every result: a clean simulation is evidence, not a guarantee.
const BlindSpot = "a contract can treat later buyers differently (time switches, allowlists, limits it can change), so a clean simulation is not a guarantee"

const (
	honeypotTax = 0.5
	highTax     = 0.1
	// noTax absorbs rounding: a measured tax below it is reported as zero.
	noTax = 0.005
)

// Options change how the simulation runs.
type Options struct {
	// Block forks this block instead of the latest one (needs the archive RPC).
	Block uint64
}

// Step is one transaction of the simulation.
type Step struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Tax is what the token itself took: 1 - received/expected, where expected is the DEX's own quote.
	Tax *float64 `json:"tax,omitempty"`
	Gas uint64   `json:"gas,omitempty"`
}

// Result is the outcome of one simulation.
type Result struct {
	Chain    string         `json:"chain"`
	Token    common.Address `json:"token"`
	Block    uint64         `json:"block"`
	Venue    string         `json:"venue,omitempty"`
	SpentETH float64        `json:"spent_eth,omitempty"`
	Buy      *Step          `json:"buy,omitempty"`
	Transfer *Step          `json:"transfer,omitempty"`
	Sell     *Step          `json:"sell,omitempty"`
	// SellHalf is tried only when selling the whole position reverts.
	SellHalf  *Step         `json:"sell_half,omitempty"`
	Verdict   string        `json:"verdict"`
	Reasons   []string      `json:"reasons"`
	BlindSpot string        `json:"blind_spot"`
	Elapsed   time.Duration `json:"elapsed_ns"`
}

// Flagged reports whether the verdict means a buyer would lose money or be stuck.
func (r *Result) Flagged() bool {
	switch r.Verdict {
	case Honeypot, SellLimited, TransferBlocked, HighTax:
		return true
	}
	return false
}

// Run forks the chain behind c, buys token with a fresh wallet through its deepest suitable pool, moves some to a
// second wallet, then sells. It fails only when the fork cannot be set up; a token that cannot be traded gets a
// verdict saying so.
func Run(ctx context.Context, c *chain.Client, token common.Address, opts Options) (*Result, error) {
	start := time.Now()
	f, block, err := startFork(ctx, c, opts.Block)
	if err != nil {
		return nil, err
	}
	defer f.close()
	r := &Result{Chain: c.Net.Name, Token: token, Block: block, BlindSpot: BlindSpot, Reasons: []string{}}
	defer func() { r.Elapsed = time.Since(start) }()
	if err := simulate(ctx, c, f, token, r); err != nil {
		return nil, err
	}
	// Every verdict, the early "no pool" ones included, passes this gate.
	if err := unreliable(ctx, f.broken); err != nil {
		return nil, err
	}
	return r, nil
}

// simulate picks the pool and trades through it, filling r.
func simulate(ctx context.Context, c *chain.Client, f *fork, token common.Address, r *Result) error {
	pools, err := facts.Pools(ctx, f.client, token)
	if err != nil {
		return fmt.Errorf("pools on the fork: %w", err)
	}
	pool, ok := pickPool(pools)
	if !ok {
		r.Verdict = NoPool
		r.Reasons = append(r.Reasons, "no pool against WETH, USDC or USDT has liquidity")
		return nil
	}
	v, err := newVenue(c.Net, token, pool)
	if err != nil {
		r.Verdict = NoPool
		r.Reasons = append(r.Reasons, err.Error())
		return nil
	}
	r.Venue = v.describe()
	spend := spendFor(ctx, f, c.Net, pool)
	r.SpentETH = weiToETH(spend)
	return trade(ctx, f, token, v, spend, r)
}

// unreliable says why a finished simulation tells nothing about the token: a call failed outside the EVM (the fork
// could not fetch state, a connection dropped), or the budget ran out while it traded. Either way a step failed for
// the simulation's own reason, and a timed-out approve reads exactly like a sale that reverts: such a result must
// never become a verdict.
func unreliable(ctx context.Context, broken error) error {
	if broken != nil {
		return fmt.Errorf("a call failed outside the EVM, so the result would say nothing about the token: %w", chain.StripURL(broken))
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("the simulation ran out of time before it finished, so the result would say nothing about the token: %w", err)
	}
	return nil
}

// pickPool chooses the pool to trade. Pools whose routers handle fee-on-transfer tokens (v2, Aerodrome) come
// first: Uniswap v3 rejects a token that takes a fee as it moves, which would look like a honeypot when it is only
// a tax. WETH pools come before USDC/USDT ones (one hop fewer). A preferred pool must hold at least a twentieth of
// the deepest pool's value; a dust pool says little about the token. Drained pools still count down to a cent:
// a honeypot's pool is often nearly empty.
func pickPool(pools []facts.Pool) (facts.Pool, bool) {
	var live []facts.Pool
	for _, p := range pools {
		if p.USD >= 0.01 {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		return facts.Pool{}, false
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].USD > live[j].USD })
	score := func(p facts.Pool) int {
		s := 0
		if p.DEX != "uniswap-v3" {
			s += 2
		}
		if p.Quote == "WETH" {
			s++
		}
		return s
	}
	best := live[0]
	for _, p := range live[1:] {
		if p.USD*20 >= live[0].USD && score(p) > score(best) {
			best = p
		}
	}
	return best, true
}

// spendFor sizes the buy: 0.5% of the pool's quote reserve, at most 0.05 ETH. Small enough to stay under per-wallet
// and per-transaction limits, large enough that taxes are measurable.
func spendFor(ctx context.Context, f *fork, net chain.Network, p facts.Pool) *big.Int {
	maxSpend := big.NewInt(5e16)
	ethValue := p.QuoteAmount // in ETH for WETH pools
	if p.Quote != "WETH" {
		ethValue = p.USD / ethUSD(ctx, f, net)
	}
	wei, _ := new(big.Float).SetFloat64(ethValue * 0.005 * 1e18).Int(nil)
	if wei.Cmp(maxSpend) > 0 {
		return maxSpend
	}
	if wei.Sign() <= 0 {
		return big.NewInt(1)
	}
	return wei
}

// ethUSD prices ETH through the first v2 DEX's WETH/USDC pair; 3000 if that fails (only used to size a buy).
func ethUSD(ctx context.Context, f *fork, net chain.Network) float64 {
	usdc := net.Quotes[1]
	out, err := f.call(ctx, net.V2[0].Router, pack(v2RouterABI, "getAmountsOut", big.NewInt(1e18), []common.Address{net.WETH, usdc.Address}))
	if err == nil {
		if vals, err := v2RouterABI.Methods["getAmountsOut"].Outputs.Unpack(out); err == nil {
			amounts := vals[0].([]*big.Int)
			if p, _ := new(big.Float).Quo(new(big.Float).SetInt(amounts[1]), big.NewFloat(1e6)).Float64(); p > 0 {
				return p
			}
		}
	}
	return 3000
}

// trade runs the buy, transfer and sell steps and sets the verdict.
func trade(ctx context.Context, f *fork, token common.Address, v venue, spend *big.Int, r *Result) error {
	buyer, other := randomAddress(), randomAddress()
	if err := f.node.SetBalance(ctx, buyer, new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18))); err != nil {
		return err
	}

	// Buy.
	expected, err := v.quoteBuy(ctx, f, spend)
	f.blame(err)
	if err != nil || expected.Sign() == 0 {
		r.Verdict = NoPool
		r.Reasons = append(r.Reasons, "the DEX cannot quote a buy: "+errText(err))
		return nil
	}
	r.Buy = f.send(ctx, buyer, v.spender(), spend, v.buy(buyer, spend)).Step
	if !r.Buy.OK {
		r.Verdict = CannotBuy
		r.Reasons = append(r.Reasons, "buying reverts: "+r.Buy.Error)
		return nil
	}
	got, err := f.tokenBalance(ctx, token, buyer)
	if err != nil {
		return err
	}
	r.Buy.Tax = taxOf(got, expected)
	if got.Sign() == 0 {
		r.Verdict = Honeypot
		r.Reasons = append(r.Reasons, "the buy went through but delivered no tokens")
		return nil
	}

	// Let cooldowns and same-block anti-bot rules pass, as they would for a real buyer.
	if err := f.node.Advance(ctx, time.Minute, 3); err != nil {
		return err
	}

	// Transfer to another wallet, then undo it so the sale starts from the whole position.
	snap, err := f.node.Snapshot(ctx)
	if err != nil {
		return err
	}
	half := new(big.Int).Rsh(got, 1)
	r.Transfer = f.send(ctx, buyer, token, nil, pack(erc20ABI, "transfer", other, half)).Step
	if r.Transfer.OK {
		arrived, err := f.tokenBalance(ctx, token, other)
		if err != nil {
			return err
		}
		r.Transfer.Tax = taxOf(arrived, half)
	}
	if err := f.node.Revert(ctx, snap); err != nil {
		return err
	}

	// Sell everything; if that reverts, try half.
	if approve := f.send(ctx, buyer, token, nil, pack(erc20ABI, "approve", v.spender(), abi.MaxUint256)); !approve.OK {
		r.Sell = &Step{Error: "approve reverted: " + approve.Error}
	} else {
		r.Sell = f.sell(ctx, token, v, buyer, got)
		if !r.Sell.OK {
			r.SellHalf = f.sell(ctx, token, v, buyer, half)
		}
	}
	verdict(r)
	return nil
}

// verdict turns the steps into a verdict and reasons.
func verdict(r *Result) {
	add := func(format string, args ...any) { r.Reasons = append(r.Reasons, fmt.Sprintf(format, args...)) }
	tax := func(s *Step) float64 {
		if s == nil || s.Tax == nil {
			return 0
		}
		return *s.Tax
	}
	switch {
	case !r.Sell.OK && (r.SellHalf == nil || !r.SellHalf.OK):
		r.Verdict = Honeypot
		add("selling reverts: %s", r.Sell.Error)
	case !r.Sell.OK:
		r.Verdict = SellLimited
		add("selling the whole position reverts (%s); half of it sells", r.Sell.Error)
	case tax(r.Sell) >= honeypotTax:
		r.Verdict = Honeypot
		add("a sale keeps only %.0f%% of its value: the token takes %.0f%%", 100*(1-tax(r.Sell)), 100*tax(r.Sell))
	case r.Transfer != nil && !r.Transfer.OK:
		r.Verdict = TransferBlocked
		add("sending tokens to another wallet reverts: %s", r.Transfer.Error)
	case tax(r.Buy) >= highTax || tax(r.Sell) >= highTax || tax(r.Transfer) >= highTax:
		r.Verdict = HighTax
	default:
		r.Verdict = OK
	}
	for _, s := range []struct {
		name string
		step *Step
	}{{"buy", r.Buy}, {"sell", r.Sell}, {"transfer", r.Transfer}} {
		if t := tax(s.step); t > 0 {
			add("%s tax %.1f%%", s.name, 100*t)
		}
	}
	for _, s := range []*Step{r.Buy, r.Sell} {
		if s != nil && s.Gas > 2_000_000 {
			add("a swap used %d gas (an ordinary one uses under 300k)", s.Gas)
		}
	}
}

// taxOf returns 1 - got/want, clamped to [0, 1]; rounding noise below noTax reads as zero.
func taxOf(got, want *big.Int) *float64 {
	if want.Sign() == 0 {
		return nil
	}
	ratio, _ := new(big.Rat).SetFrac(got, want).Float64()
	t := min(max(1-ratio, 0), 1)
	if t < noTax {
		t = 0
	}
	return &t
}

// fork is a running anvil fork and a chain client on it.
type fork struct {
	node   *anvil.Node
	client *chain.Client
	// broken is the first call that failed outside the EVM (see blame). A step that failed that way says nothing
	// about the token, so the run ends with this error instead of a verdict.
	broken error
}

// blame records err as the simulation's own failure unless the EVM itself failed: only a revert, running out of gas
// or a bad opcode is the token's doing. A timeout, a dropped connection or a fork that cannot fetch state says
// nothing about the token, so it must not become a step's verdict.
func (f *fork) blame(err error) {
	if err != nil && f.broken == nil && (isForkFailure(err) || !isEVMFailure(err)) {
		f.broken = err
	}
}

// isEVMFailure reports whether a call failed inside the EVM: a revert (with or without a reason or custom error),
// out of gas, or an invalid instruction.
func isEVMFailure(err error) bool {
	var de rpc.DataError
	if errors.As(err, &de) && de.ErrorData() != nil {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{"revert", "out of gas", "outofgas", "invalid opcode", "opcodenotfound", "invalidfeopcode",
		"stack underflow", "stack overflow", "invalid jump"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// isForkFailure reports whether an error comes from anvil failing to reach its upstream RPC.
func isForkFailure(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Fork Error") || strings.Contains(s, "failed to get account") ||
		strings.Contains(s, "Transport(")
}

// startFork forks the given block, or the latest one. Recent blocks fork from the latest-state RPC (generous
// limits); older ones need the archive RPC, at its pace.
func startFork(ctx context.Context, c *chain.Client, block uint64) (*fork, uint64, error) {
	latest, archive, rate := c.Net.Endpoints()
	head, err := c.Head(ctx)
	if err != nil {
		return nil, 0, err
	}
	f := anvil.Fork{URL: latest, Block: head}
	if block != 0 {
		f.Block = block
	}
	if c.Net.ForkFromArchive || head-f.Block > 64 {
		f.URL = archive
		if rate > 0 {
			// About 17 compute units per storage read.
			f.ComputeUnits = int(rate * 17)
		}
	}
	node, err := anvil.Start(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	fc, err := chain.DialURLs(ctx, c.Net, node.URL, node.URL, 0)
	if err != nil {
		node.Close()
		return nil, 0, err
	}
	return &fork{node: node, client: fc}, f.Block, nil
}

func (f *fork) close() {
	f.client.Close()
	f.node.Close()
}

func (f *fork) call(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
	return f.client.CallFrom(ctx, common.Address{}, to, data)
}

var erc20ABI = chain.MustABI(`[
	{"name":"balanceOf","type":"function","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
	{"name":"transfer","type":"function","stateMutability":"nonpayable","inputs":[{"type":"address"},{"type":"uint256"}],"outputs":[{"type":"bool"}]},
	{"name":"approve","type":"function","stateMutability":"nonpayable","inputs":[{"type":"address"},{"type":"uint256"}],"outputs":[{"type":"bool"}]}]`)

func (f *fork) tokenBalance(ctx context.Context, token, holder common.Address) (*big.Int, error) {
	out, err := f.call(ctx, token, pack(erc20ABI, "balanceOf", holder))
	if err != nil {
		return nil, err
	}
	if len(out) < 32 {
		return nil, errors.New("balanceOf returned no value")
	}
	return new(big.Int).SetBytes(out[:32]), nil
}

// sell sells tokens through v and measures what came back (ETH net of gas, plus WETH) against the DEX's quote.
func (f *fork) sell(ctx context.Context, token common.Address, v venue, seller common.Address, tokens *big.Int) *Step {
	expected, err := v.quoteSell(ctx, f, tokens)
	if err != nil {
		f.blame(err)
		return &Step{Error: "the DEX cannot quote the sale: " + errText(err)}
	}
	weth := f.client.Net.WETH
	ethBefore, err1 := f.client.Balance(ctx, seller)
	wethBefore, err2 := f.tokenBalance(ctx, weth, seller)
	if err := errors.Join(err1, err2); err != nil {
		f.blame(err)
		return &Step{Error: err.Error()}
	}
	s := f.send(ctx, seller, v.spender(), nil, v.sell(seller, tokens))
	if !s.OK {
		return s.Step
	}
	ethAfter, err1 := f.client.Balance(ctx, seller)
	wethAfter, err2 := f.tokenBalance(ctx, weth, seller)
	if err := errors.Join(err1, err2); err != nil {
		f.blame(err)
		return &Step{Error: err.Error()}
	}
	got := new(big.Int).Sub(ethAfter, ethBefore)
	got.Add(got, s.feePaid)
	got.Add(got, new(big.Int).Sub(wethAfter, wethBefore))
	if expected.Sign() > 0 {
		s.Tax = taxOf(got, expected)
	}
	return s.Step
}

// sent is a mined (or refused) transaction and the ETH it paid for gas.
type sent struct {
	*Step
	feePaid *big.Int
}

// send runs a transaction from any account (the fork impersonates it). It is first tried with eth_call, which
// returns the revert reason when it fails; only a call that would succeed is mined.
func (f *fork) send(ctx context.Context, from, to common.Address, value *big.Int, data []byte) sent {
	fail := func(err error) sent {
		f.blame(err)
		return sent{Step: &Step{Error: revertReason(err)}, feePaid: new(big.Int)}
	}
	msg := ethereum.CallMsg{From: from, To: &to, Value: value, Data: data}
	if _, err := f.client.Eth.CallContract(ctx, msg, nil); err != nil {
		return fail(err)
	}
	gas, err := f.client.Eth.EstimateGas(ctx, msg)
	if err != nil {
		return fail(err)
	}
	tx := map[string]any{"from": from, "to": to, "data": hexutil.Bytes(data), "gas": hexutil.Uint64(gas + gas/2)}
	if value != nil {
		tx["value"] = (*hexutil.Big)(value)
	}
	var hash common.Hash
	if err := f.node.RPC.CallContext(ctx, &hash, "eth_sendTransaction", tx); err != nil {
		return fail(err)
	}
	receipt, err := f.receipt(ctx, hash)
	if err != nil {
		return fail(err)
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	if receipt.Status != 1 {
		return sent{Step: &Step{Error: "transaction reverted", Gas: receipt.GasUsed}, feePaid: fee}
	}
	return sent{Step: &Step{OK: true, Gas: receipt.GasUsed}, feePaid: fee}
}

// receipt waits for a transaction's receipt: anvil mines on send, but may answer with the hash first.
func (f *fork) receipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	for range 100 {
		r, err := f.client.Eth.TransactionReceipt(ctx, hash)
		if err == nil {
			return r, nil
		}
		if !errors.Is(err, ethereum.NotFound) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("transaction %s was not mined", hash)
}

// revertReason extracts the reason string from a reverted call, or the error text.
func revertReason(err error) string {
	var de rpc.DataError
	if errors.As(err, &de) {
		if s, ok := de.ErrorData().(string); ok {
			if data, derr := hexutil.Decode(s); derr == nil {
				if reason, uerr := abi.UnpackRevert(data); uerr == nil {
					return reason
				}
				if len(data) >= 4 {
					return fmt.Sprintf("custom error 0x%x", data[:4])
				}
			}
		}
	}
	msg := strings.TrimPrefix(err.Error(), "execution reverted: ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

func errText(err error) string {
	if err == nil {
		return "it quotes zero"
	}
	return revertReason(err)
}

func randomAddress() common.Address {
	var a common.Address
	_, _ = rand.Read(a[:])
	return a
}

func weiToETH(w *big.Int) float64 {
	f, _ := new(big.Rat).SetFrac(w, big.NewInt(1e18)).Float64()
	return f
}
