package facts

import (
	"context"
	"encoding/hex"
	"math/big"
	"slices"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/evm"
)

const tokenSource = `pragma solidity ^0.8.0;

interface IRouter {
    function swapExactTokensForETHSupportingFeeOnTransferTokens(uint a, uint b, address[] calldata p, address to, uint d) external;
}

library SafeFee {
    function addFee(uint a) external pure returns (uint) { return a; }
}

contract Token {
    struct Fees { uint buy; uint sell; }
    // function setFee(uint256 f) external onlyOwner {}   commented out: not a power
    /* function mint(address to, uint256 amount) external onlyOwner {} */
    string constant NOTE = "function blacklist(address who) external";

    function setBots(address[] memory bots_) external onlyOwner {}
    function updateSellFees(uint _a, uint256 _b) external onlyOwner {}
    function getBlackListStatus(address _maker) external constant returns (bool) {}
    function isBot(address a) public view returns (bool) {}
    function _setTax(uint t) internal {}
    function setFeeStruct(Fees calldata f) external onlyOwner {}
    function Approve(address[] memory list) external {}
}
`

func TestScanSourceFindsLiveFunctionsOnly(t *testing.T) {
	got := scanSource(map[string]string{"src/Token.sol": tokenSource})
	var sigs []string
	for _, p := range got {
		sigs = append(sigs, p.Kind+" "+p.Function+" "+p.Where)
	}
	want := []string{
		"blacklist setBots(address[]) Token.sol:17",
		"fees updateSellFees(uint256,uint256) Token.sol:18",
	}
	if !slices.Equal(sigs, want) {
		t.Errorf("scanSource =\n  %q\nwant\n  %q", sigs, want)
	}
}

func TestAbiSignature(t *testing.T) {
	cases := []struct {
		name, params, want string
		ok                 bool
	}{
		{"f", "", "f()", true},
		{"f", "uint a, int b", "f(uint256,int256)", true},
		{"f", "uint[] memory a, uint8[2] calldata b", "f(uint256[],uint8[2])", true},
		{"f", "address payable to, bytes32 h, string memory s", "f(address,bytes32,string)", true},
		{"f", "Fees calldata f", "", false},
		{"f", "IERC20 token", "", false},
	}
	for _, c := range cases {
		got, ok := abiSignature(c.name, c.params)
		if got != c.want || ok != c.ok {
			t.Errorf("abiSignature(%q) = %q, %v; want %q, %v", c.params, got, ok, c.want, c.ok)
		}
	}
}

func TestProbeCalldata(t *testing.T) {
	data, ok := probeCalldata("blacklist(address,bool)", probeArgs{})
	if !ok {
		t.Fatal("probeCalldata failed")
	}
	want := "404e5129" + "000000000000000000000000" + hex.EncodeToString(probeTarget[:]) +
		"0000000000000000000000000000000000000000000000000000000000000001"
	if hex.EncodeToString(data) != want {
		t.Errorf("calldata = %x\nwant       %s", data, want)
	}
	// Dynamic arrays and small integer types encode too.
	for _, sig := range []string{"setBots(address[])", "setFee(uint8)", "f(bytes32,string,bytes)"} {
		if _, ok := probeCalldata(sig, probeArgs{flip: true, large: big.NewInt(1e18)}); !ok {
			t.Errorf("probeCalldata(%s) failed", sig)
		}
	}
}

func TestFindPowersMergesBytecodeAndSource(t *testing.T) {
	code := evm.Code{Selectors: map[evm.Selector]bool{
		selectorOf("mint(address,uint256)"):              true,
		selectorOf("setBots(address[])"):                 true,
		selectorOf("updateSellFees(uint256,uint256)"):    true,
		selectorOf("transfer(address,uint256)"):          true,
		selectorOf("swapExactTokensForETHSupportingFee"): true,
	}}
	src := map[string]string{"Token.sol": tokenSource}
	got := findPowers(code, src)
	var sigs []string
	for _, p := range got {
		sigs = append(sigs, p.Function+"@"+p.Where)
	}
	// Report order follows Kinds: mint before blacklist before fees. setBots is a known signature found in the
	// bytecode and also in the source, so the source line wins.
	want := []string{"mint(address,uint256)@bytecode", "setBots(address[])@Token.sol:17", "updateSellFees(uint256,uint256)@Token.sol:18"}
	if !slices.Equal(sigs, want) {
		t.Errorf("findPowers = %q, want %q", sigs, want)
	}
}

type fakeSim map[string][]chain.SimResult // keyed by caller address hex

// Simulate answers the two probe calls (positions 2 and 3) from the map; the supply and balance reads around them
// return a fixed value unless the entry has a third element, which then stands for the reads after the calls.
func (f fakeSim) Simulate(_ context.Context, seqs [][]ethereum.CallMsg) ([][]chain.SimResult, error) {
	out := make([][]chain.SimResult, len(seqs))
	read := chain.SimResult{OK: true, ReturnData: []byte{1}}
	for i, s := range seqs {
		r := f[s[2].From.Hex()]
		after := read
		if len(r) > 2 {
			after = r[2]
		}
		out[i] = []chain.SimResult{read, read, r[0], r[1], after, after}
	}
	return out, nil
}

