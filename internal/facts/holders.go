package facts

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/evm"
)

// Holder is one balance in a holder list.
type Holder struct {
	Address common.Address `json:"address"`
	Balance *big.Int       `json:"balance"`
	Share   float64        `json:"share"`
	// Label says who the holder is: pool, burn, locker:<name>, protocol (a DEX fee collector), deployer, owner,
	// token (the contract itself), contract or wallet.
	Label string `json:"label"`
}

// Holders is the top of a token's holder list.
type Holders struct {
	Source string   `json:"source"` // "blockscout" or "transfer-logs"
	Count  int      `json:"count"`  // accounts with a non-zero balance
	Top    []Holder `json:"top"`
	// TopShare and Top10Share count only holders that can sell: pools, burn addresses and lockers are left out.
	TopShare   float64 `json:"top_share"`
	Top10Share float64 `json:"top10_share"`
}

// Replay budgets: past these, the token has too much history to replay through a free RPC, and holders are
// reported as a gap instead (Blockscout covers old tokens on Ethereum).
const (
	maxReplayRequests = 150
	maxReplayLogs     = 200_000
	topCandidates     = 30
	topShown          = 10
)

var transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

func (g *gatherer) holders(ctx context.Context) {
	supply := g.f.Token.TotalSupply
	if supply == nil || supply.Sign() == 0 {
		return
	}
	from := uint64(0)
	if cr := g.f.Contract.Creation; cr != nil {
		from = cr.Block
	}
	top, count, source, err := g.topHolders(ctx, g.f.Address, from, supply)
	if err != nil {
		g.gap("holders: %v", err)
		return
	}
	h := &Holders{Source: source, Count: count, Top: top}
	n := 0
	for _, x := range top {
		if x.Label == "pool" || x.Label == "burn" || x.Label == "protocol" || strings.HasPrefix(x.Label, "locker:") {
			continue
		}
		if n == 0 {
			h.TopShare = x.Share
		}
		if n < 10 {
			h.Top10Share += x.Share
		}
		n++
	}
	if len(h.Top) > topShown {
		h.Top = h.Top[:topShown]
	}
	g.f.Holders = h
}

// topHolders returns the largest holders of token sorted by balance, with shares of supply and labels. It asks the
// explorer first and falls back to replaying Transfer logs from block from. Balances are read with balanceOf either
// way: reflection tokens change balances without emitting Transfer events.
func (g *gatherer) topHolders(ctx context.Context, token common.Address, from uint64, supply *big.Int) ([]Holder, int, string, error) {
	var candidates []common.Address
	source, count := "blockscout", 0
	list, n, exErr := g.ex.TopHolders(ctx, g.c.Net, token)
	if exErr == nil && len(list) > 0 {
		count = n
		for _, h := range list {
			candidates = append(candidates, h.Address)
		}
	} else {
		source = "transfer-logs"
		balances, err := g.replayBalances(ctx, token, from)
		if err != nil {
			if exErr != nil {
				err = fmt.Errorf("%w (explorer: %v)", err, exErr)
			}
			return nil, 0, "", err
		}
		for a, b := range balances {
			if b.Sign() > 0 {
				count++
				candidates = append(candidates, a)
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			if c := balances[candidates[i]].Cmp(balances[candidates[j]]); c != 0 {
				return c > 0
			}
			return candidates[i].Cmp(candidates[j]) < 0
		})
	}
	if len(candidates) > topCandidates {
		candidates = candidates[:topCandidates]
	}

	calls := make([]chain.Call, len(candidates))
	for i, a := range candidates {
		calls[i] = balanceOfCall(token, a)
	}
	res, err := g.c.Multicall(ctx, calls)
	if err != nil {
		return nil, 0, "", err
	}
	top := make([]Holder, 0, len(candidates))
	for i, a := range candidates {
		if b, ok := word(res[i]); ok && b.Sign() > 0 {
			top = append(top, Holder{Address: a, Balance: b, Share: ratio(b, supply)})
		}
	}
	sort.SliceStable(top, func(i, j int) bool { return top[i].Balance.Cmp(top[j].Balance) > 0 })
	if err := g.label(ctx, top); err != nil {
		return nil, 0, "", err
	}
	return top, count, source, nil
}

