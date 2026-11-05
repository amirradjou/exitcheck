package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/exitcheck/internal/bot"
	"github.com/amirradjou/exitcheck/internal/cache"
	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/check"
	"github.com/amirradjou/exitcheck/internal/explorer"
	"github.com/amirradjou/exitcheck/internal/telegram"
)

// clients dials every supported chain.
type clients struct {
	byName map[string]*chain.Client
	all    []*chain.Client
}

func dialAll(ctx context.Context) (*clients, error) {
	cs := &clients{byName: map[string]*chain.Client{}}
	for _, net := range chain.Networks() {
		c, err := chain.Dial(ctx, net)
		if err != nil {
			cs.close()
			return nil, err
		}
		cs.byName[net.Name], cs.all = c, append(cs.all, c)
	}
	return cs, nil
}

func (cs *clients) close() {
	for _, c := range cs.all {
		c.Close()
	}
}

func (cs *clients) locate(ctx context.Context, token common.Address) ([]string, error) {
	return check.Locate(ctx, cs.all, token)
}

func (cs *clients) checker(ex *explorer.Client) bot.Checker {
	return func(ctx context.Context, chainName string, token common.Address) (*check.Report, error) {
		c, ok := cs.byName[chainName]
		if !ok {
			return nil, fmt.Errorf("unknown chain %q", chainName)
		}
		return check.Run(ctx, c, ex, token)
	}
}

// openCache connects to REDIS_URL-style url; empty means no cache. A Redis that does not answer yet is only a
// warning: the cache reconnects on its own and answers are computed until it does.
func openCache(ctx context.Context, url string, ttl time.Duration, log *slog.Logger) (*cache.Cache, error) {
	if url == "" {
		return nil, nil
	}
	c, err := cache.New(url, ttl)
	if err != nil {
		return nil, err
	}
	c.OnError = func(err error) { log.Warn("cache", "err", err) }
	if err := c.Ping(ctx); err != nil {
		log.Warn("redis is not answering; running without the cache until it does", "err", err)
	}
	return c, nil
}

func runCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	chainName := fs.String("chain", "", "chain: ethereum or base (default: wherever the address is a contract)")
	asJSON := fs.Bool("json", false, "print the whole report as JSON")
	redisURL := fs.String("cache", os.Getenv("REDIS_URL"), "Redis URL for the answer cache, as the bot uses it (default $REDIS_URL; empty: no cache)")
	ttl := fs.Duration("ttl", 10*time.Minute, "how long a cached answer stays fresh")
	timeout := fs.Duration("timeout", 3*time.Minute, "give up after this long")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: exitcheck check [-chain ethereum|base] [-json] [-cache redis://…] <token address>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(interleave(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError{err.Error()}
	}
	if fs.NArg() != 1 || !common.IsHexAddress(fs.Arg(0)) {
		fs.Usage()
		return usageError{"exitcheck check: need one token address"}
	}
	token := common.HexToAddress(fs.Arg(0))
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
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
	} else if _, err := chain.ByName(name); err != nil {
		return usageError{err.Error()}
	}
	run := cs.checker(explorer.New())
	if *asJSON {
		r, err := run(ctx, name, token)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c, err := openCache(ctx, *redisURL, *ttl, log)
	if err != nil {
		return err
	}
	if c != nil {
		defer c.Close()
	}
	a, hit, err := cache.Fetch(ctx, c, bot.Key(name, token), func(ctx context.Context) (bot.Answer, time.Duration, error) {
		r, err := run(ctx, name, token)
		if err != nil {
			return bot.Answer{}, 0, err
		}
		a := bot.NewAnswer(r)
		return a, a.TTL(), nil
	})
	if err != nil {
		return err
	}
	fmt.Println(a.Text)
	if hit {
		fmt.Fprintf(os.Stderr, "\ncached answer from %s ago; served in %d ms\n", time.Since(a.CheckedAt).Round(time.Second), time.Since(start).Milliseconds())
	}
	return nil
}

func runBot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bot", flag.ContinueOnError)
	redisURL := fs.String("cache", os.Getenv("REDIS_URL"), "Redis URL for the answer cache (default $REDIS_URL; empty: no cache)")
	ttl := fs.Duration("ttl", 10*time.Minute, "how long a cached answer stays fresh")
	concurrency := fs.Int("concurrency", 2, "checks running at once across all chats (each runs an Anvil fork)")
	timeout := fs.Duration("timeout", 60*time.Second, "give up on one check after this long")
	perChat := fs.Int("per-chat", 10, "uncached checks one chat may start per window")
	window := fs.Duration("window", 10*time.Minute, "window for -per-chat")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: exitcheck bot [flags]   (needs TELEGRAM_BOT_TOKEN; REDIS_URL for the cache)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError{err.Error()}
	}
	// Hand-edited .env files leave stray spaces and CRLF carriage returns behind.
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if token == "" {
		return usageError{"exitcheck bot: set TELEGRAM_BOT_TOKEN (from @BotFather)"}
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	tg := telegram.New(token, "")
	me, err := tg.Me(ctx)
	if err != nil {
		return err
	}
	cs, err := dialAll(ctx)
	if err != nil {
		return err
	}
	defer cs.close()
	c, err := openCache(ctx, *redisURL, *ttl, log)
	if err != nil {
		return err
	}
	if c == nil {
		log.Warn("no REDIS_URL: answers are not cached")
	} else {
		defer c.Close()
	}
	cfg := bot.Config{Concurrency: *concurrency, Timeout: *timeout, PerChat: *perChat, Window: *window}
	e := newExplainer()
	if e != nil {
		cfg.Explain = func(ctx context.Context, r *check.Report) string {
			x, err := e.Explain(ctx, r)
			if err != nil {
				log.Warn("explain", "token", r.Address, "err", err)
				return ""
			}
			if x != nil {
				log.Info("explained", "token", r.Address, "kept", len(x.Sentences), "dropped", len(x.Dropped), "ms", x.Elapsed.Milliseconds())
			}
			return x.Render()
		}
	}
	b := bot.New(tg, cs.checker(explorer.New()), cs.locate, c, cfg, log)
	log.Info("bot running", "username", me.Username, "cache", c != nil, "concurrency", *concurrency, "explain", e != nil)
	if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
