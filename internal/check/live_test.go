package check

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/explorer"
)

// Live: a facts budget too short for the slow stages must still produce a verdict, with those stages listed as not
// checked. Run with EXITCHECK_LIVE=1 (public RPCs and anvil).
func TestLiveBudgetBecomesNotChecked(t *testing.T) {
	if os.Getenv("EXITCHECK_LIVE") == "" {
		t.Skip("set EXITCHECK_LIVE=1 to use the public RPCs")
	}
	defer func(b time.Duration) { FactsBudget = b }(FactsBudget)
	FactsBudget = 1500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := chain.Dial(ctx, chain.Ethereum)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := Run(ctx, c, explorer.New(), common.HexToAddress("0x6982508145454Ce325dDbE47a25d4ec3d2311933"))
	if err != nil {
		t.Fatalf("a cut-short check failed instead of answering: %v", err)
	}
	if len(r.Verdict.NotChecked) == 0 {
		t.Errorf("nothing listed as not checked after a 1.5 s facts budget (facts took %v)", r.Facts.Elapsed)
	}
	t.Logf("level %s in %v; not checked: %q", r.Verdict.Level, r.Elapsed, r.Verdict.NotChecked)
	t.Log("\n" + Render(r))
}
