// Package facts gathers what can be checked about a token without trading it: the contract (verified, proxy,
// owner), the privileged functions its owner or deployer can still call, current fee and limit settings, who holds
// it, where its liquidity sits and whether that liquidity is burned or locked, and what else its deployer launched.
// Anything that could not be checked is listed in Facts.Gaps with the reason, never silently left out.
package facts

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/evm"
	"github.com/amirradjou/rugcheck/internal/explorer"
)

// Facts is everything rugcheck found out about one token.
type Facts struct {
	Chain     string           `json:"chain"`
	Address   common.Address   `json:"address"`
	Block     uint64           `json:"block"`
	CheckedAt time.Time        `json:"checked_at"`
	Token     Token            `json:"token"`
	Contract  Contract         `json:"contract"`
	Powers    []Power          `json:"powers"`
	Settings  []Setting        `json:"settings"`
	Shares    Shares           `json:"shares"`
	Holders   *Holders         `json:"holders,omitempty"`
	Pools     []Pool           `json:"pools"`
	Liquidity *Liquidity       `json:"liquidity,omitempty"`
	Deployer  *DeployerHistory `json:"deployer,omitempty"`
	// Gaps lists each check that could not be made, and why.
	Gaps    []string      `json:"gaps"`
	Elapsed time.Duration `json:"elapsed_ns"`
	// Timings is how long each stage took, in milliseconds.
	Timings map[string]int64 `json:"timings_ms"`
}

// Token is the ERC-20 metadata.
type Token struct {
	Name        string   `json:"name"`
	Symbol      string   `json:"symbol"`
	Decimals    uint8    `json:"decimals"`
	TotalSupply *big.Int `json:"total_supply"`
}

// Contract describes the code and who controls it.
type Contract struct {
	CodeSize int `json:"code_size"`
	// Verified is nil when it could not be determined.
	Verified   *bool           `json:"verified"`
	VerifiedBy string          `json:"verified_by,omitempty"`
	SourceName string          `json:"source_name,omitempty"`
	Proxy      *Proxy          `json:"proxy,omitempty"`
	Owner      *common.Address `json:"owner,omitempty"`
	// OwnerStatus is "renounced", "wallet", "contract", "none" when the contract has no owner() getter, or
	// "unknown" when the getter could not be read.
	OwnerStatus  string          `json:"owner_status"`
	Creation     *chain.Creation `json:"creation,omitempty"`
	CreatedAt    *time.Time      `json:"created_at,omitempty"`
	Delegatecall bool            `json:"delegatecall"`
	Selfdestruct bool            `json:"selfdestruct"`
}

// Proxy is set when the token's logic lives in another contract that can be swapped.
type Proxy struct {
	Kind           string          `json:"kind"`
	Implementation common.Address  `json:"implementation"`
	Admin          *common.Address `json:"admin,omitempty"`
}

// Shares are balances of the parties that matter, as shares of total supply.
type Shares struct {
	Deployer float64 `json:"deployer"`
	Owner    float64 `json:"owner"`
	Burned   float64 `json:"burned"`
	InPools  float64 `json:"in_pools"`
}

// Gather checks one token. It fails only when the address has no code; every other failure becomes a gap.
func Gather(ctx context.Context, c *chain.Client, ex *explorer.Client, token common.Address) (*Facts, error) {
	start := time.Now()
	g := &gatherer{c: c, ex: ex, f: &Facts{
		Chain: c.Net.Name, Address: token, CheckedAt: start.UTC(), Gaps: []string{}, Timings: map[string]int64{},
	}}
	var err error
	g.timed("contract", func() { err = g.contract(ctx) })()
	if err != nil {
		return nil, err
	}

	var src *explorer.Source
	g.parallel(
		g.timed("basics", func() { g.basics(ctx) }),
		g.timed("creation", func() { g.creation(ctx) }),
		g.timed("source", func() { src = g.source(ctx) }),
		g.timed("pools", func() { g.pools(ctx) }),
	)
	var files map[string]string
	if src != nil {
		files = src.Files
	}
	g.f.Powers = findPowers(g.code, files)
	g.parallel(
		g.timed("probe", func() { g.probe(ctx) }),
		g.timed("shares", func() { g.shares(ctx) }),
		g.timed("holders", func() { g.holders(ctx) }),
		g.timed("liquidity", func() { g.liquidity(ctx) }),
		g.timed("deployer", func() { g.deployerHistory(ctx) }),
	)
	g.f.Elapsed = time.Since(start)
	return g.f, nil
}

