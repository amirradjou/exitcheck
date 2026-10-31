package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/launch"
)

// Integration: needs Postgres (docker compose up -d postgres); run with RUGCHECK_INTEGRATION=1. DATABASE_URL
// overrides the compose default.
func open(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("RUGCHECK_INTEGRATION") == "" {
		t.Skip("set RUGCHECK_INTEGRATION=1 (with Postgres from compose.yaml)")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://rugcheck:rugcheck@localhost:5434/rugcheck?sslmode=disable"
	}
	s, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// chain names a fresh chain per run, so runs never see each other's rows, and deletes the rows afterwards: the
// tests share the database the live watch reports from.
func chain(t *testing.T, s *Store) string {
	c := fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		for _, table := range []string{"watch_cursors", "pending_pools", "launches", "alerts"} {
			if _, err := s.db.Exec(context.Background(), "DELETE FROM "+table+" WHERE chain = $1", c); err != nil {
				t.Errorf("cleaning %s: %v", table, err)
			}
		}
	})
	return c
}

func TestCursorAndPendingAdvanceTogether(t *testing.T) {
	s, ctx := open(t), context.Background()
	c := chain(t, s)
	if _, ok, err := s.Cursor(ctx, c); err != nil || ok {
		t.Fatalf("fresh chain has a cursor (%v, %v)", ok, err)
	}
	a := launch.Pool{Chain: c, DEX: "uniswap-v2", Pool: common.HexToAddress("0xa1"), Token: common.HexToAddress("0x71"),
		Quote: common.HexToAddress("0x42"), Block: 5, Tx: common.HexToHash("0x99"), SeenAt: time.Now().UTC().Truncate(time.Microsecond)}
	b := a
	b.Pool = common.HexToAddress("0xa2")
	if err := s.Advance(ctx, c, 10, []launch.Pool{a, b}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance(ctx, c, 12, nil, []common.Address{a.Pool}); err != nil {
		t.Fatal(err)
	}
	cur, ok, err := s.Cursor(ctx, c)
	pend, perr := s.Pending(ctx, c)
	if err != nil || perr != nil || !ok || cur != 12 || len(pend) != 1 || pend[0] != b {
		t.Errorf("cursor %d (%v, %v); pending %+v (%v)", cur, ok, err, pend, perr)
	}
}

func TestLaunchRowsCountRedeliveries(t *testing.T) {
	s, ctx := open(t), context.Background()
	c := chain(t, s)
	l := launch.Launch{Pool: launch.Pool{Chain: c, DEX: "aerodrome", Pool: common.HexToAddress("0xb1"), Token: common.HexToAddress("0x72"),
		Quote: common.HexToAddress("0x42"), Block: 7}, FundedBlock: 8, FundedAt: time.Now(), DetectedAt: time.Now()}
	if p, err := s.Receive(ctx, l); err != nil || p.Done {
		t.Fatalf("first delivery: %+v, %v", p, err)
	}
	report := []byte(`{"verdict":{"level":"scam"},"facts":{"block":8}}`)
	if err := s.Finish(ctx, l.ID(), Result{Level: "scam", Headline: "Honeypot", Answer: "text", Complete: true, CheckSeconds: 8.5,
		Report: report}); err != nil {
		t.Fatal(err)
	}
	var block int
	if err := s.db.QueryRow(ctx, `SELECT (report->'facts'->>'block')::int FROM launches WHERE id = $1`, l.ID()).Scan(&block); err != nil || block != 8 {
		t.Errorf("stored report: facts.block = %d (%v)", block, err)
	}
	p, err := s.Receive(ctx, l)
	if err != nil || !p.Done || p.Level != "scam" || p.Text != "text" {
		t.Fatalf("redelivery: %+v, %v", p, err)
	}
	var n int
	if err := s.db.QueryRow(ctx, `SELECT deliveries FROM launches WHERE id = $1`, l.ID()).Scan(&n); err != nil || n != 2 {
		t.Errorf("deliveries %d (%v)", n, err)
	}
}

func TestAlertsAreClaimedOncePerToken(t *testing.T) {
	s, ctx := open(t), context.Background()
	c := chain(t, s)
	tok := common.HexToAddress("0x73")
	if ok, err := s.ClaimAlert(ctx, c, tok, c+":p1", "scam"); err != nil || !ok {
		t.Fatalf("first claim: %v, %v", ok, err)
	}
	if ok, _ := s.ClaimAlert(ctx, c, tok, c+":p2", "scam"); ok {
		t.Error("a second claim on the same token succeeded")
	}
	if err := s.ReleaseAlert(ctx, c, tok); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ClaimAlert(ctx, c, tok, c+":p2", "scam"); !ok {
		t.Error("a released claim could not be taken again")
	}
	if err := s.AlertSent(ctx, c, tok, true); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAlert(ctx, c, tok); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ClaimAlert(ctx, c, tok, c+":p3", "scam"); ok {
		t.Error("a sent alert was claimed again after a release")
	}
}

func TestRedactHidesThePassword(t *testing.T) {
	err := redact(fmt.Errorf(`connect to postgres://rugcheck:hunter2@db:5432/x failed`))
	if got := err.Error(); got != "connect to postgres://…@db:5432/x failed" {
		t.Errorf("redacted to %q", got)
	}
}

func TestReport(t *testing.T) {
	s, ctx := open(t), context.Background()
	c := chain(t, s)
	since := time.Now().Add(-time.Minute)
	funded := time.Now().Add(-30 * time.Second)
	for i, lvl := range []string{"scam", "clear"} {
		l := launch.Launch{Pool: launch.Pool{Chain: c, DEX: "uniswap-v2", Pool: common.BytesToAddress([]byte{0xc0, byte(i)}),
			Token: common.BytesToAddress([]byte{0x74, byte(i)}), Quote: common.HexToAddress("0x42")},
			FundedBlock: 1, FundedAt: funded, DetectedAt: funded.Add(3 * time.Second)}
		if _, err := s.Receive(ctx, l); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(ctx, l.ID(), Result{Level: lvl, CheckSeconds: 9}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Report(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	// Other test runs share the table, so check this run's chain and the report's internal consistency.
	if r.Chains[c] != 2 || r.Launches < 2 || r.Analyzed+r.Failed+r.Waiting != r.Launches {
		t.Errorf("report %+v", r)
	}
	if r.LagP50 < 25 || r.DetectP50 < 2.9 || r.DuplicateResults != 0 {
		t.Errorf("lag %.1f s, detect %.1f s, duplicates %d", r.LagP50, r.DetectP50, r.DuplicateResults)
	}
}
