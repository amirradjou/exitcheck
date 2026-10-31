package outcome

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/amirradjou/rugcheck/internal/launch"
)

var (
	zero     = common.Address{}
	pair     = common.HexToAddress("0x00000000000000000000000000000000000000b0")
	deployer = common.HexToAddress("0x00000000000000000000000000000000000000d0")
	feeTo    = common.HexToAddress("0x00000000000000000000000000000000000000fe")
	// The token sorts after WETH here, so it is token1.
	weth  = common.HexToAddress("0x1000000000000000000000000000000000000000")
	token = common.HexToAddress("0x2000000000000000000000000000000000000000")
)

func word(v *big.Int) []byte { return common.LeftPadBytes(v.Bytes(), 32) }

func signed(v int64) []byte {
	if v >= 0 {
		return word(big.NewInt(v))
	}
	// two's complement in 256 bits
	m := new(big.Int).Lsh(big.NewInt(1), 256)
	return word(m.Add(m, big.NewInt(v)))
}

type logs struct {
	block uint64
	out   []types.Log
}

func (b *logs) at(block uint64) *logs { b.block = block; return b }

func (b *logs) add(topics []common.Hash, data ...[]byte) *logs {
	var d []byte
	for _, w := range data {
		d = append(d, w...)
	}
	b.out = append(b.out, types.Log{Address: pair, Topics: topics, Data: d, BlockNumber: b.block, Index: uint(len(b.out))})
	return b
}

func (b *logs) transfer(from, to common.Address, v int64) *logs {
	return b.add([]common.Hash{TopicTransfer, common.BytesToHash(from.Bytes()), common.BytesToHash(to.Bytes())}, word(big.NewInt(v)))
}

// swapV2 records a v2 swap; amounts are (amount0In, amount1In, amount0Out, amount1Out), token1 being the token.
func (b *logs) swapV2(a0In, a1In, a0Out, a1Out int64) *logs {
	return b.add([]common.Hash{TopicSwapV2, {}, {}}, word(big.NewInt(a0In)), word(big.NewInt(a1In)), word(big.NewInt(a0Out)), word(big.NewInt(a1Out)))
}

// mintV2 is a first deposit: the permanent minimum to the zero address, the rest to the depositor.
func (b *logs) mintV2(to common.Address, lp int64) *logs {
	return b.transfer(zero, zero, 1000).transfer(zero, to, lp)
}

// burnV2 is a removal: the LP goes to the pair, which burns it.
func (b *logs) burnV2(from common.Address, lp int64) *logs {
	return b.transfer(from, pair, lp).transfer(pair, zero, lp)
}

var v2 = Pool{DEX: "uniswap-v2", Token: token, Quote: weth, FeeTo: feeTo}