type gatherer struct {
	c    *chain.Client
	ex   *explorer.Client
	f    *Facts
	code evm.Code
	// feeTo is each v2 DEX's protocol fee collector (set by pools), which receives LP tokens as fees accrue.
	feeTo map[string]common.Address
	mu    sync.Mutex // guards f.Gaps and f.Timings
}

// timed wraps a stage so its duration is recorded.
func (g *gatherer) timed(name string, fn func()) func() {
	return func() {
		start := time.Now()
		fn()
		g.mu.Lock()
		defer g.mu.Unlock()
		g.f.Timings[name] = time.Since(start).Milliseconds()
	}
}

func (g *gatherer) gap(format string, args ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.f.Gaps = append(g.f.Gaps, fmt.Sprintf(format, args...))
}

func (g *gatherer) parallel(fns ...func()) {
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Go(fn)
	}
	wg.Wait()
}

// deployer is the account that sent the creating transaction, or the zero address if unknown.
func (g *gatherer) deployer() common.Address {
	if g.f.Contract.Creation == nil {
		return common.Address{}
	}
	return g.f.Contract.Creation.Deployer
}

// owner is the current owner, or the zero address if there is none or it renounced.
func (g *gatherer) owner() common.Address {
	if g.f.Contract.Owner == nil || chain.IsBurn(*g.f.Contract.Owner) {
		return common.Address{}
	}
	return *g.f.Contract.Owner
}

var (
	slotEIP1967Impl   = common.HexToHash("0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc")
	slotEIP1967Beacon = common.HexToHash("0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50")
	slotEIP1967Admin  = common.HexToHash("0xb53127684a568b3173ae13b9f8a6016e243e63b6e8ee1178d6a717850b5d6103")
	slotZosImpl       = common.HexToHash("0x7050c9e0f4ca769c69bd3a8ef740bc37934f8e2c036e5a723fd8ee048ed3f8c3")
	slotZosAdmin      = common.HexToHash("0x10d6a54a4754c8869d6886b5f5d7fbfa5b4522237ea5c60d11bc4e7a1ff9390b")
	slotEIP1822       = common.HexToHash("0xc5f16f0fcc639fa48a6947836d9850f504798523bf8c9a3a87d5876cf622bcf7")
)

// contract reads the code, finds a proxy's implementation and analyses both as one contract.
func (g *gatherer) contract(ctx context.Context) error {
	head, err := g.c.SafeHead(ctx)
	if err != nil {
		return err
	}
	g.f.Block = head
	code, err := g.c.Code(ctx, g.f.Address)
	if err != nil {
		return err
	}
	if len(code) == 0 {
		return fmt.Errorf("%s has no contract code on %s", g.f.Address, g.c.Net.Name)
	}
	g.f.Contract.CodeSize = len(code)
	g.code = evm.Analyze(code)

	proxy, err := g.proxy(ctx, code)
	if err != nil {
		g.gap("proxy check: %v", err)
	}
	if proxy != nil {
		g.f.Contract.Proxy = proxy
		impl, err := g.c.Code(ctx, proxy.Implementation)
		if err != nil {
			g.gap("implementation code: %v", err)
		} else {
			g.code = g.code.Merge(evm.Analyze(impl))
		}
	}
	g.f.Contract.Delegatecall = g.code.Delegatecall
	g.f.Contract.Selfdestruct = g.code.Selfdestruct
	return nil
}

