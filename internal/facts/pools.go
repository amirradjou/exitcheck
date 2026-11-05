package facts

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/exitcheck/internal/chain"
)

// Pool is a DEX pool that trades the token.
type Pool struct {
	DEX     string         `json:"dex"` // uniswap-v2, uniswap-v3, aerodrome
	Address common.Address `json:"address"`
	Quote   string         `json:"quote"`
	// QuoteToken is the quote token's address.
	QuoteToken common.Address `json:"quote_token"`
	Fee        uint32         `json:"fee,omitempty"` // v3 fee tier, in hundredths of a basis point
	Stable     bool           `json:"stable,omitempty"`
	// QuoteAmount is the quote token the pool holds, in whole units; USD values it.
	QuoteAmount float64  `json:"quote_amount"`
	USD         float64  `json:"usd"`
	TokenAmount *big.Int `json:"token_amount"`
}

// LPTokens reports whether the pool's liquidity is a fungible LP token (v2-style) rather than NFT positions (v3).
func (p Pool) LPTokens() bool { return p.DEX != "uniswap-v3" }

var v3Fees = []uint32{100, 500, 3000, 10000}

type poolKey struct {
	dex    string
	quote  chain.Quote
	fee    uint32
	stable bool
}

// pools finds every pool pairing the token with a quote token, and values each one.
func (g *gatherer) pools(ctx context.Context) {
	found, feeTo, err := findPools(ctx, g.c, g.f.Address)
	g.feeTo = feeTo
	if err != nil {
		g.gap("pools: %v", err)
		return
	}
	g.f.Pools = found
	if len(found) == 0 {
		g.gap("pools: no pool against %s on the DEXes exitcheck knows", quoteSymbols(g.c.Net))
	}
}

// Pools lists every pool that trades token against a quote token (WETH, USDC, USDT) on the DEXes exitcheck knows,
// most valuable first.
func Pools(ctx context.Context, c *chain.Client, token common.Address) ([]Pool, error) {
	found, _, err := findPools(ctx, c, token)
	return found, err
}

// findPools does the work of Pools and also returns each v2 DEX's protocol fee collector.
func findPools(ctx context.Context, c *chain.Client, t common.Address) ([]Pool, map[string]common.Address, error) {
	net := c.Net
	var keys []poolKey
	var calls []chain.Call
	for _, q := range net.Quotes {
		if q.Address == t {
			continue
		}
		for _, d := range net.V2 {
			keys = append(keys, poolKey{dex: d.Name, quote: q})
			calls = append(calls, chain.Call{To: d.Factory, Data: pack(v2FactoryABI, "getPair", t, q.Address)})
		}
		if net.V3Factory != (common.Address{}) {
			for _, fee := range v3Fees {
				keys = append(keys, poolKey{dex: "uniswap-v3", quote: q, fee: fee})
				calls = append(calls, chain.Call{To: net.V3Factory, Data: pack(v3FactoryABI, "getPool", t, q.Address, new(big.Int).SetUint64(uint64(fee)))})
			}
		}
		if net.Aerodrome != (common.Address{}) {
			for _, stable := range []bool{false, true} {
				keys = append(keys, poolKey{dex: "aerodrome", quote: q, stable: stable})
				calls = append(calls, chain.Call{To: net.Aerodrome, Data: pack(aerodromeFactoryABI, "getPool", t, q.Address, stable)})
			}
		}
	}
	// The WETH/USDC pair prices ETH in dollars.
	weth, usdc := net.Quotes[0], net.Quotes[1]
	calls = append(calls, chain.Call{To: net.V2[0].Factory, Data: pack(v2FactoryABI, "getPair", weth.Address, usdc.Address)})
	feeToAt := len(calls)
	for _, d := range net.V2 {
		calls = append(calls, chain.Call{To: d.Factory, Data: pack(v2FactoryABI, "feeTo")})
	}

	res, err := c.Multicall(ctx, calls)
	if err != nil {
		return nil, nil, err
	}
	feeTo := map[string]common.Address{}
	for i, d := range net.V2 {
		if a, ok := addressOf(res[feeToAt+i]); ok && a != (common.Address{}) {
			feeTo[d.Name] = a
		}
	}
	var found []Pool
	var foundKeys []poolKey
	for i, k := range keys {
		if a, ok := addressOf(res[i]); ok && a != (common.Address{}) {
			found = append(found, Pool{DEX: k.dex, Address: a, Quote: k.quote.Symbol, QuoteToken: k.quote.Address, Fee: k.fee, Stable: k.stable})
			foundKeys = append(foundKeys, k)
		}
	}
	ethPair, _ := addressOf(res[len(keys)])

	calls = calls[:0]
	for i, p := range found {
		calls = append(calls, balanceOfCall(foundKeys[i].quote.Address, p.Address), balanceOfCall(t, p.Address))
	}
	calls = append(calls, balanceOfCall(weth.Address, ethPair), balanceOfCall(usdc.Address, ethPair))
	res, err = c.Multicall(ctx, calls)
	if err != nil {
		return nil, feeTo, fmt.Errorf("reserves: %w", err)
	}
	ethUSD := 0.0
	if w, ok := word(res[2*len(found)]); ok && w.Sign() > 0 {
		if u, ok := word(res[2*len(found)+1]); ok {
			ethUSD = units(u, usdc.Decimals) / units(w, weth.Decimals)
		}
	}
	for i := range found {
		q := foundKeys[i].quote
		if v, ok := word(res[2*i]); ok {
			found[i].QuoteAmount = units(v, q.Decimals)
		}
		found[i].TokenAmount, _ = word(res[2*i+1])
		if q.Symbol == weth.Symbol {
			found[i].USD = found[i].QuoteAmount * ethUSD
		} else {
			found[i].USD = found[i].QuoteAmount
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].USD > found[j].USD })
	return found, feeTo, nil
}

