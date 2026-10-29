// Package verdict turns contract facts and a buy-then-sell simulation into a risk level with the reasons for it.
// The rules are fixed and explainable: the level comes from checks, never from a language model (README, section 3).
package verdict

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/facts"
	"github.com/amirradjou/rugcheck/internal/sim"
)

// Level is how dangerous a token looks, from Clear to Scam.
type Level int

const (
	Clear    Level = iota // no red flags found
	Caution               // things a buyer should know, none of them a trap on its own
	HighRisk              // someone can take a holder's money, or the token already takes a large cut
	Scam                  // a buyer cannot get out: the sale fails, or the liquidity is gone
)

var levelNames = []string{"clear", "caution", "high-risk", "scam"}

func (l Level) String() string { return levelNames[l] }

// MarshalText writes the level's name, so cached and JSON verdicts survive reordering the constants.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// UnmarshalText reads a level's name.
func (l *Level) UnmarshalText(b []byte) error {
	for i, n := range levelNames {
		if n == string(b) {
			*l = Level(i)
			return nil
		}
	}
	return fmt.Errorf("unknown level %q", b)
}

// ParseLevel reads a level's name.
func ParseLevel(s string) (Level, error) {
	var l Level
	err := l.UnmarshalText([]byte(s))
	return l, err
}

// Reason is one finding behind a verdict, with the level it alone would justify.
type Reason struct {
	Level Level  `json:"level"`
	Text  string `json:"text"`
}

// Verdict is the judgement on one token.
type Verdict struct {
	Level    Level  `json:"level"`
	Headline string `json:"headline"`
	// Reasons are ordered from most to least severe; Clear-level reasons are reassurances.
	Reasons []Reason `json:"reasons"`
	// NotChecked lists checks that could not be made; a verdict is only as complete as its checks.
	NotChecked []string `json:"not_checked"`
	// Complete is false when a decisive check did not finish: the answer may be milder than the truth, so it should
	// be retried soon rather than cached.
	Complete bool `json:"complete"`
}

// A token earns the benefit of the doubt for its owner's powers with a record: a year on chain while keeping real
// liquidity. USDT's owner can mint, blacklist and pause, and has for years. Powers on such a token are Caution, not
// HighRisk; a scam does not keep six figures in its pools for a year.
const (
	establishedAge = 365 * 24 * time.Hour
	establishedUSD = 100_000
)

// Thresholds for the Caution findings.
const (
	youngPool       = 72 * time.Hour
	thinLiquidity   = 10_000 // USD across all pools
	concentratedTop = 0.2    // one sellable wallet's share of the supply
	insiderLP       = 0.5    // LP share the deployer and owner can withdraw
	burnedLP        = 0.9
)

// powerWords phrases each kind of privileged function as what its holder can do.
var powerWords = map[string]string{
	facts.KindMint:      "mint new tokens",
	facts.KindBlacklist: "block wallets from selling",
	facts.KindPause:     "pause transfers",
	facts.KindFees:      "change the taxes",
	facts.KindTrading:   "switch trading off",
	facts.KindLimits:    "change transaction and wallet limits",
	facts.KindSeize:     "take tokens from wallets",
	facts.KindReclaim:   "take back ownership after renouncing it",
	facts.KindUpgrade:   "replace the contract's code",
}

// trapKinds are powers that can trap holders on their own; limits only matter alongside them.
var trapKinds = map[string]bool{
	facts.KindMint: true, facts.KindBlacklist: true, facts.KindPause: true, facts.KindFees: true,
	facts.KindTrading: true, facts.KindSeize: true, facts.KindReclaim: true, facts.KindUpgrade: true,
}

// decisive are the facts gaps (by label) without which a token cannot be called clear: who can use its powers
// (which needs the owner and the deployer), its pools and whether they were pulled, and whether its code can be
// swapped.
var decisive = []string{"power probes", "pools", "LP supply", "proxy check", "token metadata, owner and settings",
	"owner() reverted", "creation block", "deployer"}

// lpDecisive are gaps that hide whether insiders can pull the liquidity; an established token is exempt.
var lpDecisive = []string{"LP holders", "LP lock"}

