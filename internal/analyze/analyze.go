// Package analyze is the live watch's worker: it checks each launch the stream delivers, stores the verdict, and
// alerts about high-risk launches. The check is the bot's own (same rules, same Redis cache), so a token asked about
// in Telegram right after its launch is answered from the cache.
package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/bot"
	"github.com/amirradjou/rugcheck/internal/cache"
	"github.com/amirradjou/rugcheck/internal/check"
	"github.com/amirradjou/rugcheck/internal/launch"
	"github.com/amirradjou/rugcheck/internal/store"
	"github.com/amirradjou/rugcheck/internal/verdict"
)

// Store is the part of the store the analyzer uses.
type Store interface {
	Receive(ctx context.Context, l launch.Launch) (store.Prior, error)
	Finish(ctx context.Context, id string, r store.Result) error
	ClaimAlert(ctx context.Context, chain string, token common.Address, launchID, level string) (bool, error)
	AlertSent(ctx context.Context, chain string, token common.Address, posted bool) error
	ReleaseAlert(ctx context.Context, chain string, token common.Address) error
}

// Alerter posts an alert; nil means alerts are stored and logged but not posted.
type Alerter func(ctx context.Context, text string) error

// Analyzer handles launches.
type Analyzer struct {
	store Store
	check bot.Checker
	cache *cache.Cache
	alert Alerter
	log   *slog.Logger
	slots chan struct{}
	// Attempts is how many times a failing check is tried before its error is stored as the result.
	Attempts int
	// Retry is the wait between attempts.
	Retry time.Duration
	// MaxAlertAge is how old a launch may be for its alert to go out. After an outage the watcher reads the blocks it
	// missed, so nothing is lost, but a channel should not get a day of old launches at once: half of them were pulled
	// long ago. They are still checked and stored.
	MaxAlertAge time.Duration
}

// New returns an analyzer running at most concurrency checks at once (each one forks the chain with Anvil).
func New(s Store, check bot.Checker, c *cache.Cache, alert Alerter, concurrency int, log *slog.Logger) *Analyzer {
	return &Analyzer{store: s, check: check, cache: c, alert: alert, log: log,
		slots: make(chan struct{}, max(concurrency, 1)), Attempts: 3, Retry: 5 * time.Second, MaxAlertAge: 30 * time.Minute}
}

// Handle processes one delivery of a launch. It returns an error only when the store cannot be reached: the stream
// then retries without committing. A check that keeps failing is stored as a failed result, not retried forever.
func (a *Analyzer) Handle(ctx context.Context, l launch.Launch) error {
	prior, err := a.store.Receive(ctx, l)
	if err != nil {
		return fmt.Errorf("recording the launch: %w", err)
	}
	level, text := prior.Level, prior.Text
	if prior.Done {
		a.log.Info("redelivered launch; already analyzed", "id", l.ID())
	} else {
		select {
		case a.slots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		res, ans := a.run(ctx, l)
		<-a.slots
		if ctx.Err() != nil {
			return ctx.Err() // cut off by shutdown: leave it to the replay
		}
		if err := a.store.Finish(ctx, l.ID(), res); err != nil {
			return fmt.Errorf("storing the verdict: %w", err)
		}
		level, text = res.Level, ans.Text
		lag := time.Since(l.FundedAt)
		a.log.Info("analyzed", "id", l.ID(), "token", l.Token, "level", res.Level, "cached", res.FromCache,
			"check_s", res.CheckSeconds, "lag_s", lag.Seconds(), "err", res.Error)
	}
	if lv, err := verdict.ParseLevel(level); err == nil && lv >= verdict.HighRisk {
		return a.raise(ctx, l, lv, text)
	}
	return nil
}

// run checks the launch's token, through the shared answer cache, retrying failures that may be transient.
func (a *Analyzer) run(ctx context.Context, l launch.Launch) (store.Result, bot.Answer) {
	start := time.Now()
	var ans bot.Answer
	var report *check.Report // set only when this call ran the check, not when the cache answered
	var hit bool
	var err error
	for attempt := 1; attempt <= a.Attempts; attempt++ {
		ans, hit, err = cache.Fetch(ctx, a.cache, bot.Key(l.Chain, l.Token), func(ctx context.Context) (bot.Answer, time.Duration, error) {
			r, err := a.check(ctx, l.Chain, l.Token)
			if err != nil {
				return bot.Answer{}, 0, err
			}
			report = r
			ans := bot.NewAnswer(r)
			return ans, ans.TTL(), nil
		})
		if err == nil || errors.Is(err, check.ErrNoContract) || ctx.Err() != nil {
			break
		}
		a.log.Warn("check failed", "id", l.ID(), "attempt", attempt, "err", err)
		if attempt < a.Attempts {
			sleep(ctx, a.Retry)
		}
	}
	res := store.Result{CheckSeconds: time.Since(start).Seconds(), FromCache: hit}
	if err != nil {
		res.Error = err.Error()
		return res, ans
	}
	res.Level, res.Answer, res.Complete = ans.Level.String(), ans.Text, ans.Complete
	res.Headline, _, _ = strings.Cut(ans.Text, "\n")
	if report != nil {
		if res.Report, err = json.Marshal(report); err != nil {
			a.log.Warn("encoding the report; storing the verdict without it", "id", l.ID(), "err", err)
		}
	}
	return res, ans
}

// raise alerts about a token once: the store's claim decides who sends.
func (a *Analyzer) raise(ctx context.Context, l launch.Launch, level verdict.Level, text string) error {
	claimed, err := a.store.ClaimAlert(ctx, l.Chain, l.Token, l.ID(), level.String())
	if err != nil {
		return fmt.Errorf("claiming the alert: %w", err)
	}
	if !claimed {
		return nil
	}
	post := a.alert != nil
	if age := time.Since(l.FundedAt); post && age > a.MaxAlertAge {
		post = false
		a.log.Warn("launch too old to alert about; stored only", "id", l.ID(), "age_s", age.Seconds())
	}
	if post {
		msg := fmt.Sprintf("🚨 New launch on %s (%s pool, liquidity at block %d, %s UTC)\n\n%s",
			l.Chain, l.DEX, l.FundedBlock, l.FundedAt.UTC().Format("15:04:05"), text)
		if err := a.alert(ctx, msg); err != nil {
			// Give the claim back and fail the delivery: the stream retries until the alert is posted. That holds
			// the partition while Telegram is down, which beats losing a scam alert.
			if rerr := a.store.ReleaseAlert(ctx, l.Chain, l.Token); rerr != nil {
				a.log.Warn("releasing the alert claim", "id", l.ID(), "err", rerr)
			}
			return fmt.Errorf("posting the alert: %w", err)
		}
	}
	a.log.Info("alert", "id", l.ID(), "token", l.Token, "level", level, "posted", post)
	return a.store.AlertSent(ctx, l.Chain, l.Token, post)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