func (g *gatherer) proxy(ctx context.Context, code []byte) (*Proxy, error) {
	if impl, ok := evm.MinimalProxyTarget(code); ok {
		return &Proxy{Kind: "EIP-1167 minimal proxy (fixed implementation)", Implementation: impl}, nil
	}
	slots := []common.Hash{slotEIP1967Impl, slotEIP1967Beacon, slotEIP1967Admin, slotZosImpl, slotZosAdmin, slotEIP1822}
	vals, err := g.c.Storages(ctx, g.f.Address, slots)
	if err != nil {
		return nil, err
	}
	addr := func(i int) common.Address { return common.BytesToAddress(vals[i][12:]) }
	admin := func(i int) *common.Address {
		if a := addr(i); a != (common.Address{}) {
			return &a
		}
		return nil
	}
	switch {
	case addr(0) != (common.Address{}):
		return &Proxy{Kind: "EIP-1967", Implementation: addr(0), Admin: admin(2)}, nil
	case addr(1) != (common.Address{}):
		res, err := g.c.Multicall(ctx, []chain.Call{{To: addr(1), Data: pack(beaconABI, "implementation")}})
		if err != nil {
			return nil, err
		}
		impl, ok := addressOf(res[0])
		if !ok {
			return nil, errors.New("beacon proxy: beacon has no implementation()")
		}
		return &Proxy{Kind: "EIP-1967 beacon", Implementation: impl, Admin: admin(2)}, nil
	case addr(3) != (common.Address{}):
		return &Proxy{Kind: "OpenZeppelin (legacy slots)", Implementation: addr(3), Admin: admin(4)}, nil
	case addr(5) != (common.Address{}):
		return &Proxy{Kind: "EIP-1822 (UUPS)", Implementation: addr(5)}, nil
	}
	return nil, nil
}

var (
	selOwner    = selectorOf("owner()")
	selGetOwner = selectorOf("getOwner()")
)

// basics reads ERC-20 metadata, the owner and the current settings in one multicall.
func (g *gatherer) basics(ctx context.Context) {
	t := g.f.Address
	calls := []chain.Call{
		{To: t, Data: pack(erc20ABI, "name")},
		{To: t, Data: pack(erc20ABI, "symbol")},
		{To: t, Data: pack(erc20ABI, "decimals")},
		{To: t, Data: pack(erc20ABI, "totalSupply")},
	}
	ownerAt := -1
	switch {
	case g.code.Has(selOwner):
		ownerAt = len(calls)
		calls = append(calls, chain.Call{To: t, Data: selOwner[:]})
	case g.code.Has(selGetOwner):
		ownerAt = len(calls)
		calls = append(calls, chain.Call{To: t, Data: selGetOwner[:]})
	}
	getters, getterCalls := settingCalls(t, g.code)
	settingsAt := len(calls)
	calls = append(calls, getterCalls...)

	res, err := g.c.Multicall(ctx, calls)
	if err != nil {
		g.gap("token metadata, owner and settings: %v", err)
		g.f.Contract.OwnerStatus = "unknown"
		return
	}
	g.f.Token.Name, g.f.Token.Symbol = text(res[0]), text(res[1])
	if d, ok := word(res[2]); ok && d.IsUint64() && d.Uint64() <= 255 {
		g.f.Token.Decimals = uint8(d.Uint64())
	}
	supply, ok := word(res[3])
	if !ok {
		g.gap("totalSupply() failed: not an ERC-20 token?")
		supply = new(big.Int)
	}
	g.f.Token.TotalSupply = supply

	g.f.Contract.OwnerStatus = "none"
	if ownerAt >= 0 {
		if o, ok := addressOf(res[ownerAt]); ok {
			g.f.Contract.Owner = &o
			if chain.IsBurn(o) {
				g.f.Contract.OwnerStatus = "renounced"
			} else if code, err := g.c.Code(ctx, o); err != nil {
				g.gap("owner code: %v", err)
				g.f.Contract.OwnerStatus = "unknown"
			} else if evm.IsContract(code) {
				g.f.Contract.OwnerStatus = "contract"
			} else {
				g.f.Contract.OwnerStatus = "wallet"
			}
		} else {
			g.gap("owner() reverted")
			g.f.Contract.OwnerStatus = "unknown"
		}
	}
	g.f.Settings = decodeSettings(getters, res[settingsAt:], supply)
}