// Judge rates a token. s may be nil when the simulation did not run; simErr says why.
func Judge(f *facts.Facts, s *sim.Result, simErr error, now time.Time) Verdict {
	j := judge{f: f, now: now}
	j.simulation(s, simErr)
	if f != nil {
		j.liquidity()
		j.powers()
		j.deployer()
		j.contract()
		j.holders()
		for _, g := range f.Gaps {
			j.v.NotChecked = append(j.v.NotChecked, g)
		}
	}
	j.incomplete(f, s)
	sort.SliceStable(j.v.Reasons, func(a, b int) bool { return j.v.Reasons[a].Level > j.v.Reasons[b].Level })
	for _, r := range j.v.Reasons {
		j.v.Level = max(j.v.Level, r.Level)
	}
	j.v.Headline = j.headline()
	if j.v.Reasons == nil {
		j.v.Reasons = []Reason{}
	}
	if j.v.NotChecked == nil {
		j.v.NotChecked = []string{}
	}
	return j.v
}

type judge struct {
	f   *facts.Facts
	now time.Time
	v   Verdict
	// headline candidates, set by the finding that decides the level
	honeypot, pulled bool
}

// incomplete keeps a token whose decisive checks did not all finish from being called clear.
func (j *judge) incomplete(f *facts.Facts, s *sim.Result) {
	var missing []string
	if s == nil {
		missing = append(missing, "the buy-then-sell simulation")
	}
	if f != nil {
		labels := decisive
		if !j.established() {
			labels = append(slices.Clone(decisive), lpDecisive...)
		}
		for _, label := range labels {
			for _, g := range f.Gaps {
				if strings.HasPrefix(g, label+":") || g == label {
					missing = append(missing, label)
					break
				}
			}
		}
		if (f.Contract.Creation == nil || f.Contract.Creation.Deployer == (common.Address{})) && !slices.Contains(missing, "deployer") && !slices.Contains(missing, "creation block") {
			missing = append(missing, "who deployed it")
		}
	}
	j.v.Complete = len(missing) == 0
	if !j.v.Complete {
		j.add(Caution, "Some decisive checks did not finish (%s), so this is not a clean bill of health.", strings.Join(missing, ", "))
	}
}

func (j *judge) add(l Level, format string, args ...any) {
	j.v.Reasons = append(j.v.Reasons, Reason{Level: l, Text: fmt.Sprintf(format, args...)})
}

func (j *judge) simulation(s *sim.Result, simErr error) {
	if s == nil {
		if simErr != nil {
			j.v.NotChecked = append(j.v.NotChecked, "buy-then-sell simulation: "+simErr.Error())
		}
		return
	}
	first := ""
	if len(s.Reasons) > 0 {
		first = s.Reasons[0]
	}
	switch s.Verdict {
	case sim.Honeypot:
		j.honeypot = true
		j.add(Scam, "A test buy worked but the sale failed (%s), on a fork of block %d.", first, s.Block)
	case sim.SellLimited:
		j.add(HighRisk, "A small position could not be sold in one go (%s); only half of it sold.", first)
	case sim.TransferBlocked:
		j.add(HighRisk, "Tokens cannot be sent to another wallet (%s).", first)
	case sim.CannotBuy:
		j.add(Caution, "Buying fails right now (%s): trading may not be open, or new buyers are blocked.", first)
	case sim.NoPool:
		j.add(Caution, "There is no pool to sell into (%s).", first)
	}
	if t, top := taxes(s); t != "" {
		l := Caution
		if top >= highTax {
			l = HighRisk
		}
		j.add(l, "Taxes in a test trade: %s.", t)
	}
	if t, _ := taxes(s); s.Verdict == sim.OK && t == "" {
		j.add(Clear, "A test buy, transfer and sale on a fork of block %d all worked, with no tax.", s.Block)
	} else if s.Verdict == sim.OK {
		j.add(Clear, "A test buy, transfer and sale on a fork of block %d all worked.", s.Block)
	}
}

// highTax matches the simulation's high-tax threshold.
const highTax = 0.1