// replayBalances rebuilds balances from Transfer logs.
func (g *gatherer) replayBalances(ctx context.Context, token common.Address, from uint64) (map[common.Address]*big.Int, error) {
	balances := map[common.Address]*big.Int{}
	seen := 0
	q := ethereum.FilterQuery{Addresses: []common.Address{token}, Topics: [][]common.Hash{{transferTopic}}}
	err := g.c.ScanLogs(ctx, chain.LogScan{Query: q, From: from, To: g.f.Block, MaxRequests: maxReplayRequests},
		func(logs []types.Log) error {
			seen += len(logs)
			if seen > maxReplayLogs {
				return fmt.Errorf("more than %d transfers to replay", maxReplayLogs)
			}
			for _, l := range logs {
				src, dst, v, ok := decodeTransfer(l)
				if !ok {
					continue
				}
				if src != chain.ZeroAddress {
					sub(balances, src, v)
				}
				add(balances, dst, v)
			}
			return nil
		})
	if errors.Is(err, chain.ErrBudget) {
		return nil, fmt.Errorf("too much history to replay through the RPC (%v)", err)
	}
	return balances, err
}

// decodeTransfer reads an ERC-20 Transfer log. Old tokens emit it with no indexed parameters; ERC-721 transfers
// (three indexed parameters, no data) are skipped.
func decodeTransfer(l types.Log) (from, to common.Address, v *big.Int, ok bool) {
	switch {
	case len(l.Topics) == 3 && len(l.Data) >= 32:
		return common.BytesToAddress(l.Topics[1][12:]), common.BytesToAddress(l.Topics[2][12:]),
			new(big.Int).SetBytes(l.Data[:32]), true
	case len(l.Topics) == 1 && len(l.Data) >= 96:
		return common.BytesToAddress(l.Data[12:32]), common.BytesToAddress(l.Data[44:64]),
			new(big.Int).SetBytes(l.Data[64:96]), true
	}
	return common.Address{}, common.Address{}, nil, false
}

func add(m map[common.Address]*big.Int, a common.Address, v *big.Int) {
	if m[a] == nil {
		m[a] = new(big.Int)
	}
	m[a].Add(m[a], v)
}

func sub(m map[common.Address]*big.Int, a common.Address, v *big.Int) {
	if m[a] == nil {
		m[a] = new(big.Int)
	}
	m[a].Sub(m[a], v)
}

// label names each holder. Known roles come first; the rest are told apart by their code.
func (g *gatherer) label(ctx context.Context, hs []Holder) error {
	pools := map[common.Address]bool{}
	for _, p := range g.f.Pools {
		pools[p.Address] = true
	}
	protocol := map[common.Address]bool{}
	for _, a := range g.feeTo {
		protocol[a] = true
	}
	var unknown []int
	for i := range hs {
		a := hs[i].Address
		switch {
		case chain.IsBurn(a):
			hs[i].Label = "burn"
		case pools[a]:
			hs[i].Label = "pool"
		case protocol[a]:
			hs[i].Label = "protocol"
		case g.c.Net.Lockers[a] != "":
			hs[i].Label = "locker:" + g.c.Net.Lockers[a]
		case a == g.deployer():
			hs[i].Label = "deployer"
		case a == g.owner():
			hs[i].Label = "owner"
		case a == g.f.Address:
			hs[i].Label = "token"
		default:
			unknown = append(unknown, i)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	addrs := make([]common.Address, len(unknown))
	for k, i := range unknown {
		addrs[k] = hs[i].Address
	}
	codes, err := g.c.Codes(ctx, addrs)
	if err != nil {
		return err
	}
	for k, i := range unknown {
		hs[i].Label = map[bool]string{true: "contract", false: "wallet"}[evm.IsContract(codes[k])]
	}
	return nil
}