func quoteSymbols(net chain.Network) string {
	var s []string
	for _, q := range net.Quotes {
		s = append(s, q.Symbol)
	}
	return strings.Join(s, ", ")
}

// units converts a raw token amount to whole units.
func units(v *big.Int, decimals uint8) float64 {
	f, _ := new(big.Rat).SetFrac(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)).Float64()
	return f
}

// lpPulled reports whether a v2 pool's liquidity providers have withdrawn: the LP held by anyone other than the
// protocol's fee collector, beyond the minimum locked at the first deposit, is under 1% of supply. Burned LP counts
// as held, so a burned pool is never "pulled".
func lpPulled(supply, protocol *big.Int) bool {
	rest := new(big.Int).Sub(supply, uniswapMinimumLiquidity)
	rest.Sub(rest, protocol)
	return rest.Sign() <= 0 || new(big.Int).Mul(rest, big.NewInt(100)).Cmp(supply) < 0
}

// Liquidity is the state of the token's main pool: its age and, for pools with LP tokens, who holds them.
type Liquidity struct {
	Pool         common.Address `json:"pool"`
	DEX          string         `json:"dex"`
	CreatedBlock uint64         `json:"created_block,omitempty"`
	CreatedAt    *time.Time     `json:"created_at,omitempty"`
	// Pulled is set when the liquidity providers have withdrawn (see lpPulled).
	Pulled bool `json:"pulled"`
	// Shares of the LP token supply. Nil when LP holders could not be read, the pool has no LP token, or it was
	// pulled.
	LP *LPShares `json:"lp,omitempty"`
}

// LPShares says where a pool's LP tokens are: burned liquidity can never be withdrawn, locked liquidity can be
// withdrawn after its unlock date, and the rest can be withdrawn at any time.
type LPShares struct {
	Burned   float64 `json:"burned"`
	Locked   float64 `json:"locked"`
	Deployer float64 `json:"deployer"`
	Owner    float64 `json:"owner"`
	// Protocol is the DEX's fee collector, which is minted LP tokens as trading fees accrue (Uniswap v2's fee
	// switch). It never withdraws on the token's behalf.
	Protocol float64  `json:"protocol"`
	Other    float64  `json:"other"`
	Locks    []Lock   `json:"locks,omitempty"`
	Top      []Holder `json:"top"`
	Source   string   `json:"source"`
}

// Lock is LP held by a locker contract.
type Lock struct {
	Locker string  `json:"locker"`
	Share  float64 `json:"share"`
	// Unlock is the earliest unlock date of the locks on this pool; nil if the locker's records could not be read.
	Unlock *time.Time `json:"unlock,omitempty"`
}

