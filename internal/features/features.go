// Package features turns a check's report into the numbers the model reads (M4). The same code builds the training
// set and will score live launches, so the two cannot drift apart.
//
// Only what the chain says at the checked block counts. Explorer answers are left out (verified source, powers found
// only in source, Blockscout's holder list): explorers know the present, so a past launch re-checked on a fork would
// see its own future, and a live launch is usually too new to be indexed anyway. NaN marks a value the check could not
// read; gradient-boosted trees take missing values as they are.
package features

import (
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/amirradjou/rugcheck/internal/check"
	"github.com/amirradjou/rugcheck/internal/facts"
	"github.com/amirradjou/rugcheck/internal/sim"
)

// powerKinds are the privileged functions facts looks for, each one feature.
var powerKinds = []string{
	facts.KindMint, facts.KindPause, facts.KindBlacklist, facts.KindLimits, facts.KindFees, facts.KindTrading,
	facts.KindUpgrade, facts.KindReclaim, facts.KindSeize,
}

// chainGaps are the checks a gap can count against: each is made from chain data alone. Explorer-backed ones
// (verified source, a traced deployer) are left out, as is the v3 LP lock, which dex_v3 already says.
var chainGaps = []string{
	"balances", "creation block", "deployer history", "holders", "LP holders", "LP supply", "pool age", "pools",
	"power probes", "proxy check", "token metadata",
}

// Names are the features, in the order Vector returns them.
var Names = func() []string {
	names := []string{
		"chain_base", "dex_v2", "dex_v3", "dex_aerodrome", "quote_stable", "v3_fee",
		"code_size_log", "proxy", "owner_renounced", "owner_wallet", "owner_contract", "owner_none", "owner_unknown",
		"delegatecall", "selfdestruct", "decimals", "supply_log", "token_age_log", "pool_age_log",
	}
	for _, k := range powerKinds {
		names = append(names, "power_"+strings.ReplaceAll(k, "-", "_"))
	}
	return append(names,
		"powers_no_effect", "settings_fees", "settings_limits", "settings_trading", "limit_min_share",
		"share_deployer", "share_owner", "share_burned", "share_in_pools",
		"holders_log", "top_share", "top10_share",
		"pools", "liquidity_usd_log", "main_pool_usd_log", "lp_pulled",
		"lp_burned", "lp_locked", "lp_deployer", "lp_owner", "lp_protocol", "lp_other",
		"deployer_nonce_log", "deployer_balance_eth", "deployer_tokens", "deployer_pulled",
		"sim_ran", "sim_no_pool", "sim_cannot_buy", "sim_buy_ok", "sim_transfer_ok", "sim_sell_ok",
		"buy_tax", "transfer_tax", "sell_tax", "sim_flagged",
		"gaps",
	)
}()

// Vector returns r's features in Names order.
func Vector(r *check.Report) []float64 {
	v := map[string]float64{}
	f, s := r.Facts, r.Sim
	if f != nil {
		contract(v, f)
		powers(v, f)
		holdings(v, f)
		pools(v, f)
		deployer(v, f)
		gaps := 0
		for _, g := range f.Gaps {
			for _, prefix := range chainGaps {
				if strings.HasPrefix(g, prefix) {
					gaps++
					break
				}
			}
		}
		v["gaps"] = float64(gaps)
	}
	simulation(v, s)
	if r.SimError != "" {
		v["gaps"]++
	}
	out := make([]float64, len(Names))
	for i, n := range Names {
		x, ok := v[n]
		if !ok {
			x = math.NaN()
		}
		out[i] = x
	}
	return out
}

func b(cond bool) float64 {
	if cond {
		return 1
	}
	return 0
}

// logAge is log10 of seconds since t, as of the checked block.
func logAge(f *facts.Facts, t *time.Time) (float64, bool) {
	if t == nil {
		return 0, false
	}
	return math.Log10(math.Max(f.AsOf().Sub(*t).Seconds(), 0) + 1), true
}

func contract(v map[string]float64, f *facts.Facts) {
	c := f.Contract
	v["chain_base"] = b(f.Chain == "base")
	v["code_size_log"] = math.Log10(float64(c.CodeSize) + 1)
	v["proxy"] = b(c.Proxy != nil)
	for _, st := range []string{"renounced", "wallet", "contract", "none", "unknown"} {
		v["owner_"+st] = b(c.OwnerStatus == st)
	}
	v["delegatecall"], v["selfdestruct"] = b(c.Delegatecall), b(c.Selfdestruct)
	if f.Token.TotalSupply != nil {
		v["decimals"] = float64(f.Token.Decimals)
		whole, _ := new(big.Float).Quo(new(big.Float).SetInt(f.Token.TotalSupply),
			new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(f.Token.Decimals)), nil))).Float64()
		v["supply_log"] = math.Log10(whole + 1)
	}
	if a, ok := logAge(f, c.CreatedAt); ok {
		v["token_age_log"] = a
	}
}