// taxes lists the non-zero taxes a simulation measured, and the largest.
func taxes(s *sim.Result) (string, float64) {
	var parts []string
	top := 0.0
	for _, st := range []struct {
		name string
		step *sim.Step
	}{{"buy", s.Buy}, {"sell", s.Sell}, {"transfer", s.Transfer}} {
		if st.step != nil && st.step.Tax != nil && *st.step.Tax > 0 {
			parts = append(parts, fmt.Sprintf("%s %.1f%%", st.name, 100**st.step.Tax))
			top = max(top, *st.step.Tax)
		}
	}
	return strings.Join(parts, ", "), top
}

func (j *judge) liquidity() {
	l := j.f.Liquidity
	if l != nil && l.Pulled {
		j.pulled = true
		j.add(Scam, "The liquidity was pulled from the main pool (%s): there is almost nothing left to sell into.", l.DEX)
		return
	}
	usd := j.poolUSD()
	switch {
	case len(j.f.Pools) > 0 && usd < 1:
		j.add(Caution, "Almost no liquidity is left in any pool (%s).", dollars(usd))
	case len(j.f.Pools) > 0 && usd < thinLiquidity:
		j.add(Caution, "Only %s of liquidity in all pools: a few sales move the price a lot.", dollars(usd))
	}
	if l == nil {
		return
	}
	if l.CreatedAt != nil && j.now.Sub(*l.CreatedAt) < youngPool {
		j.add(Caution, "The main pool is %s old.", age(j.now.Sub(*l.CreatedAt)))
	}
	if l.LP == nil {
		return
	}
	insiders := l.LP.Deployer + l.LP.Owner
	switch {
	case insiders >= insiderLP:
		j.add(j.unlessEstablished(HighRisk), "The deployer or owner holds %s of the main pool's LP tokens and can withdraw that liquidity at any time.", pct(insiders))
	case l.LP.Burned >= burnedLP:
		j.add(Clear, "%s of the main pool's LP tokens are burned: that liquidity can never be withdrawn.", pct(l.LP.Burned))
	}
	for _, lock := range l.LP.Locks {
		if lock.Unlock != nil {
			j.add(Clear, "%s of the LP tokens are locked with %s until %s.", pct(lock.Share), lock.Locker, lock.Unlock.Format("2006-01-02"))
		}
	}
}

func (j *judge) powers() {
	who := map[string][]string{} // caller → what they can do, in kind order
	seen := map[string]bool{}
	for _, p := range j.f.Powers {
		if !trapKinds[p.Kind] && p.Kind != facts.KindLimits {
			continue
		}
		for _, c := range p.CallableBy {
			if seen[c+p.Kind] {
				continue
			}
			seen[c+p.Kind] = true
			who[c] = append(who[c], p.Kind)
		}
	}
	// The deployer is usually also the owner, and anyone includes both: each caller lists only what a broader one
	// cannot already do.
	said := map[string]bool{}
	for _, caller := range []string{"anyone", "owner", "deployer"} {
		var kinds []string
		for _, k := range who[caller] {
			if !said[k] {
				kinds = append(kinds, k)
				said[k] = true
			}
		}
		if !hasTrap(kinds) {
			continue
		}
		level := HighRisk
		if caller != "anyone" {
			level = j.unlessEstablished(HighRisk)
		}
		subject := map[string]string{"anyone": "Anyone", "owner": "The owner", "deployer": "The deployer"}[caller]
		if caller == "deployer" && j.f.Contract.OwnerStatus == "renounced" {
			subject = "Ownership is renounced, but the deployer"
		}
		j.add(level, "%s can %s.%s", subject, list(kinds), j.establishedNote(level))
	}
	if j.upgradeable() {
		level := j.unlessEstablished(HighRisk)
		j.add(level, "The token is an upgradeable proxy (%s): whoever controls upgrades can replace its code at any time.%s",
			j.f.Contract.Proxy.Kind, j.establishedNote(level))
	}
}

// upgradeable reports whether the token's code can still be replaced. Upgrade functions are never probed (a probe
// would have to deploy new code), so this reads the proxy: a transparent proxy upgrades through its admin; a UUPS or
// beacon proxy through the implementation's own access control (normally the owner) or the beacon's owner, which
// facts does not read, so only a renounced owner of a UUPS proxy counts as locked.
func (j *judge) upgradeable() bool {
	p := j.f.Contract.Proxy
	switch {
	case p == nil || strings.HasPrefix(p.Kind, "EIP-1167"):
		return false
	case p.Admin != nil:
		return !chain.IsBurn(*p.Admin)
	case strings.Contains(p.Kind, "beacon"):
		return true
	}
	return j.f.Contract.OwnerStatus != "renounced"
}

