package facts

import (
	"bytes"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/amirradjou/exitcheck/internal/chain"
)

func TestDecodeTransfer(t *testing.T) {
	from, to := common.HexToAddress("0x01"), common.HexToAddress("0x02")
	v := common.BigToHash(big.NewInt(42))
	indexed := types.Log{Topics: []common.Hash{transferTopic, common.BytesToHash(from[:]), common.BytesToHash(to[:])}, Data: v[:]}
	if f, tt, val, ok := decodeTransfer(indexed); !ok || f != from || tt != to || val.Int64() != 42 {
		t.Errorf("indexed transfer = %s %s %v %v", f, tt, val, ok)
	}
	var data []byte
	data = append(data, common.BytesToHash(from[:]).Bytes()...)
	data = append(data, common.BytesToHash(to[:]).Bytes()...)
	data = append(data, v[:]...)
	old := types.Log{Topics: []common.Hash{transferTopic}, Data: data}
	if f, tt, val, ok := decodeTransfer(old); !ok || f != from || tt != to || val.Int64() != 42 {
		t.Errorf("unindexed transfer = %s %s %v %v", f, tt, val, ok)
	}
	nft := types.Log{Topics: []common.Hash{transferTopic, {}, {}, v}}
	if _, _, _, ok := decodeTransfer(nft); ok {
		t.Error("ERC-721 transfer decoded as ERC-20")
	}
}

func TestDecodeSettings(t *testing.T) {
	gs := []getter{
		{KindFees, "sellTax()", false, false},
		{KindTrading, "tradingOpen()", true, false},
		{KindLimits, "maxWallet()", false, true},
		{KindFees, "buyTax()", false, false},
	}
	res := []chain.Result{
		{OK: true, Data: common.BigToHash(big.NewInt(100)).Bytes()},
		{OK: true, Data: common.BigToHash(big.NewInt(1)).Bytes()},
		{OK: true, Data: common.BigToHash(big.NewInt(20)).Bytes()},
		{OK: false},
	}
	got := decodeSettings(gs, res, big.NewInt(1000))
	if len(got) != 3 {
		t.Fatalf("decodeSettings = %+v, want 3 settings (the reverted getter dropped)", got)
	}
	if got[0].Value != "100" || got[1].Value != "true" || got[2].SupplyShare == nil || *got[2].SupplyShare != 0.02 {
		t.Errorf("decodeSettings = %+v", got)
	}
}

func TestRenderReportsWhatMatters(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	owner := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	f := &Facts{
		Chain: "ethereum", Address: common.HexToAddress("0x01"), Block: 1, CheckedAt: now,
		Token:    Token{Name: "Scam", Symbol: "SCAM", Decimals: 18, TotalSupply: new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil)},
		Contract: Contract{Owner: &owner, OwnerStatus: "wallet"},
		Powers: []Power{{Kind: KindBlacklist, Function: "blacklist(address,bool)", Where: "bytecode", Probed: true,
			CallableBy: []string{"owner"}, NoEffectFor: []string{"anyone"}}},
		Pools:     []Pool{{DEX: "uniswap-v2", Address: common.HexToAddress("0x02"), Quote: "WETH"}},
		Liquidity: &Liquidity{Pool: common.HexToAddress("0x02"), DEX: "uniswap-v2", Pulled: true},
		Gaps:      []string{"verified source: unknown"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, f); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"SCAM (Scam) · ethereum",
		"1,000,000,000 SCAM",
		"callable by: owner; accepted without effect from: anyone",
		"LIQUIDITY WITHDRAWN",
		"NOT CHECKED\n  - verified source: unknown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestAgesAreMeasuredFromTheCheckedBlock(t *testing.T) {
	// A check run today on a fork of a block from yesterday: the pool was a minute old then.
	block := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	created := block.Add(-time.Minute)
	f := &Facts{
		Chain: "base", Address: common.HexToAddress("0x01"), Block: 1, CheckedAt: block.Add(20 * time.Hour), BlockTime: block,
		Liquidity: &Liquidity{Pool: common.HexToAddress("0x02"), DEX: "uniswap-v2", CreatedAt: &created},
	}
	if !f.AsOf().Equal(block) {
		t.Errorf("AsOf = %v, want the block's time %v", f.AsOf(), block)
	}
	var buf bytes.Buffer
	if err := Render(&buf, f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "(1 min ago)") {
		t.Errorf("pool age not measured from the block:\n%s", buf.String())
	}
	if f.BlockTime = (time.Time{}); !f.AsOf().Equal(f.CheckedAt) {
		t.Errorf("without a block time, AsOf = %v, want the check's start", f.AsOf())
	}
}

func TestLPPulledIgnoresTheProtocolFeeShare(t *testing.T) {
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	cases := []struct {
		name             string
		supply, protocol *big.Int
		want             bool
	}{
		{"only the minimum left", big.NewInt(1000), new(big.Int), true},
		// DREAM's pair: almost all remaining LP is the fee collector's.
		{"fee share and dust left", big.NewInt(511316057713226), big.NewInt(511185484987504), true},
		{"live pool with a fee share", e18, new(big.Int).Div(e18, big.NewInt(50)), false},
		{"burned LP stays in the pool", e18, new(big.Int), false},
	}
	for _, c := range cases {
		if got := lpPulled(c.supply, c.protocol); got != c.want {
			t.Errorf("%s: lpPulled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPinkLockUnlocksRoundTrip(t *testing.T) {
	lock := func(amount, unlocked, tge int64) pinkLock {
		return pinkLock{
			Id: big.NewInt(1), Token: common.HexToAddress("0x01"), Owner: common.HexToAddress("0x02"),
			Amount: big.NewInt(amount), LockDate: big.NewInt(100), TgeDate: big.NewInt(tge), TgeBps: new(big.Int),
			Cycle: new(big.Int), CycleBps: new(big.Int), UnlockedAmount: big.NewInt(unlocked), Description: "lp",
		}
	}
	data, err := pinkLockABI.Methods["getLocksForToken"].Outputs.Pack([]pinkLock{lock(10, 0, 2_000_000_000), lock(5, 5, 1_000)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := pinkLockUnlocks(data)
	if err != nil {
		t.Fatal(err)
	}
	// The second lock is fully withdrawn, so only the first one's date counts.
	if len(got) != 1 || got[0] != 2_000_000_000 {
		t.Errorf("unlocks = %v, want [2000000000]", got)
	}
}

func TestTokenNamesCannotCarryTerminalEscapes(t *testing.T) {
	data, err := erc20ABI.Methods["name"].Outputs.Pack("SAFE\x1b[2K\x1b[1A\rlegit token")
	if err != nil {
		t.Fatal(err)
	}
	got := text(chain.Result{OK: true, Data: data})
	if strings.ContainsAny(got, "\x1b\r") {
		t.Errorf("text kept control characters: %q", got)
	}
	long, _ := erc20ABI.Methods["name"].Outputs.Pack(strings.Repeat("x", 500))
	if n := len([]rune(text(chain.Result{OK: true, Data: long}))); n > maxNameRunes+1 {
		t.Errorf("name not capped: %d runes", n)
	}
	var buf bytes.Buffer
	f := &Facts{Token: Token{Symbol: "X"}, Contract: Contract{OwnerStatus: "none"}, Gaps: []string{"evil\x1b]0;title\x07gap"}}
	if err := Render(&buf, f); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(buf.String(), "\x1b\x07") {
		t.Errorf("report kept control characters: %q", buf.String())
	}
}
