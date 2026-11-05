// Package model serves the launch classifier that ml/train.py trains (M4), without Python: it reads XGBoost's JSON model
// and walks the trees. The rules decide first. The model only speaks about a launch they let through, and only upward:
// it can raise a verdict, never lower one, and a scam verdict stays the rules' alone.
package model

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

//go:embed model.json
var modelJSON []byte

//go:embed meta.json
var metaJSON []byte

// Load returns the model built into the binary.
var Load = sync.OnceValues(func() (*Model, error) { return Parse(modelJSON, metaJSON) })

// Model is a trained classifier and the threshold it alerts from.
type Model struct {
	// Threshold is the score from which a launch the rules let through is raised. train.py sets it on launches the model
	// did not train on, at the rules' own precision.
	Threshold float64
	// Features are the names the trees' indices refer to, in order.
	Features []string
	trees    []tree
	base     float64 // the starting margin, before any tree
}

// tree is one regression tree, as parallel arrays by node. A leaf has left == -1 and keeps its value in cond.
type tree struct {
	left, right, feature []int32
	cond                 []float32
	defaultLeft          []bool
}

// Score returns the probability that a launch's liquidity is pulled within a day. x is in Features order; NaN marks a
// value the check could not read, which goes the way training sent missing values.
func (m *Model) Score(x []float64) float64 {
	margin := m.base
	for i := range m.trees {
		margin += float64(m.trees[i].leaf(x))
	}
	return 1 / (1 + math.Exp(-margin))
}

func (t *tree) leaf(x []float64) float32 {
	n := int32(0)
	for t.left[n] != -1 {
		v := x[t.feature[n]]
		switch {
		case math.IsNaN(v):
			if t.defaultLeft[n] {
				n = t.left[n]
			} else {
				n = t.right[n]
			}
		case float32(v) < t.cond[n]: // XGBoost compares as float32: as a float64 a value can land on the other side
			n = t.left[n]
		default:
			n = t.right[n]
		}
	}
	return t.cond[n]
}

// Parse reads XGBoost's JSON model and the metadata train.py writes beside it.
func Parse(modelData, metaData []byte) (*Model, error) {
	var meta struct {
		Threshold float64  `json:"threshold"`
		Features  []string `json:"features"`
	}
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return nil, fmt.Errorf("model metadata: %w", err)
	}
	var raw struct {
		Learner struct {
			Attributes struct {
				BestIteration string `json:"best_iteration"`
			} `json:"attributes"`
			FeatureNames []string `json:"feature_names"`
			Booster      struct {
				Name  string `json:"name"`
				Model struct {
					Trees []struct {
						Left        []int32   `json:"left_children"`
						Right       []int32   `json:"right_children"`
						Feature     []int32   `json:"split_indices"`
						Cond        []float32 `json:"split_conditions"`
						DefaultLeft []int     `json:"default_left"`
					} `json:"trees"`
				} `json:"model"`
			} `json:"gradient_booster"`
			Params struct {
				BaseScore string `json:"base_score"`
			} `json:"learner_model_param"`
			Objective struct {
				Name string `json:"name"`
			} `json:"objective"`
		} `json:"learner"`
	}
	if err := json.Unmarshal(modelData, &raw); err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	l := raw.Learner
	if l.Booster.Name != "gbtree" || l.Objective.Name != "binary:logistic" {
		return nil, fmt.Errorf("model: %s with %s is not the model this code scores", l.Booster.Name, l.Objective.Name)
	}
	if len(l.FeatureNames) != len(meta.Features) {
		return nil, fmt.Errorf("model has %d features, its metadata %d", len(l.FeatureNames), len(meta.Features))
	}
	// base_score is written as "[7.619048E-1]" by recent versions and "7.619048E-1" by older ones.
	base, err := strconv.ParseFloat(strings.Trim(l.Params.BaseScore, "[]"), 64)
	if err != nil || base <= 0 || base >= 1 {
		return nil, fmt.Errorf("model: base score %q", l.Params.BaseScore)
	}
	// Early stopping keeps the trees after the best one in the file; XGBoost itself stops at the best when it predicts.
	n := len(l.Booster.Model.Trees)
	if s := l.Attributes.BestIteration; s != "" {
		best, err := strconv.Atoi(s)
		if err != nil || best < 0 || best >= n {
			return nil, fmt.Errorf("model: best iteration %q of %d trees", s, n)
		}
		n = best + 1
	}
	m := &Model{Threshold: meta.Threshold, Features: meta.Features, base: math.Log(base / (1 - base))}
	for _, t := range l.Booster.Model.Trees[:n] {
		dl := make([]bool, len(t.DefaultLeft))
		for i, d := range t.DefaultLeft {
			dl[i] = d != 0
		}
		m.trees = append(m.trees, tree{left: t.Left, right: t.Right, feature: t.Feature, cond: t.Cond, defaultLeft: dl})
	}
	return m, nil
}
