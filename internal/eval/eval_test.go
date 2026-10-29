package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/facts"
	"github.com/amirradjou/rugcheck/internal/sim"
)

func ptr[T any](v T) *T { return &v }

func TestCheckFacts(t *testing.T) {
	deployer := common.HexToAddress("0xd0")
	f := &facts.Facts{
		Contract: facts.Contract{OwnerStatus: "wallet", Creation: &chain.Creation{Deployer: deployer}},
		Powers: []facts.Power{
			{Kind: facts.KindFees, Function: "setSellTax(uint256)", Probed: true, CallableBy: []string{"owner"}},
			{Kind: facts.KindBlacklist, Function: "blacklist(address,bool)", Probed: true, NoEffectFor: []string{"anyone"}},
			{Kind: facts.KindMint, Function: "mint(address,uint256)", Probed: true},
		},
		Settings:  []facts.Setting{{Kind: facts.KindFees, Getter: "sellTax()", Value: "100"}},
		Liquidity: &facts.Liquidity{LP: &facts.LPShares{Deployer: 0.998}},
		Deployer:  &facts.DeployerHistory{Tokens: make([]facts.PastToken, 3), Pulled: 2},
	}
	pass := Expect{
		Verified:          ptr(false), // unknown counts as "not claimed verified"
		OwnerStatus:       "wallet",
		Deployer:          &deployer,
		ViaFactory:        ptr(false),
		LivePowers:        []string{"fees"},
		NoEffectPowers:    []string{"blacklist"},
		Settings:          map[string]string{"sellTax()": "100"},
		Pulled:            ptr(false),
		LPDeployerMin:     ptr(0.99),
		DeployerTokensMin: ptr(3),
		DeployerPulledMin: ptr(1),
	}
	if s := Summary(CheckFacts(pass, f)); s != "ok" {
		t.Errorf("expected all checks to pass, got: %s", s)
	}

	fail := Expect{Verified: ptr(true), LivePowers: []string{}, LPBurnedMin: ptr(0.9), DeployerPulledMin: ptr(3)}
	s := Summary(CheckFacts(fail, f))
	for _, want := range []string{"verified: want true, got unknown", "live_powers: want [], got [fees]", "lp_burned", "deployer_pulled"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q lacks %q", s, want)
		}
	}
}

func TestCheckSim(t *testing.T) {
	honeypot := &sim.Result{Verdict: sim.Honeypot}
	if s := Summary(CheckSim(SimExpect{Flagged: true}, honeypot)); s != "ok" {
		t.Errorf("flagged honeypot: %s", s)
	}
	if s := Summary(CheckSim(SimExpect{Flagged: true}, &sim.Result{Verdict: sim.OK})); s == "ok" {
		t.Error("a missed honeypot passed")
	}
	// cannot-buy is not flagged: nobody can be trapped while buying reverts.
	if s := Summary(CheckSim(SimExpect{Flagged: false}, &sim.Result{Verdict: sim.CannotBuy})); s != "ok" {
		t.Errorf("cannot-buy on a legitimate token: %s", s)
	}
}

func TestLoadRejectsUnknownLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cases.json")
	raw, _ := json.Marshal([]Case{{Chain: "ethereum", Name: "x", Label: "maybe"}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("label \"maybe\" accepted")
	}
	raw, _ = json.Marshal([]Case{{Chain: "ethereum", Name: "x", Label: "scam"}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("a scam without a kind (honeypot or rug) accepted")
	}
}

func TestShippedCasesLoad(t *testing.T) {
	cases, err := Load("../../testdata/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	scams, legit := 0, 0
	for _, c := range cases {
		if c.Why == "" {
			t.Errorf("%s: no evidence for its label", c.Name)
		}
		if c.Label == "scam" {
			scams++
		} else {
			legit++
		}
	}
	if scams < 10 || legit < 10 {
		t.Errorf("%d scams and %d legitimate tokens; M0 needs 10 of each", scams, legit)
	}
}
