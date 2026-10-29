// Package eval checks rugcheck against hand-labelled tokens. Each case states why the token is labelled scam or
// legitimate and lists facts established independently of rugcheck (block explorer records, cast calls against
// another RPC, the verified source); a run passes when every stated fact comes out the same.
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/facts"
)

// Case is one labelled token.
type Case struct {
	Chain   string         `json:"chain"`
	Address common.Address `json:"address"`
	Name    string         `json:"name"`
	Label   string         `json:"label"` // "scam" or "legit"
	// Why is the evidence for the label.
	Why    string `json:"why"`
	Expect Expect `json:"expect"`
}

// Expect lists facts to check; a field left out is not checked.
type Expect struct {
	// Verified true requires verified source. False requires that rugcheck does not claim verified source (it may
	// say unknown: without an Etherscan key, "not on Sourcify or Blockscout" is not a no).
	Verified    *bool           `json:"verified,omitempty"`
	Proxy       *bool           `json:"proxy,omitempty"`
	OwnerStatus string          `json:"owner_status,omitempty"`
	Deployer    *common.Address `json:"deployer,omitempty"`
	CreationTx  *common.Hash    `json:"creation_tx,omitempty"`
	ViaFactory  *bool           `json:"via_factory,omitempty"`
	// LivePowers are the power kinds someone (owner, deployer or anyone) can use with effect; the set must match.
	LivePowers []string `json:"live_powers,omitempty"`
	// NoEffectPowers are power kinds that accept calls but change nothing for every caller tried.
	NoEffectPowers []string          `json:"no_effect_powers,omitempty"`
	Settings       map[string]string `json:"settings,omitempty"`
	Pulled         *bool             `json:"liquidity_pulled,omitempty"`
	LPBurnedMin    *float64          `json:"lp_burned_min,omitempty"`
	LPDeployerMin  *float64          `json:"lp_deployer_min,omitempty"`
	// DeployerTokensMin and DeployerPulledMin are lower bounds: the deployer can launch more after the label.
	DeployerTokensMin *int `json:"deployer_tokens_min,omitempty"`
	DeployerPulledMin *int `json:"deployer_pulled_min,omitempty"`
}

// Load reads a JSON list of cases.
func Load(path string) ([]Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []Case
	if err := json.Unmarshal(raw, &cases); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i, c := range cases {
		if c.Label != "scam" && c.Label != "legit" {
			return nil, fmt.Errorf("%s: case %d (%s): label %q, want scam or legit", path, i, c.Name, c.Label)
		}
	}
	return cases, nil
}

// Check is the outcome of one expected fact.
type Check struct {
	Field string
	Want  string
	Got   string
	OK    bool
}

// CheckFacts compares facts with a case's expectations.
func CheckFacts(e Expect, f *facts.Facts) []Check {
	var out []Check
	add := func(field string, want, got any, ok bool) {
		out = append(out, Check{Field: field, Want: fmt.Sprint(want), Got: fmt.Sprint(got), OK: ok})
	}
	c := f.Contract
	if e.Verified != nil {
		got := "unknown"
		if c.Verified != nil {
			got = fmt.Sprint(*c.Verified)
		}
		if *e.Verified {
			add("verified", true, got, got == "true")
		} else {
			add("verified", "false or unknown", got, got != "true")
		}
	}
	if e.Proxy != nil {
		add("proxy", *e.Proxy, c.Proxy != nil, *e.Proxy == (c.Proxy != nil))
	}
	if e.OwnerStatus != "" {
		add("owner_status", e.OwnerStatus, c.OwnerStatus, e.OwnerStatus == c.OwnerStatus)
	}
	if e.Deployer != nil || e.CreationTx != nil || e.ViaFactory != nil {
		cr := c.Creation
		if cr == nil {
			cr = &chain.Creation{}
		}
		if e.Deployer != nil {
			add("deployer", e.Deployer.Hex(), cr.Deployer.Hex(), *e.Deployer == cr.Deployer)
		}
		if e.CreationTx != nil {
			add("creation_tx", e.CreationTx.Hex(), cr.Tx.Hex(), *e.CreationTx == cr.Tx)
		}
		if e.ViaFactory != nil {
			add("via_factory", *e.ViaFactory, cr.Factory != nil, *e.ViaFactory == (cr.Factory != nil))
		}
	}
	if e.LivePowers != nil {
		got := kinds(f.Powers, func(p facts.Power) bool { return len(p.CallableBy) > 0 })
		want := sorted(e.LivePowers)
		add("live_powers", want, got, slices.Equal(want, got))
	}
	if e.NoEffectPowers != nil {
		got := kinds(f.Powers, func(p facts.Power) bool { return len(p.CallableBy) == 0 && len(p.NoEffectFor) > 0 })
		want := sorted(e.NoEffectPowers)
		add("no_effect_powers", want, got, slices.Equal(want, got))
	}
	for getter, want := range e.Settings {
		got := "(not read)"
		for _, s := range f.Settings {
			if s.Getter == getter {
				got = s.Value
			}
		}
		add("setting "+getter, want, got, got == want)
	}
	l := f.Liquidity
	if e.Pulled != nil {
		got := l != nil && l.Pulled
		add("liquidity_pulled", *e.Pulled, got, got == *e.Pulled)
	}
	lpShare := func(get func(*facts.LPShares) float64) (float64, bool) {
		if l == nil || l.LP == nil {
			return 0, false
		}
		return get(l.LP), true
	}
	if e.LPBurnedMin != nil {
		got, ok := lpShare(func(s *facts.LPShares) float64 { return s.Burned })
		add("lp_burned", fmt.Sprintf(">= %.2f", *e.LPBurnedMin), shareOrUnknown(got, ok), ok && got >= *e.LPBurnedMin)
	}
	if e.LPDeployerMin != nil {
		got, ok := lpShare(func(s *facts.LPShares) float64 { return s.Deployer })
		add("lp_deployer", fmt.Sprintf(">= %.2f", *e.LPDeployerMin), shareOrUnknown(got, ok), ok && got >= *e.LPDeployerMin)
	}
	if e.DeployerTokensMin != nil || e.DeployerPulledMin != nil {
		tokens, pulled := -1, -1
		if d := f.Deployer; d != nil {
			tokens, pulled = len(d.Tokens), d.Pulled
		}
		if e.DeployerTokensMin != nil {
			add("deployer_tokens", fmt.Sprintf(">= %d", *e.DeployerTokensMin), tokens, tokens >= *e.DeployerTokensMin)
		}
		if e.DeployerPulledMin != nil {
			add("deployer_pulled", fmt.Sprintf(">= %d", *e.DeployerPulledMin), pulled, pulled >= *e.DeployerPulledMin)
		}
	}
	return out
}

func kinds(ps []facts.Power, keep func(facts.Power) bool) []string {
	set := map[string]bool{}
	for _, p := range ps {
		if keep(p) {
			set[p.Kind] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	sort.Strings(out)
	return out
}

func shareOrUnknown(v float64, ok bool) string {
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%.4f", v)
}

// Summary formats failed checks, or "ok".
func Summary(checks []Check) string {
	var bad []string
	for _, c := range checks {
		if !c.OK {
			bad = append(bad, fmt.Sprintf("%s: want %s, got %s", c.Field, c.Want, c.Got))
		}
	}
	if len(bad) == 0 {
		return "ok"
	}
	return strings.Join(bad, "; ")
}
