// Package outcome reads what happened to a launch's liquidity after it was checked: whether it was pulled within a
// horizon (a day by default), when, and how much trading came first. These are M4's labels. They come from the
// pool's own logs, so they rest on what the chain did, not on anyone's list of scams.
//
// A Uniswap v2 or Aerodrome pool is its own LP token: its Transfer logs give the LP supply and the protocol fee
// collector's share at any point, and the pool counts as pulled under facts.lpPulled's rule (the LP held by anyone
// but the fee collector, beyond the permanent minimum, is under 1% of supply). A Uniswap v3 pool's positions are
// NFTs held elsewhere, so its liquidity is summed from the pool's Mint and Burn logs instead, and it counts as pulled
// when what remains is under 1% of the most it ever held.
package outcome

import (
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Pool events the outcome reads.
var (
	TopicTransfer = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	// Swap on a Uniswap v2 pair and on an Aerodrome pool: same amounts, different signatures.
	TopicSwapV2        = crypto.Keccak256Hash([]byte("Swap(address,uint256,uint256,uint256,uint256,address)"))
	TopicSwapAerodrome = crypto.Keccak256Hash([]byte("Swap(address,address,uint256,uint256,uint256,uint256)"))
	TopicSwapV3        = crypto.Keccak256Hash([]byte("Swap(address,address,int256,int256,uint160,uint128,int24)"))
	TopicMintV3        = crypto.Keccak256Hash([]byte("Mint(address,address,int24,int24,uint128,uint256,uint256)"))
	TopicBurnV3        = crypto.Keccak256Hash([]byte("Burn(address,int24,int24,uint128,uint256,uint256)"))
)

// Topics is every event Read needs from a pool, for one eth_getLogs query.
var Topics = []common.Hash{TopicTransfer, TopicSwapV2, TopicSwapAerodrome, TopicSwapV3, TopicMintV3, TopicBurnV3}

// minimumLiquidity is the LP a v2-style pool mints once and never releases.
var minimumLiquidity = big.NewInt(1000)

// Pool is what the outcome needs to know about a launch.
type Pool struct {
	DEX   string // uniswap-v2, uniswap-v3, aerodrome
	Token common.Address
	Quote common.Address
	// FeeTo is the v2 factory's protocol fee collector, which LP is minted to as fees accrue; zero if none.
	FeeTo common.Address
}

// Outcome is what happened in a pool from its creation to the end of the horizon.
type Outcome struct {
	Pulled bool `json:"pulled"`
	// PulledBlock is the block where the liquidity first fell below the threshold; zero if it never did.
	PulledBlock uint64 `json:"pulled_block,omitempty"`
	// PulledAt is PulledBlock's time; Label sets it, Read does not.
	PulledAt time.Time `json:"pulled_at"`
	// Buys and Sells count swaps before the pull (or, if there was none, up to the end of the horizon). A buy pays
	// the quote token in; a sell pays the token in.
	Buys  int `json:"buys"`
	Sells int `json:"sells"`
	// Bait is a pool pulled before anyone traded: nobody lost money in it.
	Bait bool `json:"bait"`
}

// Read decides the outcome from the pool's logs, which must run from its creation to the end of the horizon.
func Read(p Pool, logs []types.Log) Outcome {
	sort.SliceStable(logs, func(i, j int) bool {
		if logs[i].BlockNumber != logs[j].BlockNumber {
			return logs[i].BlockNumber < logs[j].BlockNumber
		}
		return logs[i].Index < logs[j].Index
	})
	tokenIs0 := p.Token.Cmp(p.Quote) < 0
	var o Outcome
	supply, protocol := new(big.Int), new(big.Int) // v2-style LP
	live, peak := new(big.Int), new(big.Int)       // v3 liquidity
	for _, l := range logs {
		if len(l.Topics) == 0 || l.Removed {
			continue
		}
		switch l.Topics[0] {
		case TopicTransfer:
			if p.DEX == "uniswap-v3" || len(l.Topics) < 3 || len(l.Data) < 32 {
				continue
			}
			from, to := common.BytesToAddress(l.Topics[1].Bytes()), common.BytesToAddress(l.Topics[2].Bytes())
			v := new(big.Int).SetBytes(l.Data[:32])
			burn := to == (common.Address{}) && from != (common.Address{})
			switch {
			case from == (common.Address{}):
				supply.Add(supply, v)
			case burn:
				supply.Sub(supply, v)
			}
			if p.FeeTo != (common.Address{}) {
				if to == p.FeeTo {
					protocol.Add(protocol, v)
				}
				if from == p.FeeTo {
					protocol.Sub(protocol, v)
				}
			}
			// Only a burn can pull: other transfers move LP between holders, and fee mints add to both sides.
			if burn && !o.Pulled && lpPulled(supply, protocol) {
				o.Pulled, o.PulledBlock = true, l.BlockNumber
			}
		case TopicMintV3, TopicBurnV3:
			if p.DEX != "uniswap-v3" {
				continue
			}
			if l.Topics[0] == TopicMintV3 && len(l.Data) >= 64 { // data: sender, amount, amount0, amount1
				live.Add(live, new(big.Int).SetBytes(l.Data[32:64]))
				if live.Cmp(peak) > 0 {
					peak.Set(live)
				}
			} else if l.Topics[0] == TopicBurnV3 && len(l.Data) >= 32 { // data: amount, amount0, amount1
				live.Sub(live, new(big.Int).SetBytes(l.Data[:32]))
				if !o.Pulled && peak.Sign() > 0 && new(big.Int).Mul(live, big.NewInt(100)).Cmp(peak) < 0 {
					o.Pulled, o.PulledBlock = true, l.BlockNumber
				}
			}
		case TopicSwapV2, TopicSwapAerodrome, TopicSwapV3:
			if o.Pulled {
				continue
			}
			if sell, ok := isSell(p.DEX, l, tokenIs0); ok {
				if sell {
					o.Sells++
				} else {
					o.Buys++
				}
			}
		}
	}
	o.Bait = o.Pulled && o.Buys+o.Sells == 0
	return o
}

// lpPulled is facts.lpPulled's rule on a v2-style pool: what is left outside the minimum and the fee collector is
// gone, or under 1% of the supply.
func lpPulled(supply, protocol *big.Int) bool {
	if supply.Sign() == 0 {
		return false
	}
	rest := new(big.Int).Sub(supply, minimumLiquidity)
	rest.Sub(rest, protocol)
	return rest.Sign() <= 0 || new(big.Int).Mul(rest, big.NewInt(100)).Cmp(supply) < 0
}

// isSell reports whether a swap paid the token into the pool. ok is false for a log of another venue's shape.
func isSell(dex string, l types.Log, tokenIs0 bool) (sell, ok bool) {
	switch {
	case dex == "uniswap-v3" && l.Topics[0] == TopicSwapV3 && len(l.Data) >= 64:
		// amount0 and amount1 are signed: positive is paid into the pool.
		amt := l.Data[32:64]
		if tokenIs0 {
			amt = l.Data[:32]
		}
		return new(big.Int).SetBytes(amt).Sign() > 0 && amt[0] < 0x80, true
	case (dex == "uniswap-v2" && l.Topics[0] == TopicSwapV2 || dex == "aerodrome" && l.Topics[0] == TopicSwapAerodrome) && len(l.Data) >= 128:
		// amount0In, amount1In, amount0Out, amount1Out
		in := l.Data[32:64]
		if tokenIs0 {
			in = l.Data[:32]
		}
		return new(big.Int).SetBytes(in).Sign() > 0, true
	}
	return false, false
}

// Horizon is how long after its first liquidity a launch is watched for a pull.
const Horizon = 24 * time.Hour
