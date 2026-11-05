// Package bot answers token addresses sent to a Telegram bot with a verdict and its reasons. Answers are cached in
// Redis, concurrent requests for one token share a single check, and checks are rationed per chat and globally:
// each one starts an Anvil fork and makes dozens of calls to free RPCs.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/cache"
	"github.com/amirradjou/rugcheck/internal/check"
	"github.com/amirradjou/rugcheck/internal/telegram"
	"github.com/amirradjou/rugcheck/internal/verdict"
)

// Messenger is the part of the Telegram client the bot uses.
type Messenger interface {
	Updates(ctx context.Context, offset int64, wait time.Duration) ([]telegram.Update, error)
	Send(ctx context.Context, chat int64, text string, replyTo int64) (int64, error)
	Edit(ctx context.Context, chat, message int64, text string) error
	Typing(ctx context.Context, chat int64) error
}

// Checker checks one token on one chain.
type Checker func(ctx context.Context, chain string, token common.Address) (*check.Report, error)

// Locator lists the chains where an address is a contract.
type Locator func(ctx context.Context, token common.Address) ([]string, error)

// Answer is what the bot sends for a token, and what the cache keeps.
type Answer struct {
	Text  string        `json:"text"`
	Level verdict.Level `json:"level"`
	// Complete is false when a decisive check did not finish; such answers are kept for PartialTTL only.
	Complete  bool          `json:"complete"`
	Chain     string        `json:"chain"`
	CheckedAt time.Time     `json:"checked_at"`
	Elapsed   time.Duration `json:"elapsed_ns"`
}

// PartialTTL is how long an answer with unfinished decisive checks is cached: long enough to absorb a burst of
// requests while the RPCs throttle, short enough that a retry soon gets a full check (a honeypot whose simulation
// timed out must not read "caution" for the whole cache lifetime).
const PartialTTL = time.Minute

// Config tunes the bot.
type Config struct {
	// Concurrency is how many checks run at once across all chats (each runs an Anvil fork).
	Concurrency int
	// Timeout bounds one check.
	Timeout time.Duration
	// PerChat is how many uncached checks a chat may start per Window.
	PerChat int
	Window  time.Duration
	// Explain writes the plain-words "why" of a report, or "" when it has none; nil runs without a model.
	Explain func(ctx context.Context, r *check.Report) string
	// ExplainTimeout bounds it.
	ExplainTimeout time.Duration
}

// Bot is a running Telegram bot.
type Bot struct {
	tg     Messenger
	check  Checker
	locate Locator
	cache  *cache.Cache
	cfg    Config
	log    *slog.Logger
	// Served is called after every answer, for logs and measurements.
	Served func(Served)

	slots chan struct{}
	mu    sync.Mutex
	busy  map[int64]bool        // chats with a check in flight
	spent map[int64][]time.Time // start times of each chat's recent uncached checks
	now   func() time.Time
}

// Served describes one answered request.
type Served struct {
	Chain  string
	Token  common.Address
	Level  verdict.Level
	Cached bool
	// Took is from receiving the message to sending the answer.
	Took time.Duration
}

// New builds a bot. c may be nil (no cache).
func New(tg Messenger, check Checker, locate Locator, c *cache.Cache, cfg Config, log *slog.Logger) *Bot {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 2
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.PerChat == 0 {
		cfg.PerChat, cfg.Window = 10, 10*time.Minute
	}
	if cfg.ExplainTimeout == 0 {
		cfg.ExplainTimeout = 30 * time.Second
	}
	return &Bot{
		tg: tg, check: check, locate: locate, cache: c, cfg: cfg, log: log,
		slots: make(chan struct{}, cfg.Concurrency),
		busy:  map[int64]bool{}, spent: map[int64][]time.Time{}, now: time.Now,
	}
}

// Run polls Telegram for messages until ctx ends, handling each message in its own goroutine.
func (b *Bot) Run(ctx context.Context) error {
	var offset int64
	var wg sync.WaitGroup
	defer wg.Wait()
	backoff := time.Second
	for ctx.Err() == nil {
		updates, err := b.tg.Updates(ctx, offset, 30*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			wait := backoff
			if api, ok := errors.AsType[*telegram.APIError](err); ok && api.RetryAfter > 0 {
				wait = api.RetryAfter
			}
			b.log.Warn("polling telegram", "err", err, "retry_in", wait)
			sleep(ctx, wait)
			backoff = min(2*backoff, 30*time.Second)
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			offset = max(offset, u.ID+1)
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			wg.Go(func() { b.Handle(ctx, u.Message) })
		}
	}
	return ctx.Err()
}

