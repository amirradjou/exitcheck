package store

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"github.com/amirradjou/rugcheck/internal/launch"
)

// Backfill is a launch to check again at a past block.
type Backfill struct {
	launch.Launch
	CheckBlock uint64
	Source     string // watch or scan
}

// BackfillFromWatch queues the live watch's launches analyzed before their report was kept, to be checked again at
// the block their answer was given for (its last line names it).
func (s *Store) BackfillFromWatch(ctx context.Context) (int, error) {
	tag, err := s.db.Exec(ctx, `INSERT INTO backfill
		(id, chain, dex, pool, token, quote, created_block, funded_block, funded_at, check_block, source)
		SELECT id, chain, dex, pool, token, quote, created_block, funded_block, funded_at,
			coalesce(substring(answer from 'Block ([0-9]+) ·')::bigint, funded_block), 'watch'
		FROM launches WHERE report IS NULL AND analyzed_at IS NOT NULL AND error IS NULL
		ON CONFLICT (id) DO NOTHING`)
	return int(tag.RowsAffected()), err
}

// AddBackfill queues launches found in the factories' logs; ones already queued are left as they are.
func (s *Store) AddBackfill(ctx context.Context, bs []Backfill) (int, error) {
	n := 0
	for _, b := range bs {
		tag, err := s.db.Exec(ctx, `INSERT INTO backfill
			(id, chain, dex, pool, token, quote, created_block, funded_block, funded_at, check_block, source)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT (id) DO NOTHING`,
			b.ID(), b.Chain, b.DEX, hex(b.Pool.Pool), hex(b.Token), hex(b.Quote), int64(b.Block), int64(b.FundedBlock),
			b.FundedAt, int64(b.CheckBlock), b.Source)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// PendingBackfill lists a chain's queued launches not yet checked, newest first, so that a backfill stopped part way
// leaves an unbroken stretch of launches up to the present. limit 0 means all.
func (s *Store) PendingBackfill(ctx context.Context, chain string, limit int) ([]Backfill, error) {
	q := `SELECT id, dex, pool, token, quote, created_block, funded_block, funded_at, check_block, source
		FROM backfill WHERE chain = $1 AND checked_at IS NULL ORDER BY funded_at DESC`
	args := []any{chain}
	if limit > 0 {
		q += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Backfill, error) {
		var b Backfill
		var id, pool, token, quote string
		var created, funded, check int64
		err := r.Scan(&id, &b.DEX, &pool, &token, &quote, &created, &funded, &b.FundedAt, &check, &b.Source)
		b.Chain, b.Pool.Pool, b.Token, b.Quote = chain, common.HexToAddress(pool), common.HexToAddress(token), common.HexToAddress(quote)
		b.Block, b.FundedBlock, b.CheckBlock = uint64(created), uint64(funded), uint64(check)
		return b, err
	})
}

// FinishBackfill stores a backfilled check: its level and JSON report, or the error that stopped it.
func (s *Store) FinishBackfill(ctx context.Context, id, level string, report []byte, checkErr string) error {
	_, err := s.db.Exec(ctx, `UPDATE backfill SET level = $2, report = $3, error = $4, checked_at = $5 WHERE id = $1`,
		id, nullString(level), nullString(string(report)), nullString(checkErr), time.Now())
	return err
}
