package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/check"
	"github.com/amirradjou/rugcheck/internal/eval"
	"github.com/amirradjou/rugcheck/internal/explain"
	"github.com/amirradjou/rugcheck/internal/explorer"
	"github.com/amirradjou/rugcheck/internal/facts"
	"github.com/amirradjou/rugcheck/internal/sim"
	"github.com/amirradjou/rugcheck/internal/verdict"
)

// newExplainer returns the explainer the environment configures, or nil when no model is.
func newExplainer() *explain.Explainer {
	llm := explain.FromEnv()
	if llm == nil {
		return nil
	}
	ex := explorer.New()
	return &explain.Explainer{LLM: llm, Source: func(ctx context.Context, name string, addr common.Address) (*explorer.Source, error) {
		net, err := chain.ByName(name)
		if err != nil {
			return nil, err
		}
		return ex.Source(ctx, net, addr)
	}}
}

// runExplain checks one token and explains the verdict, or runs the review set.
func runExplain(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	chainName := fs.String("chain", "", "chain: ethereum or base (default: wherever the address is a contract)")
	review := fs.Bool("review", false, "explain the review set instead of one token, and report what survived")
	out := fs.String("out", "", "with -review: write every sentence and what it cites to this Markdown file, for reading")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: rugcheck explain [-chain ethereum|base] <token address>   (RUGCHECK_LLM_URL, RUGCHECK_LLM_MODEL)")
		fmt.Fprintln(fs.Output(), "       rugcheck explain -review [-out review.md]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(interleave(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError{err.Error()}
	}
	e := newExplainer()
	if e == nil {
		return usageError{"rugcheck explain: set RUGCHECK_LLM_URL and RUGCHECK_LLM_MODEL (see .env.example)"}
	}
	if *review {
		return runReview(ctx, e, *out)
	}
	if fs.NArg() != 1 || !common.IsHexAddress(fs.Arg(0)) {
		fs.Usage()
		return usageError{"rugcheck explain: need one token address"}
	}
	token := common.HexToAddress(fs.Arg(0))
	cs, err := dialAll(ctx)
	if err != nil {
		return err
	}
	defer cs.close()
	name := *chainName
	if name == "" {
		chains, err := cs.locate(ctx, token)
		if err != nil {
			return err
		}
		if len(chains) == 0 {
			return fmt.Errorf("no contract at %s on Ethereum or Base", token.Hex())
		}
		name = chains[0]
	}
	r, err := cs.checker(explorer.New())(ctx, name, token)
	if err != nil {
		return err
	}
	fmt.Println(check.Render(r))
	x, err := e.Explain(ctx, r)
	if err != nil {
		return err
	}
	if why := x.Render(); why != "" {
		fmt.Printf("\n%s\n", why)
	}
	return nil
}

// reviewItem is one report of the review set.
type reviewItem struct {
	name, label string
	report      *check.Report
}

// reviewSet is the 20 labelled tokens, judged again from their recorded facts and simulations, and the launches
// recorded in internal/explain/testdata: what the live watch actually sees.
func reviewSet() ([]reviewItem, error) {
	cases, err := eval.Load("testdata/tokens.json")
	if err != nil {
		return nil, err
	}
	var items []reviewItem
	for _, c := range cases {
		var f facts.Facts
		var s sim.Result
		if err := readJSON(filepath.Join("internal/verdict/testdata", c.Name+".facts.json"), &f); err != nil {
			return nil, err
		}
		if err := readJSON(filepath.Join("internal/verdict/testdata", c.Name+".sim.json"), &s); err != nil {
			return nil, err
		}
		items = append(items, reviewItem{c.Name, c.Label, &check.Report{
			Chain: f.Chain, Address: f.Address, Name: f.Token.Name, Symbol: f.Token.Symbol, Block: f.Block,
			CheckedAt: f.CheckedAt, Facts: &f, Sim: &s, Verdict: verdict.Judge(&f, &s, nil, f.CheckedAt),
		}})
	}
	files, err := filepath.Glob("internal/explain/testdata/*.report.json")
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	for _, file := range files {
		var r check.Report
		if err := readJSON(file, &r); err != nil {
			return nil, err
		}
		items = append(items, reviewItem{strings.TrimSuffix(filepath.Base(file), ".report.json"), "launch", &r})
	}
	return items, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// runReview explains the review set one report at a time and counts what the grounding step did. The model's own
// compliance is the number to read: every sentence kept cites a check or a source line by construction.
func runReview(ctx context.Context, e *explain.Explainer, out string) error {
	items, err := reviewSet()
	if err != nil {
		return err
	}
	var doc strings.Builder
	var explained, empty, clear, failed, kept, sourced int
	whys := map[string]int{}
	var took []time.Duration
	for _, it := range items {
		x, err := e.Explain(ctx, it.report)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			failed++
			fmt.Fprintf(os.Stderr, "%s: %v\n", it.name, err)
			continue
		case x == nil:
			clear++
			continue
		}
		explained++
		took = append(took, x.Elapsed)
		if len(x.Sentences) == 0 {
			empty++
		}
		fmt.Fprintf(&doc, "## %s (%s, %s): %s\n\n", it.name, it.label, it.report.Verdict.Level, it.report.Verdict.Headline)
		byID := map[string]explain.Evidence{}
		for _, ev := range x.Evidence {
			byID[ev.ID] = ev
		}
		for _, s := range x.Sentences {
			kept++
			fmt.Fprintf(&doc, "- %s\n", s.Text)
			hasSource := false
			for _, id := range s.Cites {
				ev := byID[id]
				hasSource = hasSource || ev.Kind == explain.KindSource
				fmt.Fprintf(&doc, "  - `%s` %s: %s\n", id, ev.Ref, strings.Join(strings.Fields(ev.Text), " "))
			}
			if hasSource {
				sourced++
			}
		}
		for _, d := range x.Dropped {
			whys[d.Why]++
			fmt.Fprintf(&doc, "- ~~%s~~ (%s)\n", strings.Join(strings.Fields(d.Text), " "), d.Why)
		}
		doc.WriteByte('\n')
		fmt.Printf("%-22s %-9s kept %d, dropped %d, %.1f s\n", it.name, it.report.Verdict.Level, len(x.Sentences), len(x.Dropped), x.Elapsed.Seconds())
	}
	dropped := 0
	for _, n := range whys {
		dropped += n
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	fmt.Printf("\n%d reports: %d explained, %d clear (not put to the model), %d failed, %d with no sentence left\n",
		len(items), explained, clear, failed, empty)
	fmt.Printf("%d sentences written: %d kept (%d citing source lines), %d dropped\n", kept+dropped, kept, sourced, dropped)
	for why, n := range whys {
		fmt.Printf("  %3d %s\n", n, why)
	}
	if len(took) > 0 {
		fmt.Printf("median %.1f s per explanation\n", took[len(took)/2].Seconds())
	}
	if out != "" {
		return os.WriteFile(out, []byte(doc.String()), 0o644)
	}
	return nil
}
