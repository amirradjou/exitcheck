package sim

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/facts"
)

func taxed(t float64) *Step { return &Step{OK: true, Tax: &t} }

func TestVerdict(t *testing.T) {
	reverted := &Step{Error: "TransferHelper: TRANSFER_FROM_FAILED"}
	cases := []struct {
		name string
		r    Result
		want string
	}{
		{"clean", Result{Buy: taxed(0), Transfer: taxed(0), Sell: taxed(0)}, OK},
		{"sell reverts, half too", Result{Buy: taxed(0), Transfer: taxed(0), Sell: reverted, SellHalf: reverted}, Honeypot},
		{"only half sells", Result{Buy: taxed(0), Transfer: taxed(0), Sell: reverted, SellHalf: taxed(0)}, SellLimited},
		{"sale keeps most of the value", Result{Buy: taxed(0), Transfer: taxed(0), Sell: taxed(0.99)}, Honeypot},
		{"wallet transfers blocked", Result{Buy: taxed(0), Transfer: reverted, Sell: taxed(0)}, TransferBlocked},
		{"20% sell tax", Result{Buy: taxed(0), Transfer: taxed(0), Sell: taxed(0.2)}, HighTax},
		{"5% buy and sell tax", Result{Buy: taxed(0.05), Transfer: taxed(0), Sell: taxed(0.05)}, OK},
	}
	for _, c := range cases {
		r := c.r
		verdict(&r)
		if r.Verdict != c.want {
			t.Errorf("%s: verdict %s, want %s (reasons %q)", c.name, r.Verdict, c.want, r.Reasons)
		}
		if got := r.Flagged(); got != (c.want != OK) {
			t.Errorf("%s: Flagged() = %v", c.name, got)
		}
	}
}

func TestTaxOf(t *testing.T) {
	if got := *taxOf(big.NewInt(997), big.NewInt(1000)); got != 0 {
		t.Errorf("0.3%% rounding noise read as tax %v", got)
	}
	if got := *taxOf(big.NewInt(900), big.NewInt(1000)); got < 0.0999 || got > 0.1001 {
		t.Errorf("tax = %v, want 0.1", got)
	}
	if got := *taxOf(big.NewInt(1100), big.NewInt(1000)); got != 0 {
		t.Errorf("receiving more than quoted is no tax, got %v", got)
	}
	if taxOf(big.NewInt(1), new(big.Int)) != nil {
		t.Error("a zero quote has no tax")
	}
}

func TestPickPoolPrefersFeeTolerantRouters(t *testing.T) {
	v3 := facts.Pool{DEX: "uniswap-v3", Quote: "WETH", USD: 700_000}
	v2Deep := facts.Pool{DEX: "uniswap-v2", Quote: "WETH", USD: 100_000}
	v2Dust := facts.Pool{DEX: "uniswap-v2", Quote: "WETH", USD: 300}
	usdc := facts.Pool{DEX: "aerodrome", Quote: "USDC", USD: 3_000_000}
	v2Twentieth := facts.Pool{DEX: "uniswap-v2", Quote: "WETH", USD: 200_000}
	if p, _ := pickPool([]facts.Pool{v3, v2Deep}); p != v2Deep {
		t.Errorf("picked %+v, want the v2 pool (handles fee-on-transfer tokens)", p)
	}
	if p, _ := pickPool([]facts.Pool{v3, v2Dust}); p != v3 {
		t.Errorf("picked %+v, want the deep v3 pool over a dust v2 pool", p)
	}
	if p, _ := pickPool([]facts.Pool{usdc, v2Twentieth}); p != v2Twentieth {
		t.Errorf("picked %+v, want the WETH v2 pool (a twentieth of the deepest is enough)", p)
	}
	if _, ok := pickPool([]facts.Pool{{DEX: "uniswap-v2", USD: 0.001}}); ok {
		t.Error("an empty pool was picked")
	}
	drained := facts.Pool{DEX: "uniswap-v2", Quote: "WETH", USD: 0.1}
	if p, ok := pickPool([]facts.Pool{drained}); !ok || p != drained {
		t.Error("a nearly drained honeypot pool should still be traded")
	}
}

