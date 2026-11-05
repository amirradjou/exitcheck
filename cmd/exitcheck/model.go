package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amirradjou/exitcheck/internal/backfill"
	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/check"
	"github.com/amirradjou/exitcheck/internal/features"
	"github.com/amirradjou/exitcheck/internal/outcome"
	"github.com/amirradjou/exitcheck/internal/store"
)

// runLabel reads the outcome of every launch whose horizon has ended and stores it.
func runLabel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("label", flag.ContinueOnError)
	horizon := fs.Duration("horizon", outcome.Horizon, "how long after its first liquidity a launch is watched for a pull")
	limit := fs.Int("limit", 0, "label at most this many launches per chain (0: all)")
	checked := fs.Bool("checked", false, "only launches with a report, the ones the dataset can use")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: exitcheck label [-horizon 24h] [-checked] [-limit N]   (DATABASE_URL)")
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	st, err := store.Open(ctx, env("DATABASE_URL", defaultDatabase))
	if err != nil {
		return err
	}
	defer st.Close()
	for _, net := range chain.Networks() {
		launches, err := st.Unlabelled(ctx, net.Name, time.Now().Add(-*horizon), *checked, *limit)
		if err != nil {
			return err
		}
		if len(launches) == 0 {
			continue
		}
		c, err := chain.Dial(ctx, net)
		if err != nil {
			return err
		}
		lb := outcome.NewLabeller(c)
		var done, pulled, failed int
		for _, l := range launches {
			o, end, ok, err := lb.Label(ctx, l, *horizon)
			if ctx.Err() != nil {
				c.Close()
				return ctx.Err()
			}
			if err != nil {
				failed++
				log.Warn("labelling", "id", l.ID, "err", err)
				continue
			}
			if !ok {
				continue // the chain's head has not reached the horizon yet
			}
			if err := st.SaveOutcome(ctx, l.ID, *horizon, end, o); err != nil {
				c.Close()
				return err
			}
			done++
			if o.Pulled {
				pulled++
			}
		}
		c.Close()
		fmt.Printf("%-9s labelled %d of %d (%d pulled), %d failed\n", net.Name, done, len(launches), pulled, failed)
	}
	return nil
}

// runBackfill queues past launches and checks each one as it stood at its block, on a fork.
func runBackfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	watch := fs.Bool("watch", false, "queue the live watch's launches analyzed before their report was kept")
	from := fs.String("from", "", "scan the factories' logs for launches from this time (2006-01-02 or RFC 3339)")
	to := fs.String("to", "", "end of the scan (default: now)")
	chains := fs.String("chains", "ethereum,base", "comma-separated chains")
	limit := fs.Int("limit", 0, "check at most this many queued launches per chain (0: all)")
	factsBudget := fs.Duration("facts-budget", 2*time.Minute, "facts budget per check (live checks get 20 s)")
	simBudget := fs.Duration("sim-budget", 3*time.Minute, "simulation budget per check (live checks get 30 s)")
	forkEthereum := fs.String("fork-ethereum", "https://mainnet.gateway.tenderly.co", "archive RPC Ethereum forks are made from")
	forkBase := fs.String("fork-base", "", "archive RPC Base forks are made from (default: Base's archive RPC)")
	cups := fs.Int("compute-units", 136, "anvil's cap on requests to the archive RPC, in compute units per second")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: exitcheck backfill [-watch] [-from 2026-10-15 [-to …]] [-limit N]   (DATABASE_URL)")
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	st, err := store.Open(ctx, env("DATABASE_URL", defaultDatabase))
	if err != nil {
		return err
	}
	defer st.Close()
	if *watch {
		n, err := st.BackfillFromWatch(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("queued %d of the live watch's launches\n", n)
	}
	var nets []chain.Network
	for name := range strings.SplitSeq(*chains, ",") {
		net, err := chain.ByName(strings.TrimSpace(name))
		if err != nil {
			return usageError{err.Error()}
		}
		nets = append(nets, net)
	}
	if *from != "" {
		start, err := parseTime(*from)
		if err != nil {
			return usageError{"-from: " + err.Error()}
		}
		end := time.Now()
		if *to != "" {
			if end, err = parseTime(*to); err != nil {
				return usageError{"-to: " + err.Error()}
			}
		}
		for _, net := range nets {
			n, err := scan(ctx, st, net, start, end)
			if err != nil {
				return fmt.Errorf("%s: %w", net.Name, err)
			}
			fmt.Printf("%-9s queued %d launches funded %s to %s\n", net.Name, n, start.Format(time.DateTime), end.Format(time.DateTime))
		}
	}

	check.FactsBudget, check.SimBudget = *factsBudget, *simBudget
	upstream := map[string]string{"ethereum": *forkEthereum, "base": *forkBase}
	// Chains run side by side (each forks its own archive RPC); launches within a chain run one at a time, the
	// archive RPC's pace being the limit.
	var wg sync.WaitGroup
	for _, net := range nets {
		up := upstream[net.Name]
		if up == "" {
			_, up, _ = net.Endpoints()
		}
		wg.Go(func() {
			pending, err := st.PendingBackfill(ctx, net.Name, *limit)
			if err != nil {
				log.Error("listing queued launches", "chain", net.Name, "err", err)
				return
			}
			var done, failed int
			for _, b := range pending {
				r, err := backfill.CheckAt(ctx, net, up, *cups, b.Token, b.CheckBlock)
				if ctx.Err() != nil {
					return
				}
				var level, msg string
				var report []byte
				if err != nil {
					failed++
					msg = err.Error()
					log.Warn("backfill check", "id", b.ID(), "block", b.CheckBlock, "err", err)
				} else {
					done++
					level = r.Verdict.Level.String()
					if report, err = json.Marshal(r); err != nil {
						log.Error("encoding a backfilled report", "id", b.ID(), "err", err)
						return
					}
				}
				if err := st.FinishBackfill(ctx, b.ID(), level, report, msg); err != nil {
					log.Error("storing a backfilled check", "id", b.ID(), "err", err)
					return
				}
			}
			fmt.Printf("%-9s checked %d of %d queued launches, %d failed\n", net.Name, done, len(pending), failed)
		})
	}
	wg.Wait()
	return ctx.Err()
}

