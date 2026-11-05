// Package check runs everything rugcheck knows how to check about one token (contract facts and the buy-then-sell
// simulation, side by side) and turns the result into a verdict and a short plain-text answer.
package check

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/evm"
	"github.com/amirradjou/rugcheck/internal/explorer"
	"github.com/amirradjou/rugcheck/internal/facts"
	"github.com/amirradjou/rugcheck/internal/sim"
	"github.com/amirradjou/rugcheck/internal/verdict"
)

// Report is one token's check.
type Report struct {
	Chain     string          `json:"chain"`
	Address   common.Address  `json:"address"`
	Name      string          `json:"name"`
	Symbol    string          `json:"symbol"`
	Block     uint64          `json:"block"`
	CheckedAt time.Time       `json:"checked_at"`
	Verdict   verdict.Verdict `json:"verdict"`
	Facts     *facts.Facts    `json:"facts"`
	Sim       *sim.Result     `json:"sim,omitempty"`
	SimError  string          `json:"sim_error,omitempty"`
	Elapsed   time.Duration   `json:"elapsed_ns"`
}

// ErrNoContract is returned for an address without contract code.
var ErrNoContract = errors.New("no contract at this address")

// Budgets bound each half of a check. Free RPCs throttle in bursts (mevblocker's Cloudflare limit imposes a 30 s
// cooldown), which once turned a 6 s check into 74 s: a facts stage still running when its budget ends becomes a
// check not made, and the answer goes out without it.
var (
	FactsBudget = 20 * time.Second
	SimBudget   = 30 * time.Second
)

// Run checks a token. Facts and the simulation run at the same time: one waits on RPC round trips, the other on a
// local fork, and an answer has to come back in seconds. It fails only when the facts cannot start (no code, RPC
// down); a simulation failure is reported in the verdict as a check not made.
func Run(ctx context.Context, c *chain.Client, ex *explorer.Client, token common.Address) (*Report, error) {
	start := time.Now()
	fctx, cancelFacts := context.WithTimeout(ctx, FactsBudget)
	defer cancelFacts()
	sctx, cancelSim := context.WithTimeout(ctx, SimBudget)
	defer cancelSim()
	var (
		f          *facts.Facts
		s          *sim.Result
		fErr, sErr error
		wg         sync.WaitGroup
	)
	wg.Go(func() { f, fErr = facts.Gather(fctx, c, ex, token) })
	wg.Go(func() { s, sErr = sim.Run(sctx, c, token, sim.Options{}) })
	wg.Wait()
	if fErr != nil {
		if strings.Contains(fErr.Error(), "has no contract code") {
			return nil, ErrNoContract
		}
		return nil, fErr
	}
	r := &Report{
		Chain: c.Net.Name, Address: token, Name: f.Token.Name, Symbol: f.Token.Symbol, Block: f.Block,
		CheckedAt: f.CheckedAt, Facts: f, Sim: s,
		Verdict: verdict.Judge(f, s, sErr, f.AsOf()),
	}
	if sErr != nil {
		r.SimError = sErr.Error()
	}
	r.Elapsed = time.Since(start)
	return r, nil
}

// Locate returns the chains, among clients, where address holds contract code. An EIP-7702 wallet's 23-byte
// delegation is not a contract.
func Locate(ctx context.Context, clients []*chain.Client, address common.Address) ([]string, error) {
	found := make([]bool, len(clients))
	errs := make([]error, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Go(func() {
			code, err := c.Code(ctx, address)
			found[i], errs[i] = err == nil && evm.IsContract(code), err
		})
	}
	wg.Wait()
	var chains []string
	for i, c := range clients {
		if found[i] {
			chains = append(chains, c.Net.Name)
		}
	}
	if len(chains) == 0 {
		if err := errors.Join(errs...); err != nil {
			return nil, fmt.Errorf("looking up the address: %w", chain.StripURL(err))
		}
	}
	return chains, nil
}

var icons = map[verdict.Level]string{verdict.Clear: "🟢", verdict.Caution: "🟡", verdict.HighRisk: "🟠", verdict.Scam: "🔴"}

// Render writes the report as a short plain-text answer: the verdict, its reasons, what was not checked, and the
// caveat. Every line passes through printable and Defang: names and revert reasons are written by the token's author.
func Render(r *Report) string {
	var b strings.Builder
	line := func(format string, args ...any) {
		b.WriteString(printable(Defang(fmt.Sprintf(format, args...))))
		b.WriteByte('\n')
	}
	v := r.Verdict
	line("%s %s", icons[v.Level], v.Headline)
	name := r.Name
	if r.Symbol != "" && r.Symbol != r.Name {
		name += " (" + r.Symbol + ")"
	}
	if name == "" {
		name = "Unnamed token"
	}
	line("%s on %s", name, chainTitle(r.Chain))
	line("%s", r.Address.Hex())
	b.WriteByte('\n')
	for _, reason := range v.Reasons {
		mark := "•"
		if reason.Level == verdict.Clear {
			mark = "✓"
		}
		line("%s %s", mark, clip(reason.Text, 400))
	}
	if len(v.NotChecked) > 0 {
		b.WriteByte('\n')
		line("Not checked: %s.", strings.Join(gapNames(v.NotChecked), "; "))
	}
	b.WriteByte('\n')
	line("Block %d · checked in %.1f s. A clean result is not a guarantee: a contract can treat later buyers differently. Not financial advice.",
		r.Block, r.Elapsed.Seconds())
	return strings.TrimRight(b.String(), "\n")
}

// gapNames keeps the part of each gap before its explanation ("holders: too much history …" → "holders").
func gapNames(gaps []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range gaps {
		name, _, _ := strings.Cut(g, ":")
		name = clip(strings.TrimSpace(name), 60)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func chainTitle(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

var (
	schemeRE = regexp.MustCompile(`(?i)\b(https?)://`)
	hostRE   = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}\b`)
	handleRE = regexp.MustCompile(`(^|\s)@(\w{3,})`)
)

// Defang stops links and Telegram handles in author-written text from being live. Telegram turns "evil.xyz" and "@evil"
// into links in plain text, and these answers go to a public channel: a token's name would otherwise be a free phishing
// post, sent by this bot, about a token it has just called dangerous.
func Defang(s string) string {
	s = schemeRE.ReplaceAllString(s, "${1}[:]//")
	s = hostRE.ReplaceAllStringFunc(s, func(h string) string {
		if strings.HasSuffix(strings.ToLower(h), ".sol") {
			return h // a source file, which Telegram does not link
		}
		return strings.ReplaceAll(h, ".", "[.]")
	})
	return handleRE.ReplaceAllString(s, "${1}(at)${2}")
}

// printable replaces control and other non-printing characters, keeping newlines out of single lines.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return unicode.ReplacementChar
	}, s)
}
