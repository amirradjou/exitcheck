package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amirradjou/rugcheck/internal/analyze"
	"github.com/amirradjou/rugcheck/internal/backfill"
	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/explorer"
	"github.com/amirradjou/rugcheck/internal/store"
	"github.com/amirradjou/rugcheck/internal/stream"
	"github.com/amirradjou/rugcheck/internal/telegram"
	"github.com/amirradjou/rugcheck/internal/watch"
)

// Defaults match compose.yaml's ports on the host; inside compose, the services get kafka:9092 and postgres:5432.
const (
	defaultBrokers  = "localhost:9094"
	defaultDatabase = "postgres://rugcheck:rugcheck@localhost:5434/rugcheck?sslmode=disable"
	partitions      = 6
)

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return usageError{err.Error()}
	}
	return nil
}

func runWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	chains := fs.String("chains", "ethereum,base", "comma-separated chains to watch")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: rugcheck watch [-chains ethereum,base]   (KAFKA_BROKERS, DATABASE_URL)")
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("svc", "watch")
	brokers := stream.Brokers(env("KAFKA_BROKERS", defaultBrokers))
	if err := stream.EnsureTopic(ctx, brokers, stream.Topic, partitions); err != nil {
		return err
	}
	pub, err := stream.NewProducer(brokers, stream.Topic)
	if err != nil {
		return err
	}
	defer pub.Close()
	st, err := store.Open(ctx, env("DATABASE_URL", defaultDatabase))
	if err != nil {
		return err
	}
	defer st.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for name := range strings.SplitSeq(*chains, ",") {
		net, err := chain.ByName(strings.TrimSpace(name))
		if err != nil {
			return usageError{err.Error()}
		}
		c, err := chain.Dial(ctx, net)
		if err != nil {
			return err
		}
		defer c.Close()
		w := watch.New(net.Name, watch.NewRPC(c), st, pub, log)
		wg.Go(func() {
			if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				errs <- fmt.Errorf("%s: %w", net.Name, err)
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case err := <-errs:
		return err
	case <-done:
		return nil
	}
}

func runAnalyze(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	concurrency := fs.Int("concurrency", 1, "checks running at once in this worker (each runs an Anvil fork)")
	ttl := fs.Duration("ttl", 10*time.Minute, "how long a cached answer stays fresh")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: rugcheck analyze [-concurrency N]   (KAFKA_BROKERS, DATABASE_URL, REDIS_URL;")
		fmt.Fprintln(fs.Output(), "       TELEGRAM_BOT_TOKEN and TELEGRAM_ALERTS_CHAT to post alerts)")
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("svc", "analyze")
	st, err := store.Open(ctx, env("DATABASE_URL", defaultDatabase))
	if err != nil {
		return err
	}
	defer st.Close()
	cs, err := dialAll(ctx)
	if err != nil {
		return err
	}
	defer cs.close()
	c, err := openCache(ctx, os.Getenv("REDIS_URL"), *ttl, log)
	if err != nil {
		return err
	}
	if c != nil {
		defer c.Close()
	}
	var alert analyze.Alerter
	token, chat := env("TELEGRAM_BOT_TOKEN", ""), env("TELEGRAM_ALERTS_CHAT", "")
	if token != "" && chat != "" {
		tg := telegram.New(token, "")
		alert = func(ctx context.Context, text string) error { return tg.Post(ctx, chat, text) }
	} else {
		log.Warn("TELEGRAM_ALERTS_CHAT not set: alerts are stored and logged, not posted")
	}
	a := analyze.New(st, cs.checker(explorer.New()), c, alert, *concurrency, log)

	brokers := stream.Brokers(env("KAFKA_BROKERS", defaultBrokers))
	if err := stream.EnsureTopic(ctx, brokers, stream.Topic, partitions); err != nil {
		return err
	}
	cons, err := stream.NewConsumer(brokers, stream.Topic, stream.Group, log)
	if err != nil {
		return err
	}
	defer cons.Close()
	log.Info("analyzer running", "concurrency", *concurrency, "cache", c != nil, "alerts", alert != nil)
	if err := cons.Run(ctx, a.Handle); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func runReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	since := fs.Duration("since", 24*time.Hour, "window to report on, ending now")
	markdown := fs.Bool("markdown", false, "print a Markdown table")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: rugcheck report [-since 24h] [-markdown]   (DATABASE_URL)")
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
	r, err := st.Report(ctx, time.Now().Add(-*since))
	if err != nil {
		return err
	}
	rows := [][2]string{
		{"window", fmt.Sprintf("%s to %s UTC (%s)", r.Since.UTC().Format("2006-01-02 15:04"), r.Until.UTC().Format("2006-01-02 15:04"), since.String())},
		{"launches", fmt.Sprintf("%d (%s)", r.Launches, counts(r.Chains))},
		{"analyzed / failed / waiting", fmt.Sprintf("%d / %d / %d", r.Analyzed, r.Failed, r.Waiting)},
		{"lag, liquidity → verdict", fmt.Sprintf("median %.1f s, p95 %.1f s, max %.1f s; %d over a minute", r.LagP50, r.LagP95, r.LagMax, r.OverMinute)},
		{"of which detection", fmt.Sprintf("median %.1f s, p95 %.1f s", r.DetectP50, r.DetectP95)},
		{"check", fmt.Sprintf("median %.1f s", r.CheckP50)},
		{"redelivered by Kafka", fmt.Sprintf("%d launches, %d extra deliveries (counted, not re-analyzed)", r.Redelivered, r.Extra)},
		{"duplicate results / alerts", fmt.Sprintf("%d / %d", r.DuplicateResults, r.DuplicateAlerts)},
		{"verdicts", counts(r.Levels)},
		{"alerts", fmt.Sprintf("%d (%d posted)", r.Alerts, r.AlertsSent)},
	}
	if *markdown {
		fmt.Println("| | |\n|---|---|")
		for _, row := range rows {
			fmt.Printf("| %s | %s |\n", row[0], row[1])
		}
		return nil
	}
	for _, row := range rows {
		fmt.Printf("%-28s %s\n", row[0], row[1])
	}
	return nil
}

// counts formats a map as "a 3, b 1" in descending order.
func counts(m map[string]int) string {
	type kv struct {
		k string
		v int
	}
	var s []kv
	for k, v := range m {
		s = append(s, kv{k, v})
	}
	slices.SortFunc(s, func(a, b kv) int {
		if a.v != b.v {
			return b.v - a.v
		}
		return strings.Compare(a.k, b.k)
	})
	parts := make([]string, len(s))
	for i, e := range s {
		parts[i] = fmt.Sprintf("%s %d", e.k, e.v)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// runAudit counts the launches the live watch missed: every pool created against a quote token in the window that
// received liquidity, from the factories' logs, set against the launches the watch recorded.
func runAudit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	since := fs.Duration("since", 24*time.Hour, "window to audit, ending at -until")
	until := fs.String("until", "", "end of the window, RFC 3339 (default: now)")
	chains := fs.String("chains", "ethereum,base", "comma-separated chains")
	markdown := fs.Bool("markdown", false, "print a Markdown table")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: rugcheck audit [-since 24h] [-until 2026-11-01T17:13:00Z] [-markdown]   (DATABASE_URL)")
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	end := time.Now()
	if *until != "" {
		var err error
		if end, err = time.Parse(time.RFC3339, *until); err != nil {
			return usageError{"-until: " + err.Error()}
		}
	}
	start := end.Add(-*since)
	st, err := store.Open(ctx, env("DATABASE_URL", defaultDatabase))
	if err != nil {
		return err
	}
	defer st.Close()

	type row struct {
		chain, dex           string
		launches, seen, miss int
	}
	var rows []row
	var missed []string
	for name := range strings.SplitSeq(*chains, ",") {
		net, err := chain.ByName(strings.TrimSpace(name))
		if err != nil {
			return usageError{err.Error()}
		}
		c, err := chain.Dial(ctx, net)
		if err != nil {
			return err
		}
		from, to, err := auditRange(ctx, c, start, end)
		if err != nil {
			c.Close()
			return fmt.Errorf("%s: %w", net.Name, err)
		}
		funded, err := backfill.FundedPools(ctx, c, from, to)
		c.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", net.Name, err)
		}
		seen, err := st.LaunchPools(ctx, net.Name)
		if err != nil {
			return err
		}
		byDEX := map[string]*row{}
		for _, f := range funded {
			r := byDEX[f.DEX]
			if r == nil {
				r = &row{chain: net.Name, dex: f.DEX}
				byDEX[f.DEX] = r
			}
			r.launches++
			if seen[f.Pool.Pool] {
				r.seen++
			} else {
				r.miss++
			}
		}
		for _, f := range backfill.Missed(funded, seen) {
			missed = append(missed, fmt.Sprintf("%s:%s (%s, created in block %d, first liquidity in block %d)",
				net.Name, strings.ToLower(f.Pool.Pool.Hex()), f.DEX, f.Block, f.FirstMint))
		}
		dexes := slices.Sorted(maps.Keys(byDEX))
		for _, d := range dexes {
			rows = append(rows, *byDEX[d])
		}
		fmt.Fprintf(os.Stderr, "%s: blocks %d to %d\n", net.Name, from, to)
	}
	window := fmt.Sprintf("%s to %s UTC", start.UTC().Format("2006-01-02 15:04"), end.UTC().Format("2006-01-02 15:04"))
	if *markdown {
		fmt.Printf("Launches %s: pools created against a quote token that received liquidity, from the factories' logs.\n\n", window)
		fmt.Println("| chain | venue | launches | seen by the watch | missed |\n|---|---|---|---|---|")
		for _, r := range rows {
			fmt.Printf("| %s | %s | %d | %d | %d |\n", r.chain, r.dex, r.launches, r.seen, r.miss)
		}
	} else {
		fmt.Printf("launches %s (from the factories' logs)\n", window)
		for _, r := range rows {
			fmt.Printf("%-9s %-11s %4d launches, %4d seen by the watch, %3d missed\n", r.chain, r.dex, r.launches, r.seen, r.miss)
		}
	}
	for _, m := range missed {
		fmt.Println("missed:", m)
	}
	return nil
}

// auditRange turns a window into blocks, ending far enough behind the head for the archive RPC to have every block:
// an archive RPC can answer "no logs" for a block it has not reached.
func auditRange(ctx context.Context, c *chain.Client, start, end time.Time) (uint64, uint64, error) {
	from, ok, err := c.BlockAt(ctx, start, 0)
	if err != nil || !ok {
		return 0, 0, fmt.Errorf("block at %s: %v", start, err)
	}
	safe, err := c.SafeHead(ctx)
	if err != nil {
		return 0, 0, err
	}
	to, ok, err := c.BlockAt(ctx, end, from)
	if err != nil {
		return 0, 0, err
	}
	const margin = 100
	if !ok || to+margin > safe {
		return 0, 0, fmt.Errorf("the window ends too close to the head (block %d); audit it a little later", safe)
	}
	return from + 1, to, nil
}