func hasTrap(kinds []string) bool {
	for _, k := range kinds {
		if trapKinds[k] {
			return true
		}
	}
	return false
}

func list(kinds []string) string {
	words := make([]string, len(kinds))
	for i, k := range kinds {
		words[i] = powerWords[k]
	}
	switch len(words) {
	case 1:
		return words[0]
	case 2:
		return words[0] + " and " + words[1]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

func (j *judge) deployer() {
	d := j.f.Deployer
	if d == nil || d.Pulled == 0 {
		return
	}
	j.add(HighRisk, "The deployer launched %d other tokens and pulled the liquidity of %d.", len(d.Tokens), d.Pulled)
}

func (j *judge) contract() {
	c := j.f.Contract
	if c.Verified != nil && !*c.Verified {
		j.add(Caution, "The source code is not published, so nobody can read what the contract does.")
	}
	if c.OwnerStatus == "renounced" {
		j.add(Clear, "Ownership is renounced.")
	}
}

func (j *judge) holders() {
	if d := j.f.Shares.Deployer; d >= concentratedTop {
		j.add(Caution, "The deployer still holds %s of the supply and could sell it at once.", pct(d))
		return
	}
	if h := j.f.Holders; h != nil && h.TopShare >= concentratedTop {
		j.add(Caution, "One wallet holds %s of the supply and could sell it at once.", pct(h.TopShare))
	}
}

// established reports whether the token has a year of history and deep liquidity.
func (j *judge) established() bool {
	c := j.f.Contract.CreatedAt
	return c != nil && j.now.Sub(*c) >= establishedAge && j.poolUSD() >= establishedUSD
}

func (j *judge) unlessEstablished(l Level) Level {
	if j.established() {
		return Caution
	}
	return l
}

func (j *judge) establishedNote(l Level) string {
	if l != Caution || !j.established() {
		return ""
	}
	return fmt.Sprintf(" It has %s of history and %s in pools, so this is a known trade-off, not a trap.",
		age(j.now.Sub(*j.f.Contract.CreatedAt)), dollars(j.poolUSD()))
}

func (j *judge) poolUSD() float64 {
	sum := 0.0
	for _, p := range j.f.Pools {
		sum += p.USD
	}
	return sum
}

func (j *judge) headline() string {
	switch {
	case j.honeypot:
		return "Honeypot: you can buy, but you cannot sell"
	case j.pulled:
		return "Rug pull: the liquidity is gone"
	}
	return map[Level]string{
		Clear:    "No red flags found",
		Caution:  "Caution: no trap found, but read the notes",
		HighRisk: "High risk: see the reasons below",
		Scam:     "Scam",
	}[j.v.Level]
}

// pct formats a share, keeping a decimal near 100% so 99.6% does not read as everything. The decimal is rounded down:
// 99.97% must not print as 100.0%.
func pct(share float64) string {
	if share >= 0.995 && share < 1 {
		return fmt.Sprintf("%.1f%%", math.Floor(1000*share)/10)
	}
	return fmt.Sprintf("%.0f%%", 100*share)
}

func dollars(usd float64) string {
	switch {
	case usd >= 1e9:
		return fmt.Sprintf("$%.1fB", usd/1e9)
	case usd >= 1e6:
		return fmt.Sprintf("$%.1fM", usd/1e6)
	case usd >= 1e3:
		return fmt.Sprintf("$%.1fk", usd/1e3)
	case usd < 1:
		return fmt.Sprintf("$%.2f", usd)
	}
	return fmt.Sprintf("$%.0f", usd)
}

func age(d time.Duration) string {
	switch {
	case d >= 2*365*24*time.Hour:
		return fmt.Sprintf("%d years", int(d.Hours()/24/365))
	case d >= 60*24*time.Hour:
		return fmt.Sprintf("%d months", int(d.Hours()/24/30))
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d < time.Minute:
		return "less than a minute"
	case d < 2*time.Minute:
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}