func TestV3Path(t *testing.T) {
	weth := common.HexToAddress("0x4200000000000000000000000000000000000006")
	tok := common.HexToAddress("0x532f27101965dd16442E59d40670FaF5eBB142E4")
	got := hex.EncodeToString(v3Path(weth, uint32(10000), tok))
	want := "4200000000000000000000000000000000000006" + "002710" + "532f27101965dd16442e59d40670faf5ebb142e4"
	if got != want {
		t.Errorf("path = %s\nwant   %s", got, want)
	}
	back := hex.EncodeToString(v3Path(reversePath([]any{weth, uint32(10000), tok})...))
	if back != "532f27101965dd16442e59d40670faf5ebb142e4"+"002710"+"4200000000000000000000000000000000000006" {
		t.Errorf("reversed path = %s", back)
	}
}

func TestForkFailuresAreNotVerdicts(t *testing.T) {
	if !isForkFailure(errors.New(`Fork Error: Transport(HttpError(HttpError { status: 403, body: "Archive requests require a personal token" }))`)) {
		t.Error("anvil transport failure not recognised")
	}
	if isForkFailure(errors.New("execution reverted: TransferHelper: TRANSFER_FROM_FAILED")) {
		t.Error("a revert read as a fork failure")
	}
}

func TestTimeoutsAreNotVerdicts(t *testing.T) {
	// Sentient (SENT), 2026-10-30: the budget ran out while the simulation approved the sale, and the timeout came
	// back as "approve reverted: … context deadline exceeded", which read as a honeypot.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := unreliable(ctx, nil); err == nil || !strings.Contains(err.Error(), "ran out of time") {
		t.Errorf("a simulation cut off by its deadline: %v, want an error", err)
	}
	if err := unreliable(context.Background(), errors.New("Fork Error: Transport(…)")); err == nil {
		t.Error("a broken fork was not an error")
	}
	if err := unreliable(context.Background(), nil); err != nil {
		t.Errorf("a finished simulation: %v", err)
	}
}

// dataErr is how go-ethereum's RPC client returns a revert that carries data.
type dataErr struct{ data string }

func (e dataErr) Error() string          { return "execution reverted" }
func (e dataErr) ErrorData() interface{} { return e.data }

func TestOnlyEVMFailuresAreTheTokens(t *testing.T) {
	for _, c := range []struct {
		err   error
		token bool
	}{
		{dataErr{"0x08c379a0"}, true},
		{errors.New("execution reverted"), true},
		{errors.New("execution reverted: TransferHelper: TRANSFER_FROM_FAILED"), true},
		{errors.New("EVM error OutOfGas"), true},
		{errors.New("EVM error InvalidFEOpcode"), true},
		{errors.New(`Post "http://127.0.0.1:42681": context deadline exceeded`), false},
		{errors.New("dial tcp 127.0.0.1:42681: connect: connection refused"), false},
		{errors.New("transaction 0xabc was not mined"), false},
		{errors.New("EVM error Database(Fork Error: Transport(…))"), false}, // anvil could not fetch state
	} {
		f := &fork{}
		f.blame(c.err)
		if got := f.broken == nil; got != c.token {
			t.Errorf("%q: counted as the token's = %v, want %v", c.err, got, c.token)
		}
	}
	f := &fork{}
	f.blame(nil)
	if f.broken != nil {
		t.Error("no error was blamed on the simulation")
	}
}

// Live: forks Ethereum through the default public RPCs and needs anvil; run with EXITCHECK_LIVE=1.
func TestLiveSimulation(t *testing.T) {
	if os.Getenv("EXITCHECK_LIVE") == "" {
		t.Skip("set EXITCHECK_LIVE=1 to fork the chain")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := chain.Dial(ctx, chain.Ethereum)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, tc := range []struct {
		name, addr, want string
	}{
		{"PEPE", "0x6982508145454Ce325dDbE47a25d4ec3d2311933", OK},
		{"ASN (100% sell tax)", "0xBe2ab177599C6381C70725FEEE6663F9bCfF2197", Honeypot},
	} {
		r, err := Run(ctx, c, common.HexToAddress(tc.addr), Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if r.Verdict != tc.want {
			t.Errorf("%s: verdict %s, want %s (%q)", tc.name, r.Verdict, tc.want, r.Reasons)
		}
	}
}