func TestTopicsMatchTheChain(t *testing.T) {
	for name, c := range map[string]struct{ got, want common.Hash }{
		"transfer": {TopicTransfer, common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")},
		"v2 swap":  {TopicSwapV2, common.HexToHash("0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822")},
		"v3 swap":  {TopicSwapV3, common.HexToHash("0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67")},
		"v3 mint":  {TopicMintV3, launch.TopicMintV3},
		"v3 burn":  {TopicBurnV3, common.HexToHash("0x0c396cd989a39f4459b5fa1aed6a9a8dcdbc45908acfd67e028cd568da98982c")},
	} {
		if c.got != c.want {
			t.Errorf("%s topic = %s, want %s", name, c.got, c.want)
		}
	}
}

func TestV2RugIsPulledAfterTrading(t *testing.T) {
	b := (&logs{}).at(10).mintV2(deployer, 1_000_000).
		at(11).swapV2(5, 0, 0, 900).swapV2(3, 0, 0, 500). // two buys: WETH (token0) in
		at(12).swapV2(0, 400, 2, 0).                      // a sell: the token in
		transfer(zero, feeTo, 300).                       // protocol fee mint
		at(20).burnV2(deployer, 1_000_000).
		at(21).swapV2(0, 10, 0, 0) // after the pull: not counted
	o := Read(v2, b.out)
	if !o.Pulled || o.PulledBlock != 20 || o.Buys != 2 || o.Sells != 1 || o.Bait {
		t.Errorf("Read = %+v, want pulled at 20 after 2 buys and 1 sell", o)
	}
}

func TestV2PoolInPlace(t *testing.T) {
	b := (&logs{}).at(10).mintV2(deployer, 1_000_000).at(11).swapV2(5, 0, 0, 900).
		at(15).burnV2(deployer, 500_000) // half removed: still trading
	if o := Read(v2, b.out); o.Pulled || o.Buys != 1 {
		t.Errorf("Read = %+v, want in place with 1 buy", o)
	}
}

func TestV2PulledBeforeAnyTradeIsBait(t *testing.T) {
	b := (&logs{}).at(10).mintV2(deployer, 1_000_000).at(12).burnV2(deployer, 1_000_000)
	if o := Read(v2, b.out); !o.Pulled || !o.Bait {
		t.Errorf("Read = %+v, want bait", o)
	}
}

func TestV2FeeShareLeftAfterThePullStillCountsAsPulled(t *testing.T) {
	// DREAM's shape: trading minted LP to the fee collector, which keeps it after the deployer leaves.
	b := (&logs{}).at(10).mintV2(deployer, 1_000_000).at(11).swapV2(5, 0, 0, 900).
		transfer(zero, feeTo, 50_000).at(13).burnV2(deployer, 1_000_000)
	if o := Read(v2, b.out); !o.Pulled || o.PulledBlock != 13 {
		t.Errorf("Read = %+v, want pulled at 13", o)
	}
}

func TestV3PoolPulledWhenItsPositionsAreBurned(t *testing.T) {
	v3 := Pool{DEX: "uniswap-v3", Token: token, Quote: weth}
	mint := func(amt int64) [][]byte { // sender, amount, amount0, amount1
		return [][]byte{word(new(big.Int).SetBytes(deployer.Bytes())), word(big.NewInt(amt)), word(big.NewInt(0)), word(big.NewInt(1))}
	}
	b := (&logs{}).at(10).add([]common.Hash{TopicMintV3, {}, {}, {}}, mint(600)...).add([]common.Hash{TopicMintV3, {}, {}, {}}, mint(400)...).
		at(11).add([]common.Hash{TopicSwapV3, {}, {}}, signed(70), signed(-900), word(big.NewInt(1)), word(big.NewInt(1)), word(big.NewInt(0))). // buy: WETH (token0) in
		at(12).add([]common.Hash{TopicSwapV3, {}, {}}, signed(-20), signed(300), word(big.NewInt(1)), word(big.NewInt(1)), word(big.NewInt(0))). // sell
		at(13).add([]common.Hash{TopicBurnV3, {}, {}, {}}, word(big.NewInt(600)), word(big.NewInt(0)), word(big.NewInt(0))).                     // 40% left
		at(14).add([]common.Hash{TopicBurnV3, {}, {}, {}}, word(big.NewInt(395)), word(big.NewInt(0)), word(big.NewInt(0)))                      // 0.5% left
	if o := Read(v3, b.out); !o.Pulled || o.PulledBlock != 14 || o.Buys != 1 || o.Sells != 1 {
		t.Errorf("Read = %+v, want pulled at 14 after 1 buy and 1 sell", o)
	}
}

func TestLogsAreReadInChainOrder(t *testing.T) {
	b := (&logs{}).at(10).mintV2(deployer, 1_000_000).at(11).swapV2(5, 0, 0, 900).at(20).burnV2(deployer, 1_000_000)
	reversed := make([]types.Log, len(b.out))
	for i, l := range b.out {
		reversed[len(b.out)-1-i] = l
	}
	if o := Read(v2, reversed); !o.Pulled || o.Buys != 1 || o.Bait {
		t.Errorf("Read of shuffled logs = %+v", o)
	}
}