func (g *gatherer) liquidity(ctx context.Context) {
	if len(g.f.Pools) == 0 {
		return
	}
	main := g.f.Pools[0]
	l := &Liquidity{Pool: main.Address, DEX: main.DEX}
	g.f.Liquidity = l
	block, err := g.c.CreationBlock(ctx, main.Address, g.f.Block)
	if err != nil {
		g.gap("pool age: %v", err)
	} else {
		l.CreatedBlock = block
		if t, err := g.c.BlockTime(ctx, block); err != nil {
			g.gap("pool creation time: %v", err)
		} else {
			l.CreatedAt = &t
		}
	}
	if !main.LPTokens() {
		g.gap("LP lock: the main pool is Uniswap v3, whose positions are NFTs; burn and lock state is not checked")
		return
	}
	feeTo := g.feeTo[main.DEX]
	res, err := g.c.Multicall(ctx, []chain.Call{
		{To: main.Address, Data: pack(erc20ABI, "totalSupply")}, balanceOfCall(main.Address, feeTo),
	})
	if err != nil {
		g.gap("LP supply: %v", err)
		return
	}
	supply, ok := word(res[0])
	if !ok || supply.Sign() == 0 {
		g.gap("LP supply: pool has no LP tokens")
		return
	}
	protocol := new(big.Int)
	if feeTo != (common.Address{}) {
		if v, ok := word(res[1]); ok {
			protocol = v
		}
	}
	if lpPulled(supply, protocol) {
		// What is left is the permanent minimum (held by a burn address) and the fee collector's share: reporting
		// those as "burned" would invert the truth.
		l.Pulled = true
		return
	}
	top, _, source, err := g.topHolders(ctx, main.Address, l.CreatedBlock, supply)
	if err != nil {
		g.gap("LP holders: %v", err)
		return
	}
	lp := &LPShares{Top: top, Source: source}
	lockers := map[string]float64{}
	for _, h := range top {
		switch {
		case h.Label == "burn":
			lp.Burned += h.Share
		case strings.HasPrefix(h.Label, "locker:"):
			lp.Locked += h.Share
			lockers[strings.TrimPrefix(h.Label, "locker:")] += h.Share
		case h.Label == "deployer":
			lp.Deployer += h.Share
		case h.Label == "owner":
			lp.Owner += h.Share
		case h.Address == feeTo:
			lp.Protocol += h.Share
		}
	}
	// Everything else, including holders below the top of the list, can be withdrawn at any time.
	lp.Other = max(0, 1-lp.Burned-lp.Locked-lp.Deployer-lp.Owner-lp.Protocol)
	for name, share := range lockers {
		lk := Lock{Locker: name, Share: share}
		if t, err := g.unlockDate(ctx, name, main.Address); err != nil {
			g.gap("unlock date at %s: %v", name, err)
		} else {
			lk.Unlock = t
		}
		lp.Locks = append(lp.Locks, lk)
	}
	sort.Slice(lp.Locks, func(i, j int) bool { return lp.Locks[i].Share > lp.Locks[j].Share })
	if len(lp.Top) > topShown {
		lp.Top = lp.Top[:topShown]
	}
	l.LP = lp
}

var (
	uncxABI = chain.MustABI(`[
	{"name":"getNumLocksForToken","type":"function","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
	{"name":"tokenLocks","type":"function","stateMutability":"view","inputs":[{"type":"address"},{"type":"uint256"}],
	 "outputs":[{"name":"lockDate","type":"uint256"},{"name":"amount","type":"uint256"},{"name":"initialAmount","type":"uint256"},
	            {"name":"unlockDate","type":"uint256"},{"name":"lockID","type":"uint256"},{"name":"owner","type":"address"}]}]`)
	teamFinanceABI = chain.MustABI(`[
	{"name":"getDepositsByTokenAddress","type":"function","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256[]"}]},
	{"name":"lockedToken","type":"function","stateMutability":"view","inputs":[{"type":"uint256"}],
	 "outputs":[{"name":"tokenAddress","type":"address"},{"name":"withdrawalAddress","type":"address"},{"name":"tokenAmount","type":"uint256"},
	            {"name":"unlockTime","type":"uint256"},{"name":"withdrawn","type":"bool"}]}]`)
	pinkLockABI = chain.MustABI(`[
	{"name":"totalLockCountForToken","type":"function","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
	{"name":"getLocksForToken","type":"function","stateMutability":"view","inputs":[{"type":"address"},{"type":"uint256"},{"type":"uint256"}],
	 "outputs":[{"type":"tuple[]","components":[{"name":"id","type":"uint256"},{"name":"token","type":"address"},{"name":"owner","type":"address"},
	   {"name":"amount","type":"uint256"},{"name":"lockDate","type":"uint256"},{"name":"tgeDate","type":"uint256"},{"name":"tgeBps","type":"uint256"},
	   {"name":"cycle","type":"uint256"},{"name":"cycleBps","type":"uint256"},{"name":"unlockedAmount","type":"uint256"},{"name":"description","type":"string"}]}]}]`)
)

// maxLocks bounds how many lock records are read per locker.
const maxLocks = 50

