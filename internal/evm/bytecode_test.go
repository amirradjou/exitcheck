package evm

import (
	"fmt"
	"strings"
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

// solcTrailer is real solc metadata: {"ipfs": <34 bytes>, "solc": 0.8.24}, followed by its length.
func solcTrailer(hash string) string {
	meta := "a2" + "6469706673" + "5822" + hash + "64736f6c6343" + "000818"
	return meta + fmt.Sprintf("%04x", len(meta)/2)
}

func TestAnalyzeIgnoresMetadata(t *testing.T) {
	// The ipfs hash contains 0xff and 0xf4 bytes, which must not count as SELFDESTRUCT or DELEGATECALL.
	hash := "1220" + strings.Repeat("ff", 16) + strings.Repeat("f4", 16)
	if c := Analyze(common.FromHex("6000" + solcTrailer(hash))); c.Selfdestruct || c.Delegatecall {
		t.Errorf("metadata bytes read as opcodes: %+v", c)
	}
	if !Analyze(common.FromHex("6000ff")).Selfdestruct {
		t.Error("real SELFDESTRUCT missed")
	}
}

func TestOnlyCompleteSolcMetadataIsSkipped(t *testing.T) {
	// Starts like solc's map ("ipfs" key) but the value is code: SELFDESTRUCT must be seen.
	fake := "a2" + "6469706673" + "5b" + "ff" + "00"
	if !Analyze(common.FromHex("6000" + fake + fmt.Sprintf("%04x", len(fake)/2))).Selfdestruct {
		t.Error("opcode hidden in a trailer that only starts like metadata")
	}
	hash := "1220" + strings.Repeat("ab", 32)
	if !solcMetadata(common.FromHex(solcTrailer(hash))[:len(common.FromHex(solcTrailer(hash)))-2]) {
		t.Error("real solc metadata rejected")
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

func TestFakeMetadataCannotHideSelectors(t *testing.T) {
	// A "trailer" that does not start like solc's metadata map is code, and is scanned.
	hidden := "63" + "40c10f19"                                    // PUSH4 mint(address,uint256)
	code := common.FromHex("6000" + "a2" + "5b" + hidden + "0007") // map header, JUMPDEST, PUSH4 …
	if !Analyze(code).Has(Selector{0x40, 0xc1, 0x0f, 0x19}) {
		t.Error("selector inside a fake metadata trailer was hidden")
	}
	// Even inside a well-formed trailer, selectors are still collected.
	code = common.FromHex("6000" + "a264697066735822" + hidden + "000d")
	if !Analyze(code).Has(Selector{0x40, 0xc1, 0x0f, 0x19}) {
		t.Error("selector inside a metadata-shaped trailer was hidden")
	}
}
