package facts

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/amirradjou/exitcheck/internal/chain"
)

// DeployerHistory is what else the deploying account launched.
type DeployerHistory struct {
	Address common.Address `json:"address"`
	Nonce   uint64         `json:"nonce"`
	Balance float64        `json:"balance_eth"`
	// Scanned is how many of the account's most recent nonces were checked for contract creations.
	Scanned int `json:"scanned"`
	// Tokens are the other ERC-20 contracts it deployed directly (not through a factory), newest first.
	Tokens []PastToken `json:"tokens"`
	// Pulled counts those tokens whose v2 pool had liquidity that was later withdrawn (see lpPulled).
	Pulled int `json:"pulled"`
}

// PastToken is another token from the same deployer.
type PastToken struct {
	Address common.Address `json:"address"`
	Symbol  string         `json:"symbol"`
	// Pool is "none" (no v2 pool against WETH), "never funded", "pulled" (liquidity withdrawn) or "live".
	Pool string `json:"pool"`
	// WETH is the WETH left in its v2 pool.
	WETH float64 `json:"weth"`
}

// maxNonceScan bounds the creation scan; serial launchers rarely get past it, busy wallets are cut off.
const maxNonceScan = 1000

// uniswapMinimumLiquidity is the LP amount a Uniswap v2 pair locks forever when it is first funded.
var uniswapMinimumLiquidity = big.NewInt(1000)

// deployerHistory finds the contracts the deployer created directly. A contract made by an account with
// CREATE lives at an address derived from the account and its nonce, so checking those addresses for code lists
// every direct deployment without an indexer.
func (g *gatherer) deployerHistory(ctx context.Context) {
	d := g.deployer()
	if d == (common.Address{}) {
		return
	}
	nonce, err := g.c.Nonce(ctx, d)
	if err != nil {
		g.gap("deployer nonce: %v", err)
		return
	}
	h := &DeployerHistory{Address: d, Nonce: nonce}
	g.f.Deployer = h
	if bal, err := g.c.Balance(ctx, d); err == nil {
		h.Balance = units(bal, 18)
	}
	if g.f.Contract.Creation.Factory != nil {
		g.gap("deployer history: this token was created through contract %s, so only the deployer's direct deployments are listed",
			g.f.Contract.Creation.Factory.Hex())
	}

	first := uint64(0)
	if nonce > maxNonceScan {
		first = nonce - maxNonceScan
		g.gap("deployer history: only the last %d of %d transactions were checked", maxNonceScan, nonce)
	}
	var addrs []common.Address
	for n := nonce; n > first; n-- {
		addrs = append(addrs, crypto.CreateAddress(d, n-1))
	}
	h.Scanned = len(addrs)
	codes, err := g.c.Codes(ctx, addrs)
	if err != nil {
		g.gap("deployer history: %v", err)
		return
	}
	var created []common.Address
	for i, code := range codes {
		if len(code) > 0 && addrs[i] != g.f.Address {
			created = append(created, addrs[i])
		}
	}
	if len(created) == 0 {
		return
	}

	weth, dex := g.c.Net.WETH, g.c.Net.V2[0]
	var calls []chain.Call
	for _, a := range created {
		calls = append(calls,
			chain.Call{To: a, Data: pack(erc20ABI, "symbol")},
			chain.Call{To: a, Data: pack(erc20ABI, "totalSupply")},
			chain.Call{To: dex.Factory, Data: pack(v2FactoryABI, "getPair", a, weth)})
	}
	res, err := g.c.Multicall(ctx, calls)
	if err != nil {
		g.gap("deployer history: %v", err)
		return
	}
	var tokens []PastToken
	var pairs []common.Address
	for i, a := range created {
		if _, ok := word(res[3*i+1]); !ok {
			continue // no totalSupply(): not a token
		}
		pair, _ := addressOf(res[3*i+2])
		tokens = append(tokens, PastToken{Address: a, Symbol: text(res[3*i])})
		pairs = append(pairs, pair)
	}
	feeTo := g.feeTo[dex.Name]
	calls = calls[:0]
	for _, p := range pairs {
		calls = append(calls, chain.Call{To: p, Data: pack(erc20ABI, "totalSupply")}, balanceOfCall(weth, p),
			balanceOfCall(p, feeTo))
	}
	res, err = g.c.Multicall(ctx, calls)
	if err != nil {
		g.gap("deployer history pools: %v", err)
		return
	}
	for i := range tokens {
		if pairs[i] == (common.Address{}) {
			tokens[i].Pool = "none"
			continue
		}
		lp, _ := word(res[3*i])
		if w, ok := word(res[3*i+1]); ok {
			tokens[i].WETH = units(w, 18)
		}
		protocol := new(big.Int)
		if feeTo != (common.Address{}) {
			protocol, _ = word(res[3*i+2])
		}
		switch {
		case lp == nil || lp.Sign() == 0:
			tokens[i].Pool = "never funded"
		case protocol != nil && lpPulled(lp, protocol):
			tokens[i].Pool = "pulled"
			h.Pulled++
		default:
			tokens[i].Pool = "live"
		}
	}
	h.Tokens = tokens
}
