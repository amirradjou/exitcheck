package model

import (
	"fmt"
	"slices"
	"time"

	"github.com/amirradjou/rugcheck/internal/check"
	"github.com/amirradjou/rugcheck/internal/features"
	"github.com/amirradjou/rugcheck/internal/verdict"
)

// maxPoolAge is how old a main pool may be for the model to judge it. It learned from launches checked around their
// first liquidity and knows nothing about a token with a year of history.
const maxPoolAge = 24 * time.Hour

// Check says whether the model and the features code agree on what each number means. They are built separately, and
// a mismatch would score every launch on the wrong columns without any error.
func (m *Model) Check() error {
	if !slices.Equal(m.Features, features.Names) {
		return fmt.Errorf("the model was trained on other features than this code computes: retrain it")
	}
	return nil
}

// Apply scores a launch's report and raises its verdict if the model is sure enough and the rules were not already
// alarmed. It leaves a report the model cannot judge untouched.
func (m *Model) Apply(r *check.Report) {
	if !isLaunch(r) {
		return
	}
	score := m.Score(features.Vector(r))
	r.Model = &check.ModelScore{Score: score, Threshold: m.Threshold, RulesLevel: r.Verdict.Level}
	if score < m.Threshold || r.Verdict.Level >= verdict.HighRisk {
		return
	}
	r.Model.Raised = true
	r.Verdict.Raise(verdict.HighRisk, fmt.Sprintf(
		"The fixed rules found no trap, but launches like this one were usually emptied within a day: a model trained on past launches scores it %.2f, above its alert line of %.2f.",
		score, m.Threshold))
}

func isLaunch(r *check.Report) bool {
	if r.Facts == nil || r.Facts.Liquidity == nil || r.Facts.Liquidity.CreatedAt == nil {
		return false
	}
	return r.Facts.AsOf().Sub(*r.Facts.Liquidity.CreatedAt) <= maxPoolAge
}
