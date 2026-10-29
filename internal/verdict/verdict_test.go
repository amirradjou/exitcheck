package verdict

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/eval"
	"github.com/amirradjou/rugcheck/internal/facts"
	"github.com/amirradjou/rugcheck/internal/sim"
)

// TestLabelledSet judges the 20 labelled tokens from recorded facts and simulations (testdata/<name>.*.json,
// written by `rugcheck facts -json` and `rugcheck sim -json` on 2026-10-29): every scam must come out at least
// high risk, and no legitimate token worse than caution.
func TestLabelledSet(t *testing.T) {
	cases, err := eval.Load("../../testdata/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		f := load[facts.Facts](t, c.Name+".facts.json")
		s := load[sim.Result](t, c.Name+".sim.json")
		v := Judge(f, s, nil, f.CheckedAt)
		t.Logf("%-10s %-6s %-9s %s\n%s", c.Name, c.Label, v.Level, v.Headline, reasons(v))
		switch {
		case c.Label == "scam" && v.Level < HighRisk:
			t.Errorf("%s (%s) judged %s", c.Name, c.Kind, v.Level)
		case c.Label == "legit" && v.Level > Caution:
			t.Errorf("%s (legitimate) judged %s", c.Name, v.Level)
		}
	}
}

func load[T any](t *testing.T, name string) *T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &v
}

func reasons(v Verdict) string {
	var b strings.Builder
	for _, r := range v.Reasons {
		b.WriteString("    [" + r.Level.String() + "] " + r.Text + "\n")
	}
	if len(v.NotChecked) > 0 {
		b.WriteString("    not checked: " + strings.Join(v.NotChecked, " | ") + "\n")
	}
	return b.String()
}

var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func ok(s *sim.Result) *sim.Result {
	zero := 0.0
	s.Verdict, s.Buy, s.Sell, s.Transfer = sim.OK, &sim.Step{OK: true, Tax: &zero}, &sim.Step{OK: true, Tax: &zero}, &sim.Step{OK: true, Tax: &zero}
	return s
}

func TestEstablishedTokensKeepPowersAtCaution(t *testing.T) {
	launched := now.Add(-5 * 365 * 24 * time.Hour)
	f := &facts.Facts{
		Contract: facts.Contract{OwnerStatus: "wallet", CreatedAt: &launched},
		Powers:   []facts.Power{{Kind: facts.KindMint, Probed: true, CallableBy: []string{"owner"}}},
		Pools:    []facts.Pool{{USD: 50e6}},
	}
	v := Judge(f, ok(&sim.Result{}), nil, now)
	if v.Level != Caution || !strings.Contains(v.Reasons[0].Text, "known trade-off") {
		t.Errorf("established token: %s %+v", v.Level, v.Reasons)
	}

	young := now.Add(-48 * time.Hour)
	f.Contract.CreatedAt = &young
	if v := Judge(f, ok(&sim.Result{}), nil, now); v.Level != HighRisk {
		t.Errorf("a two-day-old token whose owner can mint: %s", v.Level)
	}
}

func TestOwnerAndDeployerPowersAreNotRepeated(t *testing.T) {
	f := &facts.Facts{
		Contract: facts.Contract{OwnerStatus: "wallet"},
		Powers: []facts.Power{
			{Kind: facts.KindBlacklist, Probed: true, CallableBy: []string{"owner", "deployer"}},
			{Kind: facts.KindFees, Probed: true, CallableBy: []string{"owner", "deployer"}},
		},
	}
	v := Judge(f, ok(&sim.Result{}), nil, now)
	var power []string
	for _, r := range v.Reasons {
		if r.Level == HighRisk {
			power = append(power, r.Text)
		}
	}
	if len(power) != 1 || power[0] != "The owner can block wallets from selling and change the taxes." {
		t.Errorf("power reasons: %q", power)
	}
}

func TestPulledLiquidityIsARug(t *testing.T) {
	f := &facts.Facts{Liquidity: &facts.Liquidity{Pulled: true, DEX: "uniswap-v2"}}
	v := Judge(f, nil, errors.New("no pool"), now)
	if v.Level != Scam || !strings.HasPrefix(v.Headline, "Rug pull") {
		t.Errorf("pulled: %s %q", v.Level, v.Headline)
	}
	if len(v.NotChecked) != 1 {
		t.Errorf("simulation error not listed as not checked: %q", v.NotChecked)
	}
}

func TestHoneypotHeadline(t *testing.T) {
	s := &sim.Result{Verdict: sim.Honeypot, Block: 9, Reasons: []string{"selling reverts: TRANSFER_FROM_FAILED"}}
	v := Judge(&facts.Facts{Address: common.HexToAddress("0x1")}, s, nil, now)
	if v.Level != Scam || !strings.HasPrefix(v.Headline, "Honeypot") || !strings.Contains(v.Reasons[0].Text, "block 9") {
		t.Errorf("honeypot: %s %q %+v", v.Level, v.Headline, v.Reasons)
	}
}