// scan queues the launches funded between start and end on one chain.
func scan(ctx context.Context, st *store.Store, net chain.Network, start, end time.Time) (int, error) {
	c, err := chain.Dial(ctx, net)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	from, ok, err := c.BlockAt(ctx, start, 0)
	if err != nil || !ok {
		return 0, fmt.Errorf("block at %s: %v", start, err)
	}
	to, ok, err := c.BlockAt(ctx, end, from)
	if err != nil {
		return 0, err
	}
	if !ok {
		if to, err = c.SafeHead(ctx); err != nil {
			return 0, err
		}
	}
	found, err := backfill.Scan(ctx, c, from+1, to)
	if err != nil {
		return 0, err
	}
	return st.AddBackfill(ctx, found)
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// runDataset writes every labelled launch as one CSV row: its identity and time, the rules' verdict, the outcome,
// and the model's features (empty where the check could not read a value).
func runDataset(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dataset", flag.ContinueOnError)
	out := fs.String("out", "ml/data/dataset.csv", "CSV file to write")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: exitcheck dataset [-out ml/data/dataset.csv]   (DATABASE_URL)")
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	st, err := store.Open(ctx, env("DATABASE_URL", defaultDatabase))
	if err != nil {
		return err
	}
	defer st.Close()
	samples, err := st.Samples(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	header := []string{"id", "chain", "dex", "funded_at", "source", "rules_level", "live_level",
		"pulled", "bait", "buys", "sells", "minutes_to_pull"}
	if err := w.Write(append(header, features.Names...)); err != nil {
		return err
	}
	for _, s := range samples {
		var r check.Report
		if err := json.Unmarshal(s.Report, &r); err != nil {
			return fmt.Errorf("%s: report: %w", s.ID, err)
		}
		o := s.Outcome
		minutes := ""
		if o.Pulled && !o.PulledAt.IsZero() {
			minutes = strconv.FormatFloat(o.PulledAt.Sub(s.FundedAt).Minutes(), 'f', 2, 64)
		}
		row := []string{s.ID, s.Chain, s.DEX, strconv.FormatInt(s.FundedAt.Unix(), 10), s.Source, s.Level, s.LiveLevel,
			strconv.FormatBool(o.Pulled), strconv.FormatBool(o.Bait), strconv.Itoa(o.Buys), strconv.Itoa(o.Sells), minutes}
		for _, x := range features.Vector(&r) {
			if math.IsNaN(x) {
				row = append(row, "")
			} else {
				row = append(row, strconv.FormatFloat(x, 'g', -1, 64))
			}
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	fmt.Printf("wrote %d labelled launches to %s\n", len(samples), *out)
	return f.Close()
}
