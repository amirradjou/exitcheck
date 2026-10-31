// Package store keeps the live watch's state in Postgres: the watcher's cursor and pending pools, each launch with
// its verdict, and the alerts sent. Duplicates are ruled out by keys, not by care: a launch is one row per pool,
// so a redelivered message only counts a delivery; an alert is one row per token, claimed before it is sent.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/amirradjou/rugcheck/internal/launch"
)

//go:embed schema.sql
var schema string

// Store is a Postgres connection pool.
type Store struct {
	db *pgxpool.Pool
}

// Open connects and creates the tables if they do not exist.
func Open(ctx context.Context, url string) (*Store, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", redact(err))
	}
	if _, err := db.Exec(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres: creating tables: %w", redact(err))
	}
	return &Store{db: db}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.db.Close() }

// redact keeps a connection string's password out of errors.
func redact(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, "://"); i >= 0 {
		if j := strings.Index(msg[i:], "@"); j >= 0 {
			msg = msg[:i+3] + "…" + msg[i+j:]
		}
	}
	return errors.New(msg)
}

// Cursor is the last block the watcher fully processed on a chain.
func (s *Store) Cursor(ctx context.Context, chain string) (uint64, bool, error) {
	var b int64
	err := s.db.QueryRow(ctx, `SELECT block FROM watch_cursors WHERE chain = $1`, chain).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return uint64(b), err == nil, err
}

// Pending lists a chain's pools created but not yet funded.
func (s *Store) Pending(ctx context.Context, chain string) ([]launch.Pool, error) {
	rows, err := s.db.Query(ctx, `SELECT dex, pool, token, quote, created_block, created_tx, seen_at
		FROM pending_pools WHERE chain = $1`, chain)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (launch.Pool, error) {
		var p launch.Pool
		var pool, token, quote, tx string
		var block int64
		err := r.Scan(&p.DEX, &pool, &token, &quote, &block, &tx, &p.SeenAt)
		p.Chain, p.Pool, p.Token, p.Quote = chain, common.HexToAddress(pool), common.HexToAddress(token), common.HexToAddress(quote)
		p.Block, p.Tx, p.SeenAt = uint64(block), common.HexToHash(tx), p.SeenAt.UTC()
		return p, err
	})
}

// Advance saves the cursor and the change in pending pools, all or nothing.
func (s *Store) Advance(ctx context.Context, chain string, block uint64, added []launch.Pool, removed []common.Address) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO watch_cursors (chain, block) VALUES ($1, $2)
			ON CONFLICT (chain) DO UPDATE SET block = EXCLUDED.block, updated_at = now()`, chain, int64(block)); err != nil {
			return err
		}
		for _, p := range added {
			if _, err := tx.Exec(ctx, `INSERT INTO pending_pools (chain, pool, dex, token, quote, created_block, created_tx, seen_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (chain, pool) DO NOTHING`,
				chain, hex(p.Pool), p.DEX, hex(p.Token), hex(p.Quote), int64(p.Block), p.Tx.Hex(), p.SeenAt); err != nil {
				return err
			}
		}
		if len(removed) > 0 {
			pools := make([]string, len(removed))
			for i, a := range removed {
				pools[i] = hex(a)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM pending_pools WHERE chain = $1 AND pool = ANY($2)`, chain, pools); err != nil {
				return err
			}
		}
		return nil
	})
}

// Prior is what an earlier delivery of a launch left behind.
type Prior struct {
	// Done is set when the launch was already analyzed; Level and Text are its verdict.
	Done  bool
	Level string
	Text  string
}

// Receive records a delivery of a launch: a new row the first time, a delivery count after that.
func (s *Store) Receive(ctx context.Context, l launch.Launch) (Prior, error) {
	var p Prior
	var level, text *string
	err := s.db.QueryRow(ctx, `INSERT INTO launches
		(id, chain, dex, pool, token, quote, created_block, created_tx, funded_block, funded_at, seen_at, detected_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO UPDATE SET deliveries = launches.deliveries + 1
		RETURNING analyzed_at IS NOT NULL, level, answer`,
		l.ID(), l.Chain, l.DEX, hex(l.Pool.Pool), hex(l.Token), hex(l.Quote), int64(l.Block), l.Tx.Hex(),
		int64(l.FundedBlock), l.FundedAt, nullTime(l.SeenAt), l.DetectedAt).Scan(&p.Done, &level, &text)
	if level != nil {
		p.Level = *level
	}
	if text != nil {
		p.Text = *text
	}
	return p, err
}

// Result is a launch's verdict, or the error that stopped its check.
type Result struct {
	Level, Headline, Answer string
	Complete, FromCache     bool
	Error                   string
	CheckSeconds            float64
	// Report is the check's full report as JSON (facts, simulation, verdict): the features as they stood at the
	// checked block. Nil when the answer came from the cache.
	Report []byte
}

// Finish stores a launch's result.
func (s *Store) Finish(ctx context.Context, id string, r Result) error {
	_, err := s.db.Exec(ctx, `UPDATE launches SET analyzed_at = now(), level = $2, headline = $3, answer = $4,
		complete = $5, from_cache = $6, error = $7, check_seconds = $8, report = $9 WHERE id = $1`,
		id, nullString(r.Level), nullString(r.Headline), nullString(r.Answer), r.Complete, r.FromCache, nullString(r.Error),
		r.CheckSeconds, nullString(string(r.Report)))
	return err
}

// ClaimAlert reserves the right to alert about a token: true for the first claim, and for a claim that was never
// marked sent within five minutes (its sender died). Everyone else gets false.
func (s *Store) ClaimAlert(ctx context.Context, chain string, token common.Address, launchID, level string) (bool, error) {
	var id string
	err := s.db.QueryRow(ctx, `INSERT INTO alerts (chain, token, launch_id, level) VALUES ($1, $2, $3, $4)
		ON CONFLICT (chain, token) DO UPDATE SET claimed_at = now(), launch_id = EXCLUDED.launch_id
			WHERE alerts.sent_at IS NULL AND alerts.claimed_at < now() - interval '5 minutes'
		RETURNING launch_id`, chain, hex(token), launchID, level).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ReleaseAlert gives up an unsent claim, so the next attempt can claim it at once.
func (s *Store) ReleaseAlert(ctx context.Context, chain string, token common.Address) error {
	_, err := s.db.Exec(ctx, `DELETE FROM alerts WHERE chain = $1 AND token = $2 AND sent_at IS NULL`, chain, hex(token))
	return err
}

// AlertSent settles a claimed alert; posted says whether it went to a channel or was only recorded.
func (s *Store) AlertSent(ctx context.Context, chain string, token common.Address, posted bool) error {
	_, err := s.db.Exec(ctx, `UPDATE alerts SET sent_at = now(), posted = $3 WHERE chain = $1 AND token = $2`,
		chain, hex(token), posted)
	return err
}

func hex(a common.Address) string { return strings.ToLower(a.Hex()) }

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