// unlockDate reads the earliest unlock date among a locker's active locks on lpToken.
func (g *gatherer) unlockDate(ctx context.Context, locker string, lpToken common.Address) (*time.Time, error) {
	var addr common.Address
	for a, n := range g.c.Net.Lockers {
		if n == locker {
			addr = a
		}
	}
	var unlocks []uint64
	switch locker {
	case "UNCX":
		n, err := g.callUint(ctx, addr, uncxABI, "getNumLocksForToken", lpToken)
		if err != nil {
			return nil, err
		}
		var calls []chain.Call
		for i := int64(0); i < min(n.Int64(), maxLocks); i++ {
			calls = append(calls, chain.Call{To: addr, Data: pack(uncxABI, "tokenLocks", lpToken, big.NewInt(i))})
		}
		res, err := g.c.Multicall(ctx, calls)
		if err != nil {
			return nil, err
		}
		for _, r := range res {
			if vals, err := uncxABI.Methods["tokenLocks"].Outputs.Unpack(r.Data); err == nil && r.OK && vals[1].(*big.Int).Sign() > 0 {
				unlocks = append(unlocks, vals[3].(*big.Int).Uint64())
			}
		}
	case "Team Finance":
		res, err := g.c.Multicall(ctx, []chain.Call{{To: addr, Data: pack(teamFinanceABI, "getDepositsByTokenAddress", lpToken)}})
		if err != nil {
			return nil, err
		}
		vals, err := teamFinanceABI.Methods["getDepositsByTokenAddress"].Outputs.Unpack(res[0].Data)
		if err != nil || !res[0].OK {
			return nil, errors.New("getDepositsByTokenAddress reverted or did not decode")
		}
		var calls []chain.Call
		for _, id := range vals[0].([]*big.Int) {
			if len(calls) == maxLocks {
				break
			}
			calls = append(calls, chain.Call{To: addr, Data: pack(teamFinanceABI, "lockedToken", id)})
		}
		res, err = g.c.Multicall(ctx, calls)
		if err != nil {
			return nil, err
		}
		for _, r := range res {
			if v, err := teamFinanceABI.Methods["lockedToken"].Outputs.Unpack(r.Data); err == nil && r.OK && !v[4].(bool) && v[2].(*big.Int).Sign() > 0 {
				unlocks = append(unlocks, v[3].(*big.Int).Uint64())
			}
		}
	case "PinkLock":
		n, err := g.callUint(ctx, addr, pinkLockABI, "totalLockCountForToken", lpToken)
		if err != nil {
			return nil, err
		}
		if n.Sign() == 0 {
			return nil, errors.New("no locks recorded for this LP token")
		}
		end := big.NewInt(min(n.Int64(), maxLocks) - 1)
		res, err := g.c.Multicall(ctx, []chain.Call{{To: addr, Data: pack(pinkLockABI, "getLocksForToken", lpToken, big.NewInt(0), end)}})
		if err != nil {
			return nil, err
		}
		if !res[0].OK {
			return nil, errors.New("getLocksForToken reverted")
		}
		if unlocks, err = pinkLockUnlocks(res[0].Data); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("reading %s locks is not supported", locker)
	}
	if len(unlocks) == 0 {
		return nil, errors.New("no active lock records for this LP token")
	}
	earliest := unlocks[0]
	for _, u := range unlocks[1:] {
		earliest = min(earliest, u)
	}
	t := time.Unix(int64(earliest), 0).UTC()
	return &t, nil
}

// pinkLock is one PinkLock02 lock record. ConvertType copies fields by position, so the fields follow the ABI's
// component order.
type pinkLock struct {
	Id                                                                 *big.Int
	Token, Owner                                                       common.Address
	Amount, LockDate, TgeDate, TgeBps, Cycle, CycleBps, UnlockedAmount *big.Int
	Description                                                        string
}

// pinkLockUnlocks decodes getLocksForToken's result into the first unlock date (tgeDate) of each lock that still
// holds tokens.
func pinkLockUnlocks(data []byte) ([]uint64, error) {
	vals, err := pinkLockABI.Methods["getLocksForToken"].Outputs.Unpack(data)
	if err != nil {
		return nil, fmt.Errorf("getLocksForToken did not decode: %w", err)
	}
	var unlocks []uint64
	for _, lk := range *abi.ConvertType(vals[0], new([]pinkLock)).(*[]pinkLock) {
		if lk.Amount.Cmp(lk.UnlockedAmount) > 0 {
			unlocks = append(unlocks, lk.TgeDate.Uint64())
		}
	}
	return unlocks, nil
}

func (g *gatherer) callUint(ctx context.Context, to common.Address, a abi.ABI, method string, args ...any) (*big.Int, error) {
	res, err := g.c.Multicall(ctx, []chain.Call{{To: to, Data: pack(a, method, args...)}})
	if err != nil {
		return nil, err
	}
	v, ok := word(res[0])
	if !ok {
		return nil, fmt.Errorf("%s reverted", method)
	}
	return v, nil
}
