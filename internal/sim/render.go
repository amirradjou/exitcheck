package sim

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode"
)

// Render writes a plain-text report of a simulation.
func Render(w io.Writer, r *Result) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// Revert reasons are written by the token's author: print none of their control characters.
	p := func(format string, args ...any) {
		for i, a := range args {
			if s, ok := a.(string); ok {
				args[i] = printable(s)
			}
		}
		fmt.Fprintf(tw, format+"\n", args...)
	}
	p("BUY-THEN-SELL SIMULATION · %s · fork of block %d · %.1fs", r.Chain, r.Block, r.Elapsed.Seconds())
	if r.Venue != "" {
		p("  venue\t%s", r.Venue)
		p("  spent\t%.6g ETH from a fresh wallet", r.SpentETH)
	}
	step := func(name string, s *Step) {
		if s == nil {
			return
		}
		if !s.OK {
			p("  %s\tREVERTS: %s", name, s.Error)
			return
		}
		line := "ok"
		if s.Tax != nil {
			line += fmt.Sprintf(", token took %.1f%%", 100**s.Tax)
		}
		if s.Gas > 0 {
			line += fmt.Sprintf(", %d gas", s.Gas)
		}
		p("  %s\t%s", name, line)
	}
	step("buy", r.Buy)
	step("transfer", r.Transfer)
	step("sell", r.Sell)
	step("sell half", r.SellHalf)
	p("  verdict\t%s", strings.ToUpper(r.Verdict))
	for _, reason := range r.Reasons {
		p("  \t- %s", reason)
	}
	p("  caveat\t%s", r.BlindSpot)
	return tw.Flush()
}

func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return unicode.ReplacementChar
	}, s)
}
