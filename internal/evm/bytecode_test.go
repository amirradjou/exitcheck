package evm

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestAnalyzeFindsDispatcherSelectorsAndSkipsPushData(t *testing.T) {
	code := common.FromHex(
		"63a9059cbb" + // PUSH4 transfer(address,uint256)
			"6240c10f" + // PUSH3 0x40c10f → selector 0x0040c10f
			"7f" + "63deadbeef" + "f4ff" + "00000000000000000000000000000000000000000000000000" + // PUSH32 whose data looks like PUSH4 and DELEGATECALL
			"f4" + // DELEGATECALL
			"00")
	c := Analyze(code)
	if !c.Has(Selector{0xa9, 0x05, 0x9c, 0xbb}) {
		t.Error("PUSH4 selector missing")
	}
	if !c.Has(Selector{0x00, 0x40, 0xc1, 0x0f}) {
		t.Error("PUSH3 selector missing")
	}
	if c.Has(Selector{0xde, 0xad, 0xbe, 0xef}) {
		t.Error("read a selector out of PUSH32 data")
	}
	if !c.Delegatecall || c.Selfdestruct {
		t.Errorf("opcodes: delegatecall=%v selfdestruct=%v, want true false", c.Delegatecall, c.Selfdestruct)
	}
}

func TestAnalyzeIgnoresMetadata(t *testing.T) {
	// a2 64 'ipfs' ... 0x00 0x06-length tail; contains 0xff which must not count as SELFDESTRUCT.
	meta := "a264697066735822ff" // 9 bytes
	code := common.FromHex("6000" + meta + "0009")
	if Analyze(code).Selfdestruct {
		t.Error("metadata byte 0xff read as SELFDESTRUCT")
	}
	if !Analyze(common.FromHex("6000ff")).Selfdestruct {
		t.Error("real SELFDESTRUCT missed")
	}
}

func TestMinimalProxyTarget(t *testing.T) {
	impl := common.HexToAddress("0xbebebebebebebebebebebebebebebebebebebebe")
	code := common.FromHex("363d3d373d3d3d363d73" + impl.Hex()[2:] + "5af43d82803e903d91602b57fd5bf3")
	got, ok := MinimalProxyTarget(code)
	if !ok || got != impl {
		t.Fatalf("MinimalProxyTarget = %s, %v; want %s", got, ok, impl)
	}
	if _, ok := MinimalProxyTarget(code[:len(code)-1]); ok {
		t.Error("truncated code read as a minimal proxy")
	}
}

func TestDelegatedWalletIsNotAContract(t *testing.T) {
	code := common.FromHex("0xef010063c0c19a282a1b52b07dd5a65b58948a07dae32b")
	if d, ok := DelegatedTo(code); !ok || d != common.HexToAddress("0x63c0c19a282a1b52b07dd5a65b58948a07dae32b") {
		t.Fatalf("DelegatedTo = %s, %v", d, ok)
	}
	if IsContract(code) || IsContract(nil) || !IsContract(common.FromHex("6000")) {
		t.Error("IsContract misclassifies wallets or contracts")
	}
}
