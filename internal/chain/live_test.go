package chain

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Live tests hit the default public RPCs; run them with RUGCHECK_LIVE=1.
func liveClient(t *testing.T, net Network) *Client {
	t.Helper()
	if os.Getenv("RUGCHECK_LIVE") == "" {
		t.Skip("set RUGCHECK_LIVE=1 to run tests against public RPCs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	c, err := Dial(ctx, net)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestLiveCreationOfPepe(t *testing.T) {
	c := liveClient(t, Ethereum)
	ctx := context.Background()
	pepe := common.HexToAddress("0x6982508145454Ce325dDbE47a25d4ec3d2311933")
	head, err := c.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	block, err := c.CreationBlock(ctx, pepe, head)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := c.Creator(ctx, pepe, block)
	if err != nil {
		t.Fatal(err)
	}
	// Etherscan and Blockscout both list this deployment.
	if want := common.HexToHash("0x2afae7763487e60b893cb57803694810e6d3d136186a6de6719921afd7ca304a"); cr.Tx != want {
		t.Errorf("creation tx = %s, want %s (block %d)", cr.Tx, want, block)
	}
	if want := common.HexToAddress("0xfbfEaF0DA0F2fdE5c66dF570133aE35f3eB58c9A"); cr.Deployer != want {
		t.Errorf("deployer = %s, want %s", cr.Deployer, want)
	}
	if cr.Factory != nil {
		t.Errorf("factory = %s, want direct deployment", cr.Factory)
	}
}

func TestLiveMulticallAndLogs(t *testing.T) {
	c := liveClient(t, Base)
	ctx := context.Background()
	res, err := c.Multicall(ctx, []Call{
		{To: Base.WETH, Data: common.FromHex("0x95d89b41")}, // symbol()
		// WETH's fallback accepts any selector; the factory has no fallback, so this reverts.
		{To: Base.V2[0].Factory, Data: common.FromHex("0xdeadbeef")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].OK || res[1].OK {
		t.Fatalf("multicall results = %+v", res)
	}
	head, err := c.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 3 windows on Base's 1000-block limit.
	n := 0
	q := ethereum.FilterQuery{Addresses: []common.Address{Base.V2[0].Factory}}
	err = c.ScanLogs(ctx, LogScan{From: head - 2_500, To: head, Query: q},
		func(logs []types.Log) error { n += len(logs); return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d PairCreated logs in the last 2500 Base blocks", n)
}
