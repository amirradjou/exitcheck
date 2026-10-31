package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/amirradjou/rugcheck/internal/chain"
)

func load(t *testing.T, name string) types.Log {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var l types.Log
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// The fixtures are real creation logs (eth_getLogs); the expected pools come from each factory's getPair/getPool,
// not from the log.
func TestDecodeRealCreationLogs(t *testing.T) {
	for _, c := range []struct {
		file, dex string
		net       chain.Network
		pool      string
	}{
		{"ethereum-uniswap-v2.json", "uniswap-v2", chain.Ethereum, "0x95A21519B9b639dC976C437518E22BFE1c6562c8"},
		{"ethereum-uniswap-v3.json", "uniswap-v3", chain.Ethereum, "0x3C3586B170d19094eA96eaE7b75453d9Dd374aa8"},
		{"base-aerodrome.json", "aerodrome", chain.Base, "0x533Aa7c4519518845AF7409f973F5fbec955C798"},
	} {
		p, ok, err := Decode(c.net, load(t, c.file))
		if err != nil || !ok {
			t.Fatalf("%s: ok %v, err %v", c.file, ok, err)
		}
		if p.DEX != c.dex || p.Pool != common.HexToAddress(c.pool) || p.Quote != c.net.WETH || p.Token == c.net.WETH {
			t.Errorf("%s: %+v", c.file, p)
		}
	}
}

func TestDecodeSkipsWhatIsNotALaunch(t *testing.T) {
	l := load(t, "ethereum-uniswap-v2.json")

	wrongFactory := l
	wrongFactory.Address = common.HexToAddress("0xbad")
	if _, ok, _ := Decode(chain.Ethereum, wrongFactory); ok {
		t.Error("a log from an unknown contract decoded as a launch")
	}

	quotePair := l
	quotePair.Topics = append([]common.Hash{}, l.Topics...)
	quotePair.Topics[1] = common.BytesToHash(chain.Ethereum.Quotes[1].Address.Bytes()) // USDC/WETH
	quotePair.Topics[2] = common.BytesToHash(chain.Ethereum.WETH.Bytes())
	if _, ok, _ := Decode(chain.Ethereum, quotePair); ok {
		t.Error("a pool of two quote tokens decoded as a launch")
	}

	short := l
	short.Data = short.Data[:8]
	if _, _, err := Decode(chain.Ethereum, short); err == nil {
		t.Error("truncated data accepted")
	}

	// The v2 topic from the v3 factory is not a v2 pool.
	mixed := l
	mixed.Address = chain.Ethereum.V3Factory
	if _, ok, _ := Decode(chain.Ethereum, mixed); ok {
		t.Error("a v2 event from the v3 factory decoded")
	}
}

func TestKeysGroupATokensPools(t *testing.T) {
	token := common.HexToAddress("0x1")
	a := Launch{Pool: Pool{Chain: "base", Token: token, Pool: common.HexToAddress("0xa")}}
	b := Launch{Pool: Pool{Chain: "base", Token: token, Pool: common.HexToAddress("0xb")}}
	if string(a.Key()) != string(b.Key()) || a.ID() == b.ID() {
		t.Errorf("keys %s %s, ids %s %s", a.Key(), b.Key(), a.ID(), b.ID())
	}
}
