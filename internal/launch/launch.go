// Package launch describes token launches, the events the live watch streams: a pool created against a quote token
// (WETH, USDC, USDT) that has received its first liquidity. It decodes the pool-creation logs of the DEX factories
// rugcheck knows.
package launch

import (
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/amirradjou/rugcheck/internal/chain"
)

// Topics of the pool-creation events.
var (
	// PairCreated(address indexed token0, address indexed token1, address pair, uint256): Uniswap v2.
	TopicV2 = common.HexToHash("0x0d3648bd0f6ba80134a33ba9275ac585d9d315f0ad8355cddefde31afa28d0e9")
	// PoolCreated(address indexed token0, address indexed token1, uint24 indexed fee, int24 tickSpacing,
	// address pool): Uniswap v3.
	TopicV3 = common.HexToHash("0x783cca1c0412dd0d695e784568c96da2e9c22ff989357a2e8b1d9b2b4e6b7118")
	// PoolCreated(address indexed token0, address indexed token1, bool indexed stable, address pool, uint256):
	// Aerodrome.
	TopicAerodrome = common.HexToHash("0x2128d88d14c80cb081c1252a5acff7a264671bf199ce226b53788fb26065005e")
	// Mint events: Uniswap v2 and Aerodrome pairs (same signature), and Uniswap v3 pools.
	TopicMintV2 = common.HexToHash("0x4c209b5fc8ad50758f13e2e1088ba56a560dff690a1c6fef26394f4c03821c4f")
	TopicMintV3 = common.HexToHash("0x7a53080ba414158be7ec69b987b5fb7d07dee101fe85488f0853ae16239d0bde")
)

// Pool is a pool created against a quote token.
type Pool struct {
	Chain string         `json:"chain"`
	DEX   string         `json:"dex"` // uniswap-v2, uniswap-v3, aerodrome
	Pool  common.Address `json:"pool"`
	Token common.Address `json:"token"`
	Quote common.Address `json:"quote"`
	// Block and Tx are where the pool was created.
	Block uint64      `json:"created_block"`
	Tx    common.Hash `json:"created_tx"`
	// SeenAt is when the watcher saw the pool created.
	SeenAt time.Time `json:"seen_at"`
}

// Launch is a pool that received its first liquidity: the moment a token can be traded.
type Launch struct {
	Pool
	// FundedBlock is the block of the pool's first liquidity, and FundedAt its timestamp; lag is measured from it.
	FundedBlock uint64    `json:"funded_block"`
	FundedAt    time.Time `json:"funded_at"`
	// DetectedAt is when the watcher saw the funding.
	DetectedAt time.Time `json:"detected_at"`
}

// ID identifies a launch: one per pool.
func (l Launch) ID() string {
	return l.Chain + ":" + strings.ToLower(l.Pool.Pool.Hex())
}

// Key is the Kafka partition key: launches of one token (several pools) land on one partition, in order.
func (l Launch) Key() []byte {
	return []byte(l.Chain + ":" + strings.ToLower(l.Token.Hex()))
}

// Factories are the factory contracts to watch on a network, and the DEX each one belongs to.
func Factories(net chain.Network) map[common.Address]string {
	f := map[common.Address]string{}
	for _, d := range net.V2 {
		f[d.Factory] = d.Name
	}
	if net.V3Factory != (common.Address{}) {
		f[net.V3Factory] = "uniswap-v3"
	}
	if net.Aerodrome != (common.Address{}) {
		f[net.Aerodrome] = "aerodrome"
	}
	return f
}

// Decode reads a pool-creation log. ok is false for logs that are not one, and for pools that do not pair exactly
// one quote token with another token (two quote tokens, or none, is not a token launch).
func Decode(net chain.Network, l types.Log) (p Pool, ok bool, err error) {
	dex, known := Factories(net)[l.Address]
	if !known || len(l.Topics) < 3 {
		return p, false, nil
	}
	var pool common.Address
	switch {
	case l.Topics[0] == TopicV2 && dex != "uniswap-v3" && dex != "aerodrome", l.Topics[0] == TopicAerodrome && dex == "aerodrome":
		if len(l.Data) < 32 {
			return p, false, fmt.Errorf("%s log %s#%d: data too short", dex, l.TxHash, l.Index)
		}
		pool = common.BytesToAddress(l.Data[:32])
	case l.Topics[0] == TopicV3 && dex == "uniswap-v3":
		if len(l.Data) < 64 {
			return p, false, fmt.Errorf("%s log %s#%d: data too short", dex, l.TxHash, l.Index)
		}
		pool = common.BytesToAddress(l.Data[32:64])
	default:
		return p, false, nil
	}
	t0, t1 := common.BytesToAddress(l.Topics[1].Bytes()), common.BytesToAddress(l.Topics[2].Bytes())
	q0, q1 := isQuote(net, t0), isQuote(net, t1)
	if q0 == q1 {
		return p, false, nil
	}
	token, quote := t0, t1
	if q0 {
		token, quote = t1, t0
	}
	return Pool{Chain: net.Name, DEX: dex, Pool: pool, Token: token, Quote: quote, Block: l.BlockNumber, Tx: l.TxHash}, true, nil
}

func isQuote(net chain.Network, a common.Address) bool {
	for _, q := range net.Quotes {
		if q.Address == a {
			return true
		}
	}
	return false
}