func TestProbePowersTellsEffectFromSilentAcceptance(t *testing.T) {
	owner := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	deployer := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	callers := []caller{{"owner", owner}, {"deployer", deployer}, {"anyone", probeCaller}}

	sim := fakeSim{
		owner.Hex():       {{OK: true, GasUsed: 48_000}, {OK: true, GasUsed: 26_000}}, // wrote a fresh slot
		deployer.Hex():    {{OK: true, GasUsed: 30_000}, {OK: false}},                 // second call reverts: state changed
		probeCaller.Hex(): {{OK: true, GasUsed: 25_723}, {OK: true, GasUsed: 25_723}}, // same gas twice: no effect
	}
	powers := []Power{{Kind: KindBlacklist, Function: "blacklist(address,bool)"}, {Kind: KindUpgrade, Function: "upgradeTo(address)"}}
	if err := probePowers(context.Background(), sim, common.Address{1}, big.NewInt(1e18), powers, callers); err != nil {
		t.Fatal(err)
	}
	if p := powers[0]; !p.Probed || !slices.Equal(p.CallableBy, []string{"owner", "deployer"}) || !slices.Equal(p.NoEffectFor, []string{"anyone"}) {
		t.Errorf("blacklist probe = %+v", p)
	}
	if powers[1].Probed {
		t.Error("upgrade functions must not be probed")
	}

	// A mint repeats with the same gas (it adds to balances again), but moves the supply: that is an effect.
	sim[probeCaller.Hex()] = []chain.SimResult{{OK: true, GasUsed: 50_000}, {OK: true, GasUsed: 50_000}, {OK: true, ReturnData: []byte{2}}}
	powers = []Power{{Kind: KindMint, Function: "mint(address,uint256)"}}
	if err := probePowers(context.Background(), sim, common.Address{1}, big.NewInt(1e18), powers, callers); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(powers[0].CallableBy, []string{"anyone"}) {
		t.Errorf("a mint anyone can call should read [anyone], got %v", powers[0].CallableBy)
	}

	// An event is an effect too (USDT's issue() emits Issue).
	sim[probeCaller.Hex()] = []chain.SimResult{{OK: true, GasUsed: 30_000, Logs: 1}, {OK: true, GasUsed: 30_000}}
	powers = []Power{{Kind: KindMint, Function: "issue(uint256)"}}
	if err := probePowers(context.Background(), sim, common.Address{1}, big.NewInt(1e18), powers, callers); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(powers[0].CallableBy, []string{"anyone"}) {
		t.Errorf("a call that emits an event has an effect, got %+v", powers[0])
	}
}

func TestProbeArgsFlipSwitchesAndClampAmounts(t *testing.T) {
	data, ok := probeCalldata("setTradingEnabled(bool)", probeArgs{flip: true, large: big.NewInt(1e18)})
	if !ok || data[len(data)-1] != 0 {
		t.Errorf("flipped bool argument should be false: %x", data)
	}
	data, ok = probeCalldata("setFee(uint8)", probeArgs{flip: true, large: big.NewInt(1e18)})
	if !ok || data[len(data)-1] != 0xff {
		t.Errorf("large uint8 should clamp to 255: %x", data)
	}
	if hasSwitches("mint(address)") || !hasSwitches("mint(address,uint256)") {
		t.Error("hasSwitches should look at parameters, not the name")
	}
}

// A setter whose harmless probe is a no-op (trading already on) but whose flipped probe changes state is live.
func TestProbePowersCountsEitherArgumentSet(t *testing.T) {
	owner := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	on, _ := probeCalldata("setTradingEnabled(bool)", probeArgs{})
	sim := simByData{
		string(on): {{OK: true, GasUsed: 26_000}, {OK: true, GasUsed: 26_000}},
	}
	powers := []Power{{Kind: KindTrading, Function: "setTradingEnabled(bool)"}}
	callers := []caller{{"owner", owner}}
	if err := probePowers(context.Background(), sim, common.Address{1}, big.NewInt(1e18), powers, callers); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(powers[0].CallableBy, []string{"owner"}) || len(powers[0].NoEffectFor) != 0 {
		t.Errorf("setTradingEnabled(false) changes state, so the owner can use it: %+v", powers[0])
	}
}

// simByData answers by calldata; any calldata it does not list writes state (the repeat costs less after a refund).
type simByData map[string][]chain.SimResult

func (f simByData) Simulate(_ context.Context, seqs [][]ethereum.CallMsg) ([][]chain.SimResult, error) {
	out := make([][]chain.SimResult, len(seqs))
	read := chain.SimResult{OK: true, ReturnData: []byte{1}}
	for i, s := range seqs {
		r, ok := f[string(s[2].Data)]
		if !ok {
			r = []chain.SimResult{{OK: true, GasUsed: 24_000}, {OK: true, GasUsed: 26_000}}
		}
		out[i] = []chain.SimResult{read, read, r[0], r[1], read, read}
	}
	return out, nil
}
