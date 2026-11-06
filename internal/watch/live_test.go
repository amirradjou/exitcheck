package watch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/launch"
)

// Live: the RPC source against Ethereum's history and head, through the default public RPCs; run with
// EXITCHECK_LIVE=1.
func TestLiveRPCSource(t *testing.T) {
	if os.Getenv("EXITCHECK_LIVE") == "" {
		t.Skip("set EXITCHECK_LIVE=1 to use the public RPCs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := chain.Dial(ctx, chain.Ethereum)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	src := NewRPC(c)

	// The v2 pool of launch/testdata/ethereum-uniswap-v2.json, created in block 0x19153b1.
	const block = 0x19153b1
	created, err := src.Created(ctx, block, block)
	if err != nil {
		t.Fatal(err)
	}
	var found *launch.Pool
	for i, p := range created {
		if p.Pool == common.HexToAddress("0x95A21519B9b639dC976C437518E22BFE1c6562c8") {
			found = &created[i]
		}
	}
	if found == nil {
		t.Fatalf("pool not among %d created in block %d", len(created), block)
	}
	b, ok, err := src.FirstMint(ctx, *found, block, block+50_000)
	if err != nil || !ok || b < block {
		t.Errorf("first mint: block %d, found %v, err %v", b, ok, err)
	}

	pepe := launch.Pool{DEX: "uniswap-v2", Pool: common.HexToAddress("0xA43fe16908251ee70EF74718545e4FE6C5cCEc9f"),
		Token: common.HexToAddress("0x6982508145454Ce325dDbE47a25d4ec3d2311933"), Quote: chain.Ethereum.WETH}
	head, err := src.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	funded, err := src.Funded(ctx, []launch.Pool{pepe}, head)
	if err != nil || !funded[0] {
		t.Errorf("PEPE's pair funded: %v, %v", funded, err)
	}

	heads, err := src.Heads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case h := <-heads:
		if h < block {
			t.Errorf("head %d", h)
		}
	case <-time.After(45 * time.Second):
		t.Error("no new head within 45 s")
	}
}
