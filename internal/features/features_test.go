package features

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/amirradjou/rugcheck/internal/check"
	"github.com/amirradjou/rugcheck/internal/facts"
)

// report loads one of the 20 labelled tokens recorded for the verdict's tests.
func report(t *testing.T, name string) *check.Report {
	t.Helper()
	var r check.Report
	for file, v := range map[string]any{name + ".facts.json": &r.Facts, name + ".sim.json": &r.Sim} {
		raw, err := os.ReadFile(filepath.Join("..", "verdict", "testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	return &r
}

func get(t *testing.T, v []float64, name string) float64 {
	t.Helper()
	for i, n := range Names {
		if n == name {
			return v[i]
		}
	}
	t.Fatalf("no feature %q", name)
	return 0
}

func TestNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range Names {
		if seen[n] {
			t.Errorf("feature %q listed twice", n)
		}
		seen[n] = true
	}
}

func TestRecordedTokens(t *testing.T) {
	zig := Vector(report(t, "ZiG"))
	if len(zig) != len(Names) {
		t.Fatalf("%d values for %d names", len(zig), len(Names))
	}
	for name, want := range map[string]float64{"chain_base": 1, "dex_v2": 1, "sim_ran": 1, "sim_flagged": 1} {
		if got := get(t, zig, name); got != want {
			t.Errorf("ZiG %s = %v, want %v", name, got, want)
		}
	}

	pepe := Vector(report(t, "PEPE"))
	if got := get(t, pepe, "lp_other"); got < 0.99 {
		t.Errorf("PEPE lp_other = %v, want nearly all its LP held by others", got)
	}
	if got := get(t, pepe, "sim_flagged"); got != 0 {
		t.Errorf("PEPE sim_flagged = %v", got)
	}
	// PEPE's holders came from Blockscout: an explorer answer, so missing to the model.
	if got := get(t, pepe, "holders_log"); !math.IsNaN(got) {
		t.Errorf("PEPE holders_log = %v, want NaN (explorer data is left out)", got)
	}
}

func TestPowersFoundOnlyInSourceAreLeftOut(t *testing.T) {
	r := &check.Report{Facts: &facts.Facts{Powers: []facts.Power{
		{Kind: facts.KindMint, Where: "Token.sol:42", CallableBy: []string{"owner"}, Probed: true},
		{Kind: facts.KindBlacklist, Where: "bytecode", CallableBy: []string{"anyone"}, Probed: true},
		{Kind: facts.KindPause, Where: "bytecode", Probed: false},
	}}}
	v := Vector(r)
	for name, want := range map[string]float64{"power_mint": 0, "power_blacklist": 3, "power_pause": 1, "power_fees": 0} {
		if got := get(t, v, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestAnEmptyReportIsMissingNotZero(t *testing.T) {
	v := Vector(&check.Report{SimError: "fork failed"})
	if got := get(t, v, "sim_ran"); got != 0 {
		t.Errorf("sim_ran = %v", got)
	}
	if got := get(t, v, "share_deployer"); !math.IsNaN(got) {
		t.Errorf("share_deployer = %v without facts, want NaN", got)
	}
}

func TestLimitAboveTheSupplyIsNoLimit(t *testing.T) {
	huge, small := 1.157920892373162e+50, 0.02
	for _, c := range []struct {
		shares []*float64
		want   float64
	}{
		{[]*float64{&huge}, 1},
		{[]*float64{&huge, &small}, small},
	} {
		f := &facts.Facts{}
		for _, share := range c.shares {
			f.Settings = append(f.Settings, facts.Setting{Kind: "limits", SupplyShare: share})
		}
		v := map[string]float64{}
		powers(v, f)
		if v["limit_min_share"] != c.want {
			t.Errorf("limit_min_share = %v, want %v", v["limit_min_share"], c.want)
		}
	}
}
