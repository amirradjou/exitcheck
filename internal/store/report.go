package store

import (
	"context"
	"time"
)

// Report summarizes the live watch over a window: how many launches, how fast they were analyzed, and whether
// anything was delivered or alerted twice.
type Report struct {
	Since, Until time.Time
	// Launches counts pools funded in the window; Analyzed have a result, Failed a stored error, Waiting neither.
	Launches, Analyzed, Failed, Waiting int
	// Lag is from the first liquidity's block timestamp to the stored verdict, in seconds; Detect is the watcher's
	// share of it (block timestamp to detection); Check is the check itself.
	LagP50, LagP95, LagMax float64
	DetectP50, DetectP95   float64
	CheckP50               float64
	// OverMinute counts analyzed launches whose lag exceeded a minute.
	OverMinute int
	// Redelivered counts launches Kafka delivered more than once; Extra is the number of extra deliveries. Each was
	// turned into a count by the launch key, never into a second result.
	Redelivered, Extra int
	// DuplicateResults and DuplicateAlerts count pools with more than one result and tokens with more than one
	// alert. The keys make both zero; the report checks rather than assumes.
	DuplicateResults, DuplicateAlerts int
	Alerts, AlertsSent                int
	Levels                            map[string]int
	Chains                            map[string]int
}

// Report reads the window from since to now.
func (s *Store) Report(ctx context.Context, since time.Time) (Report, error) {
	r := Report{Since: since, Until: time.Now(), Levels: map[string]int{}, Chains: map[string]int{}}
	var lag50, lag95, lagMax, det50, det95, chk50 *float64
	err := s.db.QueryRow(ctx, `
		WITH w AS (
			SELECT *, extract(epoch FROM analyzed_at - funded_at) AS lag, extract(epoch FROM detected_at - funded_at) AS detect
			FROM launches WHERE funded_at >= $1)
		SELECT count(*), count(analyzed_at) FILTER (WHERE error IS NULL), count(*) FILTER (WHERE error IS NOT NULL),
			count(*) FILTER (WHERE analyzed_at IS NULL),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY lag), percentile_cont(0.95) WITHIN GROUP (ORDER BY lag), max(lag),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY detect), percentile_cont(0.95) WITHIN GROUP (ORDER BY detect),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY check_seconds),
			count(*) FILTER (WHERE lag > 60),
			count(*) FILTER (WHERE deliveries > 1), coalesce(sum(deliveries - 1), 0)
		FROM w`, since).Scan(&r.Launches, &r.Analyzed, &r.Failed, &r.Waiting, &lag50, &lag95, &lagMax, &det50, &det95, &chk50,
		&r.OverMinute, &r.Redelivered, &r.Extra)
	if err != nil {
		return r, err
	}
	r.LagP50, r.LagP95, r.LagMax, r.DetectP50, r.DetectP95, r.CheckP50 = val(lag50), val(lag95), val(lagMax), val(det50), val(det95), val(chk50)

	if err := s.db.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM (SELECT chain, pool FROM launches WHERE funded_at >= $1 GROUP BY 1, 2 HAVING count(*) > 1) d),
			(SELECT count(*) FROM (SELECT chain, token FROM alerts WHERE claimed_at >= $1 GROUP BY 1, 2 HAVING count(*) > 1) d),
			(SELECT count(*) FROM alerts WHERE claimed_at >= $1),
			(SELECT count(*) FILTER (WHERE posted) FROM alerts WHERE claimed_at >= $1)`, since).
		Scan(&r.DuplicateResults, &r.DuplicateAlerts, &r.Alerts, &r.AlertsSent); err != nil {
		return r, err
	}

	for _, q := range []struct {
		sql string
		m   map[string]int
	}{
		{`SELECT coalesce(level, CASE WHEN error IS NOT NULL THEN 'error' ELSE 'waiting' END), count(*)
			FROM launches WHERE funded_at >= $1 GROUP BY 1`, r.Levels},
		{`SELECT chain, count(*) FROM launches WHERE funded_at >= $1 GROUP BY 1`, r.Chains},
	} {
		rows, err := s.db.Query(ctx, q.sql, since)
		if err != nil {
			return r, err
		}
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				rows.Close()
				return r, err
			}
			q.m[k] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return r, err
		}
	}
	return r, nil
}

func val(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
