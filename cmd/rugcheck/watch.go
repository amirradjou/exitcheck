package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amirradjou/rugcheck/internal/analyze"
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
