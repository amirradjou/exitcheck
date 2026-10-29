package facts

import (
	"fmt"
	"io"
	"math"
	"math/big"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Render writes a plain-text report.
func Render(w io.Writer, f *Facts) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// Names, file paths and error texts can come from the token's author: print none of their control characters.
	p := func(format string, args ...any) {
		for i, a := range args {
			if s, ok := a.(string); ok {
				args[i] = printable(s, 400)
			}
		}
		fmt.Fprintf(tw, format+"\n", args...)
	}

	name := f.Token.Symbol
	if f.Token.Name != "" && f.Token.Name != f.Token.Symbol {
		name += " (" + f.Token.Name + ")"
	}
	p("%s · %s · %s", name, f.Chain, f.Address.Hex())
	p("checked at block %d, %s, in %.1fs", f.Block, f.CheckedAt.Format("2006-01-02 15:04 UTC"), f.Elapsed.Seconds())

	c := f.Contract
	p("\nCONTRACT")
	switch {
	case c.Verified == nil:
		p("  source\tunknown (see gaps)")
	case *c.Verified:
		p("  source\tverified on %s (%s)", c.VerifiedBy, c.SourceName)
	default:
		p("  source\tNOT verified")
	}
	if c.Proxy != nil {
		admin := ""
		if c.Proxy.Admin != nil {
			admin = ", admin " + short(*c.Proxy.Admin)
		}
		p("  proxy\t%s → %s%s", c.Proxy.Kind, short(c.Proxy.Implementation), admin)
	} else {
		p("  proxy\tno")
	}
	switch {
	case c.OwnerStatus == "none":
		p("  owner\tno owner() function")
	case c.Owner == nil:
		p("  owner\tunknown (see gaps)")
	case c.OwnerStatus == "renounced":
		p("  owner\trenounced (%s)", short(*c.Owner))
	default:
		p("  owner\t%s (%s)", short(*c.Owner), c.OwnerStatus)
	}
	if cr := c.Creation; cr != nil {
		line := fmt.Sprintf("block %d", cr.Block)
		if c.CreatedAt != nil {
			line = fmt.Sprintf("%s (%s ago), %s", c.CreatedAt.Format("2006-01-02"), age(f.CheckedAt.Sub(*c.CreatedAt)), line)
		}
		if cr.Deployer != (common.Address{}) {
			line += fmt.Sprintf(", by %s in tx %s", short(cr.Deployer), shortHash(cr.Tx))
		}
		if cr.Factory != nil {
			line += " via contract " + short(*cr.Factory)
		}
		p("  deployed\t%s", line)
	}
	p("  opcodes\tdelegatecall %s, selfdestruct %s", yesNo(c.Delegatecall), yesNo(c.Selfdestruct))

	p("\nPRIVILEGED FUNCTIONS")
	if len(f.Powers) == 0 {
		p("  none of the known ones")
	}
	for _, pw := range f.Powers {
		who := "not probed"
		if pw.Probed {
			who = "callable by: nobody we tried"
			if len(pw.CallableBy) > 0 {
				who = "callable by: " + strings.Join(pw.CallableBy, ", ")
			}
			if len(pw.NoEffectFor) > 0 {
				who += "; accepted without effect from: " + strings.Join(pw.NoEffectFor, ", ")
			}
		}
		p("  %s\t%s\t%s\t%s", pw.Kind, pw.Function, pw.Where, who)
	}

	if len(f.Settings) > 0 {
		p("\nSETTINGS")
		for _, s := range f.Settings {
			v := s.Value
			if s.SupplyShare != nil {
				v += fmt.Sprintf(" (%s of supply)", pct(*s.SupplyShare))
			}
			p("  %s\t%s = %s", s.Kind, s.Getter, v)
		}
	}

	if f.Token.TotalSupply != nil {
		p("\nSUPPLY")
		p("  total\t%s %s", amount(f.Token.TotalSupply, f.Token.Decimals), f.Token.Symbol)
		p("  held by\tdeployer %s, owner %s, burned %s, pools %s",
			pct(f.Shares.Deployer), pct(f.Shares.Owner), pct(f.Shares.Burned), pct(f.Shares.InPools))
	}

	if h := f.Holders; h != nil {
		p("\nHOLDERS (%s, %d holders)", h.Source, h.Count)
		p("  top holder %s, top 10 %s (pools, burn addresses and lockers not counted)", pct(h.TopShare), pct(h.Top10Share))
		for i, x := range h.Top {
			p("  %2d. %s\t%s\t%s", i+1, short(x.Address), pct(x.Share), x.Label)
		}
	}

	p("\nLIQUIDITY")
	if len(f.Pools) == 0 {
		p("  no pools found")
	}
	const poolsShown = 6
	for i, pl := range f.Pools {
		if i == poolsShown {
			rest := 0.0
			for _, x := range f.Pools[i:] {
				rest += x.USD
			}
			p("  … %d more pools\t\t\t%s in all", len(f.Pools)-i, usd(rest))
			break
		}
		kind := pl.DEX
		switch {
		case pl.Fee > 0:
			kind += fmt.Sprintf(" %.2f%%", float64(pl.Fee)/1e4)
		case pl.Stable:
			kind += " stable"
		}
		p("  %s\t%s\t%s %s\t%s", kind, short(pl.Address), trim(pl.QuoteAmount), pl.Quote, usd(pl.USD))
	}
	if l := f.Liquidity; l != nil {
		line := fmt.Sprintf("%s %s", l.DEX, short(l.Pool))
		if l.CreatedAt != nil {
			line += fmt.Sprintf(", created %s (%s ago)", l.CreatedAt.Format("2006-01-02 15:04"), age(f.CheckedAt.Sub(*l.CreatedAt)))
		}
		p("  main pool\t%s", line)
		if l.Pulled {
			p("  LP tokens\tLIQUIDITY WITHDRAWN: only the permanent minimum and the DEX's fee share are left")
		}
		if lp := l.LP; lp != nil {
			p("  LP tokens\tburned %s, locked %s, deployer %s, owner %s, DEX fee share %s, others %s",
				pct(lp.Burned), pct(lp.Locked), pct(lp.Deployer), pct(lp.Owner), pct(lp.Protocol), pct(lp.Other))
			for _, lk := range lp.Locks {
				until := "unlock date unknown"
				if lk.Unlock != nil {
					until = "until " + lk.Unlock.Format("2006-01-02")
					if lk.Unlock.Before(f.CheckedAt) {
						until = "unlocked since " + lk.Unlock.Format("2006-01-02")
					}
				}
				p("  lock\t%s of LP in %s, %s", pct(lk.Share), lk.Locker, until)
			}
		}
	}

	if d := f.Deployer; d != nil {
		p("\nDEPLOYER %s", d.Address.Hex())
		p("  %d transactions, %s ETH; %d other tokens deployed directly, %d with liquidity pulled",
			d.Nonce, trim(d.Balance), len(d.Tokens), d.Pulled)
		for i, t := range d.Tokens {
			if i == 15 {
				p("  … %d more", len(d.Tokens)-i)
				break
			}
			pool := t.Pool
			if t.Pool == "live" {
				pool = fmt.Sprintf("live, %s WETH", trim(t.WETH))
			}
			p("  %s\t%s\t%s", short(t.Address), t.Symbol, pool)
		}
	}

	if len(f.Gaps) > 0 {
		p("\nNOT CHECKED")
		for _, g := range f.Gaps {
			p("  - %s", g)
		}
	}
	return tw.Flush()
}

func short(a common.Address) string {
	h := a.Hex()
	return h[:6] + "…" + h[len(h)-4:]
}

func shortHash(h common.Hash) string {
	s := h.Hex()
	return s[:10] + "…"
}

func yesNo(b bool) string { return map[bool]string{true: "yes", false: "no"}[b] }

func pct(f float64) string {
	switch {
	case f == 0:
		return "0%"
	case f < 0.0001:
		return "<0.01%"
	}
	return fmt.Sprintf("%.2f%%", f*100)
}

func usd(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("$%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.1fk", v/1e3)
	}
	return fmt.Sprintf("$%.0f", v)
}

// trim formats a quantity with about four significant digits.
func trim(v float64) string {
	if v == 0 {
		return "0"
	}
	digits := max(0, 3-int(math.Floor(math.Log10(math.Abs(v)))))
	return fmt.Sprintf("%.*f", min(digits, 8), v)
}

// amount formats a raw token amount in whole units with thousands separators.
func amount(v *big.Int, decimals uint8) string {
	whole := new(big.Int).Quo(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)).String()
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.0f h", d.Hours())
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}