// deployed returns facts whose deployer is known, the baseline a clear verdict needs.
func deployed() *facts.Facts {
	return &facts.Facts{Contract: facts.Contract{OwnerStatus: "renounced",
		Creation: &chain.Creation{Deployer: common.HexToAddress("0xd0")}}}
}

func TestUnfinishedChecksAreNotClear(t *testing.T) {
	f := deployed()
	f.Gaps = []string{"power probes: Post https://rpc: context deadline exceeded"}
	if v := Judge(f, ok(&sim.Result{}), nil, now); v.Level != Caution {
		t.Errorf("power probes missing: %s", v.Level)
	}
	if v := Judge(&facts.Facts{}, nil, errors.New("fork failed"), now); v.Level != Caution {
		t.Errorf("simulation missing: %s", v.Level)
	}
	f.Gaps = []string{"holders: too much history"}
	if v := Judge(f, ok(&sim.Result{}), nil, now); v.Level != Clear || !v.Complete {
		t.Errorf("a missing holder list alone should not block clear: %s %+v", v.Level, v.Reasons)
	}
	// Without the deployer, the probes could not ask whether it kept a switch after renouncing ownership.
	f.Gaps = []string{"creation block: context deadline exceeded"}
	if v := Judge(f, ok(&sim.Result{}), nil, now); v.Level != Caution || v.Complete {
		t.Errorf("creation lookup missing: %s, complete %v", v.Level, v.Complete)
	}
	f = deployed()
	f.Contract.Creation = nil
	if v := Judge(f, ok(&sim.Result{}), nil, now); v.Level != Caution {
		t.Errorf("unknown deployer: %s", v.Level)
	}
}

func TestUpgradeableProxies(t *testing.T) {
	for _, c := range []struct {
		name  string
		proxy facts.Proxy
		owner string
		want  Level
	}{
		{"UUPS with an owner", facts.Proxy{Kind: "EIP-1822 (UUPS)"}, "wallet", HighRisk},
		{"EIP-1967 without an admin slot", facts.Proxy{Kind: "EIP-1967"}, "wallet", HighRisk},
		{"UUPS, owner renounced", facts.Proxy{Kind: "EIP-1822 (UUPS)"}, "renounced", Clear},
		{"beacon", facts.Proxy{Kind: "EIP-1967 beacon"}, "renounced", HighRisk},
		{"transparent, admin burned", facts.Proxy{Kind: "EIP-1967", Admin: &chain.DeadAddress}, "renounced", Clear},
		{"minimal proxy", facts.Proxy{Kind: "EIP-1167 minimal proxy (fixed implementation)"}, "renounced", Clear},
	} {
		f := deployed()
		f.Contract.OwnerStatus, f.Contract.Proxy = c.owner, &c.proxy
		if v := Judge(f, ok(&sim.Result{}), nil, now); v.Level != c.want {
			t.Errorf("%s: %s, want %s (%+v)", c.name, v.Level, c.want, v.Reasons)
		}
	}
}

func TestTaxesAreNeverReassurances(t *testing.T) {
	for _, c := range []struct {
		tax  float64
		want Level
	}{{0.05, Caution}, {0.95, HighRisk}} {
		s := ok(&sim.Result{})
		s.Sell.Tax = &c.tax
		v := Judge(deployed(), s, nil, now)
		for _, r := range v.Reasons {
			if strings.HasPrefix(r.Text, "Taxes") && r.Level != c.want {
				t.Errorf("sell tax %.0f%%: reason level %s, want %s", 100*c.tax, r.Level, c.want)
			}
		}
	}
}

func TestShareAndAgeWording(t *testing.T) {
	for share, want := range map[float64]string{0.9997: "99.9%", 0.996: "99.6%", 1: "100%", 0.5: "50%"} {
		if got := pct(share); got != want {
			t.Errorf("pct(%v) = %s, want %s", share, got, want)
		}
	}
	for d, want := range map[time.Duration]string{20 * time.Second: "less than a minute", 90 * time.Second: "1 minute", 5 * time.Minute: "5 minutes"} {
		if got := age(d); got != want {
			t.Errorf("age(%v) = %s, want %s", d, got, want)
		}
	}
}

func TestLevelText(t *testing.T) {
	for l := Clear; l <= Scam; l++ {
		b, _ := l.MarshalText()
		got, err := ParseLevel(string(b))
		if err != nil || got != l {
			t.Errorf("%v round-trips to %v, %v", l, got, err)
		}
	}
	if _, err := ParseLevel("maybe"); err == nil {
		t.Error("unknown level parsed")
	}
}
