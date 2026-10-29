// Package chain talks to an EVM chain over JSON-RPC: the networks rugcheck knows, a client that copes with the
// limits of free public RPCs, Multicall3 batching, windowed log scans and contract-creation lookups.
package chain

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Quote is a token a pool can be priced in.
type Quote struct {
	Symbol   string
	Address  common.Address
	Decimals uint8
}

// V2DEX is a Uniswap v2-style exchange: a factory with getPair and a router with the v2 swap functions.
type V2DEX struct {
	Name    string
	Factory common.Address
	Router  common.Address
}

// Network is a chain rugcheck can analyse. Every address here was checked on chain (factory.getPair, router.factory,
// router.WETH, quoter.factory, code present) when it was added.
type Network struct {
	Name      string
	ChainID   int64
	BlockTime time.Duration
	// RPC serves latest-state reads (eth_call, code, balances), which make up most of the traffic. ArchiveRPC serves
	// old blocks: historical state, receipts, headers and eth_getLogs. Free providers split along this line, so the
	// defaults use one of each. RUGCHECK_RPC_<NAME> overrides both; RUGCHECK_ARCHIVE_RPC_<NAME> only the archive.
	RPC        string
	ArchiveRPC string
	// ArchiveRate paces the default archive RPC, in requests per second (each element of a batch counts); 0 means
	// no limit.
	ArchiveRate float64
	// LogWindow is the largest eth_getLogs block range the default archive RPC accepts. The client shrinks it on
	// errors.
	LogWindow uint64
	// ForkFromArchive makes simulations fork the archive RPC even at the latest block: publicnode's Base endpoint
	// refuses some of anvil's fork requests as archive requests.
	ForkFromArchive bool
	// Blockscout is the explorer's base URL, or "" if there is none that answers keyless requests.
	Blockscout string

	WETH      common.Address
	Quotes    []Quote
	V2        []V2DEX
	V3Factory common.Address
	// V3Router is Uniswap's SwapRouter02 and V3Quoter its QuoterV2 (quotes v3 swaps without a pool-math port).
	V3Router common.Address
	V3Quoter common.Address
	// Aerodrome is the Aerodrome pool factory (getPool(a, b, stable)) and AerodromeRouter its router; zero on
	// chains without it.
	Aerodrome       common.Address
	AerodromeRouter common.Address
	// Lockers are contracts that hold LP tokens on behalf of their owners until an unlock date.
	Lockers map[common.Address]string
}

// Multicall3 is deployed at the same address on every chain rugcheck supports.
var Multicall3 = common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")

// Burn addresses: tokens sent here are gone for good.
var (
	ZeroAddress = common.Address{}
	DeadAddress = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
)

// IsBurn reports whether a is an address tokens are sent to be destroyed.
func IsBurn(a common.Address) bool { return a == ZeroAddress || a == DeadAddress }

var (
	Ethereum = Network{
		Name:       "ethereum",
		ChainID:    1,
		BlockTime:  12 * time.Second,
		RPC:        "https://ethereum-rpc.publicnode.com",
		ArchiveRPC: "https://rpc.mevblocker.io",
		LogWindow:  10_000,
		Blockscout: "https://eth.blockscout.com",
		WETH:       common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		Quotes: []Quote{
			{"WETH", common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"), 18},
			{"USDC", common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"), 6},
			{"USDT", common.HexToAddress("0xdAC17F958D2ee523a2206206994597C13D831ec7"), 6},
		},
		V2: []V2DEX{{
			Name:    "uniswap-v2",
			Factory: common.HexToAddress("0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f"),
			Router:  common.HexToAddress("0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D"),
		}},
		V3Factory: common.HexToAddress("0x1F98431c8aD98523631AE4a59f267346ea31F984"),
		V3Router:  common.HexToAddress("0x68b3465833fb72A70ecDF485E0e4C7bD8665Fc45"),
		V3Quoter:  common.HexToAddress("0x61fFE014bA17989E743c5F6cB21bF9697530B21e"),
		Lockers: map[common.Address]string{
			common.HexToAddress("0x663A5C229c09b049E36dCc11a9B0d4a8Eb9db214"): "UNCX",
			common.HexToAddress("0xE2fE530C047f2d85298b07D9333C05737f1435fB"): "Team Finance",
			common.HexToAddress("0x71B5759d73262FBb223956913ecF4ecC51057641"): "PinkLock",
		},
	}
	Base = Network{
		Name:       "base",
		ChainID:    8453,
		BlockTime:  2 * time.Second,
		RPC:        "https://base-rpc.publicnode.com",
		ArchiveRPC: "https://base.gateway.tenderly.co",
		// Tenderly's public gateway answers 429 to bursts (a batch of 16 is one); single requests at 15/s pass.
		ArchiveRate:     8,
		LogWindow:       1_000,
		ForkFromArchive: true,
		// base.blockscout.com sits behind a browser challenge for scripted clients; the client tries it and falls
		// back to the RPC when it answers with HTML.
		Blockscout: "https://base.blockscout.com",
		WETH:       common.HexToAddress("0x4200000000000000000000000000000000000006"),
		Quotes: []Quote{
			{"WETH", common.HexToAddress("0x4200000000000000000000000000000000000006"), 18},
			{"USDC", common.HexToAddress("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"), 6},
		},
		V2: []V2DEX{{
			Name:    "uniswap-v2",
			Factory: common.HexToAddress("0x8909Dc15e40173Ff4699343b6eB8132c65e18eC6"),
			Router:  common.HexToAddress("0x4752ba5DBc23f44D87826276BF6Fd6b1C372aD24"),
		}},
		V3Factory:       common.HexToAddress("0x33128a8fC17869897dcE68Ed026d694621f6FDfD"),
		V3Router:        common.HexToAddress("0x2626664c2603336E57B271c5C0b26F421741e481"),
		V3Quoter:        common.HexToAddress("0x3d4e44Eb1374240CE5F1B871ab261CD16335B76a"),
		Aerodrome:       common.HexToAddress("0x420DD381b31aEf6683db6B902084cB0FFECe40Da"),
		AerodromeRouter: common.HexToAddress("0xcF77a3Ba9A5CA399B7c97c74d54e5b1Beb874E43"),
		Lockers:         map[common.Address]string{},
	}
)

// Networks lists every supported network.
func Networks() []Network { return []Network{Ethereum, Base} }

// ByName finds a network by name ("ethereum", "eth", "base").
func ByName(name string) (Network, error) {
	switch strings.ToLower(name) {
	case "ethereum", "eth", "mainnet":
		return Ethereum, nil
	case "base":
		return Base, nil
	}
	return Network{}, fmt.Errorf("unknown chain %q (want ethereum or base)", name)
}

// Endpoints returns the latest-state and archive RPC URLs after environment overrides, and the archive pacing that
// applies (none once the archive endpoint is not the default).
func (n Network) Endpoints() (latest, archive string, archiveRate float64) {
	latest, archive, archiveRate = n.RPC, n.ArchiveRPC, n.ArchiveRate
	if u := os.Getenv("RUGCHECK_RPC_" + strings.ToUpper(n.Name)); u != "" {
		latest, archive, archiveRate = u, u, 0
	}
	if u := os.Getenv("RUGCHECK_ARCHIVE_RPC_" + strings.ToUpper(n.Name)); u != "" {
		archive, archiveRate = u, 0
	}
	return latest, archive, archiveRate
}