// creation finds the deploying block, transaction and account.
func (g *gatherer) creation(ctx context.Context) {
	block, err := g.c.CreationBlock(ctx, g.f.Address, g.f.Block)
	if err != nil {
		g.gap("creation block: %v", err)
		return
	}
	if t, err := g.c.BlockTime(ctx, block); err != nil {
		g.gap("deployment time: %v", err)
	} else {
		g.f.Contract.CreatedAt = &t
	}
	cr, err := g.c.Creator(ctx, g.f.Address, block)
	if err != nil {
		var exErr error
		if cr, exErr = g.creatorFromExplorer(ctx, block); exErr != nil {
			g.gap("deployer: %v (explorer: %v)", err, exErr)
			g.f.Contract.Creation = &chain.Creation{Block: block}
			return
		}
	}
	g.f.Contract.Creation = cr
}

// creatorFromExplorer asks the explorer which transaction created the token (it traces internal creations, which
// the RPC cannot) and reads the transaction's sender from the chain.
func (g *gatherer) creatorFromExplorer(ctx context.Context, block uint64) (*chain.Creation, error) {
	creator, tx, err := g.ex.Creation(ctx, g.c.Net, g.f.Address)
	if err != nil {
		return nil, err
	}
	from, txBlock, err := g.c.TxSender(ctx, tx)
	if err != nil {
		return nil, err
	}
	if txBlock != block {
		return nil, fmt.Errorf("explorer's creation tx %s is in block %d, not %d", tx, txBlock, block)
	}
	cr := &chain.Creation{Block: block, Tx: tx, Deployer: from}
	if creator != from {
		cr.Factory = &creator
	}
	return cr, nil
}

// source looks up verified source; a proxy's implementation is the contract whose source matters.
func (g *gatherer) source(ctx context.Context) *explorer.Source {
	addr := g.f.Address
	if p := g.f.Contract.Proxy; p != nil {
		addr = p.Implementation
	}
	src, err := g.ex.Source(ctx, g.c.Net, addr)
	switch {
	case errors.Is(err, explorer.ErrNotVerified):
		no := false
		g.f.Contract.Verified = &no
		return nil
	case err != nil:
		g.gap("verified source: %v", err)
		return nil
	}
	yes := true
	g.f.Contract.Verified, g.f.Contract.VerifiedBy, g.f.Contract.SourceName = &yes, src.By, src.Name
	return src
}

// probe checks who can call each privileged function right now.
func (g *gatherer) probe(ctx context.Context) {
	callers := []caller{}
	if o := g.owner(); o != (common.Address{}) {
		callers = append(callers, caller{"owner", o})
	}
	if d := g.deployer(); d != (common.Address{}) && d != g.owner() {
		callers = append(callers, caller{"deployer", d})
	}
	callers = append(callers, caller{"anyone", probeCaller})
	if err := probePowers(ctx, g.c, g.f.Address, g.f.Token.TotalSupply, g.f.Powers, callers); err != nil {
		g.gap("power probes: %v", err)
	}
}

// shares reads the balances of the deployer, the owner, burn addresses and pools.
func (g *gatherer) shares(ctx context.Context) {
	supply := g.f.Token.TotalSupply
	if supply == nil || supply.Sign() == 0 {
		return
	}
	t := g.f.Address
	calls := []chain.Call{
		balanceOfCall(t, g.deployer()), balanceOfCall(t, g.owner()),
		balanceOfCall(t, chain.ZeroAddress), balanceOfCall(t, chain.DeadAddress),
	}
	for _, p := range g.f.Pools {
		calls = append(calls, balanceOfCall(t, p.Address))
	}
	res, err := g.c.Multicall(ctx, calls)
	if err != nil {
		g.gap("balances: %v", err)
		return
	}
	share := func(i int) float64 {
		if v, ok := word(res[i]); ok {
			return ratio(v, supply)
		}
		return 0
	}
	if g.deployer() != (common.Address{}) {
		g.f.Shares.Deployer = share(0)
	}
	if g.owner() != (common.Address{}) {
		g.f.Shares.Owner = share(1)
	}
	g.f.Shares.Burned = share(2) + share(3)
	for i := range g.f.Pools {
		g.f.Shares.InPools += share(4 + i)
	}
}
