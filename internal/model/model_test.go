package model

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/amirradjou/rugcheck/internal/check"
	"github.com/amirradjou/rugcheck/internal/facts"
	"github.com/amirradjou/rugcheck/internal/features"
	"github.com/amirradjou/rugcheck/internal/verdict"
)

// TestScoresMatchXGBoost holds the Go scorer to XGBoost's own numbers on test launches (testdata/golden.json, written by
// train.py --export): a port that rounds differently or takes the wrong branch on a missing value fails here.
func TestScoresMatchXGBoost(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID       string     `json:"id"`
		Features []*float64 `json:"features"`
		Score    float64    `json:"score"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) < 20 {
		t.Fatalf("only %d golden rows", len(rows))
	}
	missing := 0
	for _, r := range rows {
		x := make([]float64, len(r.Features))
		for i, v := range r.Features {
			if v == nil {
				x[i] = math.NaN()
				missing++
			} else {
				x[i] = *v
			}
		}
		if got := m.Score(x); math.Abs(got-r.Score) > 1e-5 {
			t.Errorf("%s: scored %.6f, XGBoost %.6f", r.ID, got, r.Score)
		}
	}
	if missing == 0 {
		t.Error("no golden row has a missing value: the default branches went untested")
	}
}

func TestBuiltInModelMatchesTheFeaturesCode(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Check(); err != nil {
		t.Error(err)
	}
	if len(m.Features) != len(features.Names) {
		t.Errorf("%d features, code has %d", len(m.Features), len(features.Names))
	}
	if m.Threshold <= 0 || m.Threshold > 1.01 {
		t.Errorf("threshold %v", m.Threshold)
	}
}

func TestParseRefusesWhatItCannotScore(t *testing.T) {
	good, err := os.ReadFile("model.json")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := os.ReadFile("meta.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"not json":        {"{", string(meta)},
		"bad metadata":    {string(good), "{"},
		"other objective": {strings.Replace(string(good), "binary:logistic", "reg:squarederror", 1), string(meta)},
		"fewer features":  {string(good), `{"threshold":0.5,"features":["a"]}`},
	} {
		if _, err := Parse([]byte(c[0]), []byte(c[1])); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// constant is a model that scores everything the same: one leaf.
func constant(margin float32, threshold float64) *Model {
	return &Model{Threshold: threshold, trees: []tree{{left: []int32{-1}, right: []int32{-1}, feature: []int32{0}, cond: []float32{margin}, defaultLeft: []bool{false}}}}
}

func launchReport(level verdict.Level, poolAge time.Duration) *check.Report {
	now := time.Now()
	created := now.Add(-poolAge)
	return &check.Report{
		Facts: &facts.Facts{CheckedAt: now, Liquidity: &facts.Liquidity{CreatedAt: &created}},
		Verdict: verdict.Verdict{Level: level, Headline: "before", Reasons: []verdict.Reason{
			{Level: verdict.Caution, Text: "A note."},
			{Level: verdict.Clear, Text: "A reassurance."},
		}},
	}
}

func TestApplyRaisesOnlyWhatTheRulesLetThrough(t *testing.T) {
	sure, unsure := constant(4, 0.5), constant(-4, 0.5)
	for _, c := range []struct {
		name   string
		m      *Model
		level  verdict.Level
		age    time.Duration
		want   verdict.Level
		scored bool
		raised bool
	}{
		{"sure about a caution launch", sure, verdict.Caution, time.Minute, verdict.HighRisk, true, true},
		{"sure about a clear launch", sure, verdict.Clear, time.Minute, verdict.HighRisk, true, true},
		{"unsure", unsure, verdict.Caution, time.Minute, verdict.Caution, true, false},
		{"already high risk", sure, verdict.HighRisk, time.Minute, verdict.HighRisk, true, false},
		{"never lowers a scam", unsure, verdict.Scam, time.Minute, verdict.Scam, true, false},
		{"an old token is not a launch", sure, verdict.Caution, 72 * time.Hour, verdict.Caution, false, false},
	} {
		r := launchReport(c.level, c.age)
		c.m.Apply(r)
		if r.Verdict.Level != c.want {
			t.Errorf("%s: level %s, want %s", c.name, r.Verdict.Level, c.want)
		}
		if (r.Model != nil) != c.scored {
			t.Errorf("%s: scored = %v, want %v", c.name, r.Model != nil, c.scored)
		}
		if r.Model != nil && (r.Model.Raised != c.raised || r.Model.RulesLevel != c.level) {
			t.Errorf("%s: %+v", c.name, r.Model)
		}
		if c.raised {
			if r.Verdict.Reasons[0].Level != verdict.HighRisk || !strings.Contains(r.Verdict.Reasons[0].Text, "alert line of 0.50") {
				t.Errorf("%s: reasons %+v", c.name, r.Verdict.Reasons)
			}
			if r.Verdict.Headline == "before" {
				t.Errorf("%s: the headline still says what the rules said", c.name)
			}
		}
	}
}

func TestApplyLeavesAReportWithoutPoolsAlone(t *testing.T) {
	r := &check.Report{Facts: &facts.Facts{}, Verdict: verdict.Verdict{Level: verdict.Caution}}
	constant(4, 0.5).Apply(r)
	if r.Model != nil || r.Verdict.Level != verdict.Caution {
		t.Errorf("%+v", r)
	}
}