const help = `Send me a token address and I'll check whether it is built to take your money.

Example: 0x6982508145454Ce325dDbE47a25d4ec3d2311933
Add "base" to check a token on Base; otherwise I look on Ethereum and Base.

I read the contract (owner powers, taxes, liquidity, the deployer's other tokens) and buy then sell a little of it on a private copy of the chain. Nothing is traded for real. Not financial advice.`

var (
	addressRE = regexp.MustCompile(`0x[0-9a-fA-F]{40}`)
	chainRE   = regexp.MustCompile(`(?i)\b(base|eth|ethereum|mainnet)\b`)
)

// parse finds the token address and the chain named in a message; chain is "" when none is named.
func parse(text string) (token common.Address, chain string, ok bool) {
	m := addressRE.FindString(text)
	if m == "" {
		return token, "", false
	}
	if c := chainRE.FindString(strings.ReplaceAll(text, m, " ")); c != "" {
		chain = "ethereum"
		if strings.EqualFold(c, "base") {
			chain = "base"
		}
	}
	return common.HexToAddress(m), chain, true
}

// Handle answers one message.
func (b *Bot) Handle(ctx context.Context, m *telegram.Message) {
	start := b.now()
	chat := m.Chat.ID
	token, chain, ok := parse(m.Text)
	if !ok {
		if strings.HasPrefix(m.Text, "/") || m.Chat.Type == "private" {
			b.reply(ctx, chat, help, m.ID)
		}
		return
	}
	if !b.claim(chat) {
		b.reply(ctx, chat, "Still checking your previous token. One at a time, please.", m.ID)
		return
	}
	defer b.release(chat)

	note := ""
	if chain == "" {
		// Where an address lives does not change, and looking it up costs a round trip per chain: a cached answer
		// should not wait on the RPCs.
		chains, known := cache.Get[[]string](ctx, b.cache, LocateKey(token))
		var err error
		if !known {
			if chains, err = b.locate(ctx, token); err == nil && len(chains) > 0 {
				cache.Put(ctx, b.cache, LocateKey(token), chains)
			}
		}
		switch {
		case err != nil:
			b.log.Warn("locate", "token", token, "err", err)
			b.reply(ctx, chat, "I could not reach the chain to look this address up. Please try again in a minute.", m.ID)
			return
		case len(chains) == 0:
			b.reply(ctx, chat, fmt.Sprintf("There is no contract at %s on Ethereum or Base. Check the address?", token.Hex()), m.ID)
			return
		}
		chain = chains[0]
		if len(chains) > 1 {
			note = fmt.Sprintf("\n\nThis address is also a contract on %s: send it with %q to check it there.", strings.Join(chains[1:], ", "), chains[1])
		}
	}

	key := Key(chain, token)
	if a, hit := cache.Get[Answer](ctx, b.cache, key); hit {
		b.reply(ctx, chat, a.Text+note+fmt.Sprintf("\n\nCached answer from %s ago.", ago(b.now().Sub(a.CheckedAt))), m.ID)
		b.served(Served{Chain: chain, Token: token, Level: a.Level, Cached: true, Took: b.now().Sub(start)})
		return
	}
	if !b.allow(chat) {
		b.reply(ctx, chat, "That's a lot of checks in a short time. Please wait a few minutes.", m.ID)
		return
	}

	progress, err := b.tg.Send(ctx, chat, fmt.Sprintf("Checking %s on %s… this takes about ten seconds.", token.Hex(), chain), m.ID)
	if err != nil {
		b.log.Warn("send", "chat", chat, "err", err)
		return
	}
	_ = b.tg.Typing(ctx, chat)
	var fresh *check.Report // set only for the request that ran the check, not one that joined it
	a, hit, err := cache.Fetch(ctx, b.cache, key, func(ctx context.Context) (Answer, time.Duration, error) {
		r, err := b.runCheck(ctx, chat, progress, chain, token)
		if err != nil {
			return Answer{}, 0, err
		}
		fresh = r
		a := NewAnswer(r)
		return a, a.TTL(), nil
	})
	text := a.Text + note
	if err != nil {
		text = b.failure(token, chain, err)
	}
	if err := b.tg.Edit(ctx, chat, progress, text); err != nil {
		b.log.Warn("edit", "chat", chat, "err", err)
	}
	if err == nil {
		b.served(Served{Chain: chain, Token: token, Level: a.Level, Cached: hit, Took: b.now().Sub(start)})
		if fresh != nil {
			b.explain(ctx, chat, progress, key, a, note, fresh)
		}
	}
}

