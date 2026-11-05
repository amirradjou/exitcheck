package store

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"github.com/amirradjou/rugcheck/internal/outcome"
)

// Unlabelled lists a chain's launches (live or backfilled) funded before cutoff that have no outcome yet, oldest first.
// checkedOnly keeps those with a report, the only ones a dataset can use. limit 0 means all.
func (s *Store) Unlabelled(ctx context.Context, chain string, cutoff time.Time, checkedOnly bool, limit int) ([]outcome.Launch, error) {
	q := `SELECT l.id, l.dex, l.pool, l.token, l.quote, l.created_block, l.funded_block, l.funded_at
		FROM (SELECT id, chain, dex, pool, token, quote, created_block, funded_block, funded_at FROM launches
			UNION SELECT id, chain, dex, pool, token, quote, created_block, funded_block, funded_at FROM backfill) l
		LEFT JOIN outcomes o ON o.launch_id = l.id
		WHERE l.chain = $1 AND l.funded_at < $2 AND o.launch_id IS NULL AND (NOT $3
			OR EXISTS (SELECT 1 FROM launches x WHERE x.id = l.id AND x.report IS NOT NULL)
			OR EXISTS (SELECT 1 FROM backfill x WHERE x.id = l.id AND x.report IS NOT NULL))
		ORDER BY l.funded_at`
	args := []any{chain, cutoff, checkedOnly}
	if limit > 0 {
		q += ` LIMIT $4`
		args = append(args, limit)
	}
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (outcome.Launch, error) {
		var l outcome.Launch
		var pool, token, quote string
		var created, funded int64
		err := r.Scan(&l.ID, &l.DEX, &pool, &token, &quote, &created, &funded, &l.FundedAt)
		l.Pool, l.Token, l.Quote = common.HexToAddress(pool), common.HexToAddress(token), common.HexToAddress(quote)
		l.CreatedBlock, l.FundedBlock = uint64(created), uint64(funded)
		return l, err
	})
}

// SaveOutcome stores a launch's outcome; labelling it again replaces it.
func (s *Store) SaveOutcome(ctx context.Context, id string, horizon time.Duration, end uint64, o outcome.Outcome) error {
	var pulledBlock *int64
	if o.Pulled {
		b := int64(o.PulledBlock)
		pulledBlock = &b
	}
	_, err := s.db.Exec(ctx, `INSERT INTO outcomes
		(launch_id, horizon_s, end_block, pulled, pulled_block, pulled_at, buys, sells, bait)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (launch_id) DO UPDATE SET horizon_s = EXCLUDED.horizon_s, end_block = EXCLUDED.end_block,
			pulled = EXCLUDED.pulled, pulled_block = EXCLUDED.pulled_block, pulled_at = EXCLUDED.pulled_at,
			buys = EXCLUDED.buys, sells = EXCLUDED.sells, bait = EXCLUDED.bait, labelled_at = now()`,
		id, int(horizon.Seconds()), int64(end), o.Pulled, pulledBlock, nullTime(o.PulledAt), o.Buys, o.Sells, o.Bait)
	return err
}

// Sample is one labelled launch with the report its features come from.
type Sample struct {
	ID, Chain, DEX string
	FundedAt       time.Time
	// Source is live (checked by the live watch), watch (a live-watch launch checked again on a fork) or scan (an
	// older launch found in the factories' logs).
	Source string
	// Level is the rules' verdict on Report; LiveLevel is what the live watch said, when it saw the launch.
	Level, LiveLevel string
	Report           []byte
	Outcome          outcome.Outcome
}

// Samples lists every launch with both a report and an outcome, oldest first. A live report is preferred to a
// backfilled one.
func (s *Store) Samples(ctx context.Context) ([]Sample, error) {
	rows, err := s.db.Query(ctx, `
		SELECT o.launch_id, coalesce(l.chain, b.chain), coalesce(l.dex, b.dex), coalesce(l.funded_at, b.funded_at),
			CASE WHEN l.report IS NOT NULL THEN 'live' ELSE b.source END,
			CASE WHEN l.report IS NOT NULL THEN l.level ELSE b.level END, coalesce(l.level, ''),
			coalesce(l.report, b.report)::text, o.pulled, coalesce(o.pulled_block, 0), o.pulled_at, o.buys, o.sells, o.bait
		FROM outcomes o LEFT JOIN launches l ON l.id = o.launch_id LEFT JOIN backfill b ON b.id = o.launch_id
		WHERE coalesce(l.report, b.report) IS NOT NULL
		ORDER BY 4`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Sample, error) {
		var x Sample
		var report string
		var pulledBlock int64
		var pulledAt *time.Time
		err := r.Scan(&x.ID, &x.Chain, &x.DEX, &x.FundedAt, &x.Source, &x.Level, &x.LiveLevel, &report,
			&x.Outcome.Pulled, &pulledBlock, &pulledAt, &x.Outcome.Buys, &x.Outcome.Sells, &x.Outcome.Bait)
		x.Report, x.Outcome.PulledBlock = []byte(report), uint64(pulledBlock)
		if pulledAt != nil {
			x.Outcome.PulledAt = *pulledAt
		}
		return x, err
	})
}
