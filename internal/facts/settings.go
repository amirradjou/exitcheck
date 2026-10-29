package facts

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/evm"
)

// Setting is the current value of a fee, limit or trading switch, read through a public getter.
type Setting struct {
	Kind   string `json:"kind"` // fees, limits, trading
	Getter string `json:"getter"`
	// Value is the raw number or "true"/"false". Fee units differ between templates (percent, basis points,
	// per-mille), so fees are reported raw; the buy/sell simulation measures what is actually charged.
	Value string `json:"value"`
	// SupplyShare is Value as a share of total supply, for limits that are token amounts.
	SupplyShare *float64 `json:"supply_share,omitempty"`
}

type getter struct {
	kind, sig string
	isBool    bool
	amount    bool // a token amount, shown as a share of supply
}

var knownGetters = []getter{
	{KindFees, "buyTax()", false, false},
	{KindFees, "sellTax()", false, false},
	{KindFees, "buyFee()", false, false},
	{KindFees, "sellFee()", false, false},
	{KindFees, "buyTotalFees()", false, false},
	{KindFees, "sellTotalFees()", false, false},
	{KindFees, "totalBuyTax()", false, false},
	{KindFees, "totalSellTax()", false, false},
	{KindFees, "_buyTax()", false, false},
	{KindFees, "_sellTax()", false, false},
	{KindFees, "_finalBuyTax()", false, false},
	{KindFees, "_finalSellTax()", false, false},
	{KindFees, "_initialBuyTax()", false, false},
	{KindFees, "_initialSellTax()", false, false},
	{KindFees, "_taxFee()", false, false},
	{KindFees, "_liquidityFee()", false, false},
	{KindFees, "taxFee()", false, false},
	{KindFees, "liquidityFee()", false, false},
	{KindFees, "totalFees()", false, false},
	{KindFees, "basisPointsRate()", false, false},
	{KindFees, "maximumFee()", false, false},
	{KindLimits, "_maxTxAmount()", false, true},
	{KindLimits, "maxTxAmount()", false, true},
	{KindLimits, "maxTransactionAmount()", false, true},
	{KindLimits, "_maxWalletSize()", false, true},
	{KindLimits, "maxWallet()", false, true},
	{KindLimits, "maxWalletAmount()", false, true},
	{KindLimits, "maxBuyAmount()", false, true},
	{KindLimits, "maxSellAmount()", false, true},
	{KindLimits, "limitsInEffect()", true, false},
	{KindLimits, "transferDelayEnabled()", true, false},
	{KindTrading, "tradingOpen()", true, false},
	{KindTrading, "tradingActive()", true, false},
	{KindTrading, "tradingEnabled()", true, false},
	{KindTrading, "isTradingEnabled()", true, false},
	{KindTrading, "swapEnabled()", true, false},
	{KindPause, "paused()", true, false},
}

// settingCalls returns a multicall entry for every known getter the code has. Calling a getter the dispatcher does
// not know would hit the fallback function, which on some contracts succeeds and returns junk.
func settingCalls(token common.Address, code evm.Code) ([]getter, []chain.Call) {
	var gs []getter
	var calls []chain.Call
	for _, g := range knownGetters {
		sel := selectorOf(g.sig)
		if code.Has(sel) {
			gs = append(gs, g)
			calls = append(calls, chain.Call{To: token, Data: sel[:]})
		}
	}
	return gs, calls
}

func decodeSettings(gs []getter, res []chain.Result, supply *big.Int) []Setting {
	var out []Setting
	for i, g := range gs {
		if !res[i].OK || len(res[i].Data) < 32 {
			continue
		}
		v := new(big.Int).SetBytes(res[i].Data[:32])
		s := Setting{Kind: g.kind, Getter: g.sig, Value: v.String()}
		if g.isBool {
			s.Value = map[bool]string{true: "true", false: "false"}[v.Sign() != 0]
		}
		if g.amount && supply != nil && supply.Sign() > 0 {
			share := ratio(v, supply)
			s.SupplyShare = &share
		}
		out = append(out, s)
	}
	return out
}

// ratio returns a/b as a float.
func ratio(a, b *big.Int) float64 {
	if b.Sign() == 0 {
		return 0
	}
	f, _ := new(big.Rat).SetFrac(a, b).Float64()
	return f
}