// powers scores each kind found in the bytecode: 0 absent, 1 present but not shown usable, 2 usable now by the owner
// or deployer, 3 usable by anyone.
func powers(v map[string]float64, f *facts.Facts) {
	for _, k := range powerKinds {
		v["power_"+strings.ReplaceAll(k, "-", "_")] = 0
	}
	noEffect := 0
	for _, p := range f.Powers {
		if p.Where != "bytecode" {
			continue // found only in verified source: an explorer answer
		}
		score := 1.0
		for _, who := range p.CallableBy {
			switch who {
			case "anyone":
				score = 3
			case "owner", "deployer":
				score = math.Max(score, 2)
			}
		}
		name := "power_" + strings.ReplaceAll(p.Kind, "-", "_")
		v[name] = math.Max(v[name], score)
		if len(p.NoEffectFor) > 0 {
			noEffect++
		}
	}
	v["powers_no_effect"] = float64(noEffect)
	var fees, limits, trading int
	minShare := math.Inf(1)
	for _, s := range f.Settings {
		switch s.Kind {
		case "fees":
			fees++
		case "limits":
			limits++
			if s.SupplyShare != nil {
				minShare = math.Min(minShare, *s.SupplyShare)
			}
		case "trading":
			trading++
		}
	}
	v["settings_fees"], v["settings_limits"], v["settings_trading"] = float64(fees), float64(limits), float64(trading)
	if !math.IsInf(minShare, 1) {
		v["limit_min_share"] = minShare
	}
}

func holdings(v map[string]float64, f *facts.Facts) {
	sh := f.Shares
	v["share_deployer"], v["share_owner"], v["share_burned"], v["share_in_pools"] = sh.Deployer, sh.Owner, sh.Burned, sh.InPools
	if h := f.Holders; h != nil && h.Source == "transfer-logs" {
		v["holders_log"] = math.Log10(float64(h.Count) + 1)
		v["top_share"], v["top10_share"] = h.TopShare, h.Top10Share
	}
}

func pools(v map[string]float64, f *facts.Facts) {
	v["pools"] = float64(len(f.Pools))
	total := 0.0
	for _, p := range f.Pools {
		total += p.USD
	}
	v["liquidity_usd_log"] = math.Log10(total + 1)
	l := f.Liquidity
	if l == nil {
		return
	}
	v["dex_v2"], v["dex_v3"], v["dex_aerodrome"] = b(l.DEX == "uniswap-v2"), b(l.DEX == "uniswap-v3"), b(l.DEX == "aerodrome")
	for _, p := range f.Pools {
		if p.Address == l.Pool {
			v["quote_stable"] = b(p.Quote == "USDC" || p.Quote == "USDT")
			v["main_pool_usd_log"] = math.Log10(p.USD + 1)
			if p.DEX == "uniswap-v3" {
				v["v3_fee"] = float64(p.Fee)
			}
		}
	}
	if a, ok := logAge(f, l.CreatedAt); ok {
		v["pool_age_log"] = a
	}
	v["lp_pulled"] = b(l.Pulled)
	if lp := l.LP; lp != nil {
		v["lp_burned"], v["lp_locked"], v["lp_deployer"] = lp.Burned, lp.Locked, lp.Deployer
		v["lp_owner"], v["lp_protocol"], v["lp_other"] = lp.Owner, lp.Protocol, lp.Other
	}
}

func deployer(v map[string]float64, f *facts.Facts) {
	d := f.Deployer
	if d == nil {
		return
	}
	v["deployer_nonce_log"] = math.Log10(float64(d.Nonce) + 1)
	v["deployer_balance_eth"] = d.Balance
	v["deployer_tokens"], v["deployer_pulled"] = float64(len(d.Tokens)), float64(d.Pulled)
}

func simulation(v map[string]float64, s *sim.Result) {
	v["sim_ran"] = b(s != nil)
	if s == nil {
		return
	}
	v["sim_no_pool"], v["sim_cannot_buy"], v["sim_flagged"] = b(s.Verdict == sim.NoPool), b(s.Verdict == sim.CannotBuy), b(s.Flagged())
	for name, st := range map[string]*sim.Step{"buy": s.Buy, "transfer": s.Transfer, "sell": s.Sell} {
		if st == nil {
			continue
		}
		v["sim_"+name+"_ok"] = b(st.OK)
		if st.Tax != nil {
			v[name+"_tax"] = *st.Tax
		}
	}
}
