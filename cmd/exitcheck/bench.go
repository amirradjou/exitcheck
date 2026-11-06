package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/amirradjou/exitcheck/internal/bot"
	"github.com/amirradjou/exitcheck/internal/cache"
	"github.com/amirradjou/exitcheck/internal/eval"
	"github.com/amirradjou/exitcheck/internal/explorer"
	"github.com/amirradjou/exitcheck/internal/telegram"
	"github.com/amirradjou/exitcheck/internal/verdict"
)

// silentTG stands in for Telegram: bench measures the bot's own work, from a message arriving to the answer
// being handed to Telegram, and keeps the last text each chat was shown.
type silentTG struct {
	mu   sync.Mutex
	last map[int64]string
}

func (s *silentTG) Updates(context.Context, int64, time.Duration) ([]telegram.Update, error) {
	return nil, nil
}

func (s *silentTG) Send(_ context.Context, chat int64, text string, _ int64) (int64, error) {
	s.show(chat, text)
	return 1, nil
}

func (s *silentTG) Edit(_ context.Context, chat, _ int64, text string) error {
	s.show(chat, text)
	return nil
}

func (s *silentTG) Typing(context.Context, int64) error { return nil }

func (s *silentTG) show(chat int64, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last[chat] = text
}

func runBench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	redisURL := fs.String("cache", os.Getenv("REDIS_URL"), "Redis URL (default $REDIS_URL); required, the second pass measures cached answers")
	markdown := fs.Bool("markdown", false, "print a Markdown table of the results")
	pause := fs.Duration("pause", 2*time.Second, "wait between tokens, to stay under free RPC rate limits")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: exitcheck bench [-cache redis://…] [-markdown] <cases.json>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(interleave(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError{err.Error()}
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return usageError{"exitcheck bench: need one cases file"}
	}
	if *redisURL == "" {
		return usageError{"exitcheck bench: set -cache or REDIS_URL (docker compose up -d redis)"}
	}
	cases, err := eval.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	cs, err := dialAll(ctx)
	if err != nil {
		return err
	}
	defer cs.close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := openCache(ctx, *redisURL, time.Hour, log)
	if err != nil {
		return err
	}
	defer c.Close()

	tg := &silentTG{last: map[int64]string{}}
	var got *bot.Served
	b := bot.New(tg, cs.checker(explorer.New()), cs.locate, c,
		bot.Config{Concurrency: 1, Timeout: 3 * time.Minute, PerChat: 1 << 20, Window: time.Minute}, log)
	b.Served = func(s bot.Served) { got = &s }

	type row struct {
		cold, warm time.Duration
		level      verdict.Level
		ok         bool
		failed     string
	}
	rows := make([]row, len(cases))
	for i, tc := range cases {
		if err := cache.Delete(ctx, c, bot.Key(tc.Chain, tc.Address), bot.LocateKey(tc.Address)); err != nil {
			return err
		}
		chat := int64(i + 1)
		msg := &telegram.Message{ID: 1, Chat: telegram.Chat{ID: chat, Type: "private"}, Text: tc.Address.Hex()}
		for pass := range 2 {
			got = nil
			b.Handle(ctx, msg)
			if got == nil {
				rows[i].failed = tg.last[chat]
				break
			}
			if pass == 0 {
				rows[i].cold, rows[i].level = got.Took, got.Level
			} else {
				rows[i].warm = got.Took
			}
		}
		r := &rows[i]
		r.ok = r.failed == "" && (tc.Label == "scam" && r.level >= verdict.HighRisk || tc.Label == "legit" && r.level <= verdict.Caution)
		status := "ok  "
		if !r.ok {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "%s %-6s %-8s %-10s %-9s cold %5.1fs  cached %5.1fms %s\n", status, tc.Label, tc.Chain, tc.Name,
			r.level, r.cold.Seconds(), ms(r.warm), r.failed)
		if i < len(cases)-1 {
			sleep(ctx, *pause)
		}
	}

	var cold, warm []time.Duration
	right := 0
	for _, r := range rows {
		if r.failed == "" {
			cold, warm = append(cold, r.cold), append(warm, r.warm)
		}
		if r.ok {
			right++
		}
	}
	fmt.Fprintf(os.Stderr, "\nverdicts: %d/%d in their label's range (scams at least high-risk, legitimate at most caution)\n", right, len(cases))
	fmt.Fprintf(os.Stderr, "time to answer, cold: median %.1f s, p90 %.1f s, max %.1f s\n", quantile(cold, 0.5).Seconds(), quantile(cold, 0.9).Seconds(), quantile(cold, 1).Seconds())
	fmt.Fprintf(os.Stderr, "time to answer, cached: median %.1f ms, max %.1f ms\n", ms(quantile(warm, 0.5)), ms(quantile(warm, 1)))
	if *markdown {
		fmt.Println("| label | chain | token | verdict | cold | cached |")
		fmt.Println("|---|---|---|---|---|---|")
		for i, tc := range cases {
			r := rows[i]
			fmt.Printf("| %s | %s | %s | %s | %.1f s | %.1f ms |\n", tc.Label, tc.Chain, tc.Name, r.level, r.cold.Seconds(), ms(r.warm))
		}
	}
	if right != len(cases) {
		return fmt.Errorf("%d of %d verdicts outside their label's range", len(cases)-right, len(cases))
	}
	return nil
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// quantile returns the q-quantile (nearest rank) of ds.
func quantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	i := int(q*float64(len(s))+0.5) - 1
	return s[min(max(i, 0), len(s)-1)]
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