// explain adds the plain-words "why" to an answer already sent and cached. It runs after both on purpose: a model is the
// slowest and least reliable part of an answer, and neither its delay nor its failure may hold a verdict back, here or
// for anyone else asking about the same token meanwhile.
func (b *Bot) explain(ctx context.Context, chat, progress int64, key string, a Answer, note string, r *check.Report) {
	if b.cfg.Explain == nil || r.Verdict.Level == verdict.Clear {
		return
	}
	xctx, cancel := context.WithTimeout(ctx, b.cfg.ExplainTimeout)
	defer cancel()
	why := b.cfg.Explain(xctx, r)
	if why == "" {
		return
	}
	a.Text += "\n\n" + why
	cache.PutFor(ctx, b.cache, key, a, a.TTL())
	if err := b.tg.Edit(ctx, chat, progress, a.Text+note); err != nil {
		b.log.Warn("edit", "chat", chat, "err", err)
	}
}

// runCheck performs one check, holding a global slot. The timeout covers the wait for a slot too.
func (b *Bot) runCheck(ctx context.Context, chat, progress int64, chain string, token common.Address) (*check.Report, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	select {
	case b.slots <- struct{}{}:
	default:
		_ = b.tg.Edit(ctx, chat, progress, fmt.Sprintf("Checking %s on %s… queued behind other checks.", token.Hex(), chain))
		select {
		case b.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defer func() { <-b.slots }()
	// A slot freed at the moment of shutdown can win the select above; don't start a fork for nobody.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.check(ctx, chain, token)
}

// Key is the cache key of a token's answer.
func Key(chain string, token common.Address) string {
	return chain + ":" + strings.ToLower(token.Hex())
}

// LocateKey is the cache key of the chains where an address is a contract.
func LocateKey(token common.Address) string {
	return "chains:" + strings.ToLower(token.Hex())
}

// NewAnswer renders a report as the bot's answer.
func NewAnswer(r *check.Report) Answer {
	return Answer{Text: check.Render(r), Level: r.Verdict.Level, Complete: r.Verdict.Complete, Chain: r.Chain,
		CheckedAt: r.CheckedAt, Elapsed: r.Elapsed}
}

// TTL is how long the answer may be cached: the cache's default, or PartialTTL when checks are missing.
func (a Answer) TTL() time.Duration {
	if a.Complete {
		return 0
	}
	return PartialTTL
}

func (b *Bot) failure(token common.Address, chain string, err error) string {
	switch {
	case errors.Is(err, check.ErrNoContract):
		return fmt.Sprintf("There is no contract at %s on %s.", token.Hex(), chain)
	case errors.Is(err, context.DeadlineExceeded):
		return "The check took too long: the free RPCs may be busy. Please try again in a minute."
	}
	b.log.Warn("check", "chain", chain, "token", token, "err", err)
	return "Something went wrong while checking this token. Please try again in a minute."
}

func (b *Bot) reply(ctx context.Context, chat int64, text string, to int64) {
	if _, err := b.tg.Send(ctx, chat, text, to); err != nil {
		b.log.Warn("send", "chat", chat, "err", err)
	}
}

func (b *Bot) served(s Served) {
	b.log.Info("answered", "chain", s.Chain, "token", s.Token, "level", s.Level, "cached", s.Cached, "ms", s.Took.Milliseconds())
	if b.Served != nil {
		b.Served(s)
	}
}

// claim marks a chat busy; false when it already has a check in flight.
func (b *Bot) claim(chat int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busy[chat] {
		return false
	}
	b.busy[chat] = true
	return true
}

func (b *Bot) release(chat int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.busy, chat)
}

// allow records an uncached check for a chat, unless the chat has used up its allowance for the window.
func (b *Bot) allow(chat int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	recent := b.spent[chat][:0]
	for _, t := range b.spent[chat] {
		if now.Sub(t) < b.cfg.Window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= b.cfg.PerChat {
		b.spent[chat] = recent
		return false
	}
	b.spent[chat] = append(recent, now)
	return true
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < 2*time.Minute:
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
