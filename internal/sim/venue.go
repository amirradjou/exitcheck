package sim

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/exitcheck/internal/chain"
	"github.com/amirradjou/exitcheck/internal/facts"
)

// venue trades one pool: it quotes a swap the way the DEX prices it (before any fee the token itself takes) and
// builds the swap transaction. Buys pay ETH; sells pay out ETH or WETH.
type venue interface {
	describe() string
	spender() common.Address
	quoteBuy(ctx context.Context, f *fork, wei *big.Int) (*big.Int, error)
	buy(to common.Address, wei *big.Int) []byte
	quoteSell(ctx context.Context, f *fork, tokens *big.Int) (*big.Int, error)
	sell(to common.Address, tokens *big.Int) []byte
}

// deadline is far in the future; the fork's clock moves only when the simulation moves it.
var deadline = big.NewInt(1 << 40)

var (
	v2RouterABI = chain.MustABI(`[
	{"name":"getAmountsOut","type":"function","stateMutability":"view","inputs":[{"type":"uint256"},{"type":"address[]"}],"outputs":[{"type":"uint256[]"}]},
	{"name":"swapExactETHForTokensSupportingFeeOnTransferTokens","type":"function","stateMutability":"payable",
	 "inputs":[{"type":"uint256"},{"type":"address[]"},{"type":"address"},{"type":"uint256"}],"outputs":[]},
	{"name":"swapExactTokensForETHSupportingFeeOnTransferTokens","type":"function","stateMutability":"nonpayable",
	 "inputs":[{"type":"uint256"},{"type":"uint256"},{"type":"address[]"},{"type":"address"},{"type":"uint256"}],"outputs":[]}]`)
	v3RouterABI = chain.MustABI(`[
	{"name":"exactInput","type":"function","stateMutability":"payable","inputs":[{"name":"params","type":"tuple","components":[
	  {"name":"path","type":"bytes"},{"name":"recipient","type":"address"},{"name":"amountIn","type":"uint256"},{"name":"amountOutMinimum","type":"uint256"}]}],
	 "outputs":[{"type":"uint256"}]}]`)
	v3QuoterABI = chain.MustABI(`[
	{"name":"quoteExactInput","type":"function","stateMutability":"nonpayable","inputs":[{"type":"bytes"},{"type":"uint256"}],
	 "outputs":[{"type":"uint256"},{"type":"uint160[]"},{"type":"uint32[]"},{"type":"uint256"}]}]`)
	aerodromeRouterABI = chain.MustABI(`[
	{"name":"getAmountsOut","type":"function","stateMutability":"view","inputs":[{"type":"uint256"},{"type":"tuple[]","components":[
	  {"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"stable","type":"bool"},{"name":"factory","type":"address"}]}],
	 "outputs":[{"type":"uint256[]"}]},
	{"name":"swapExactETHForTokensSupportingFeeOnTransferTokens","type":"function","stateMutability":"payable","inputs":[{"type":"uint256"},
	  {"type":"tuple[]","components":[{"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"stable","type":"bool"},{"name":"factory","type":"address"}]},
	  {"type":"address"},{"type":"uint256"}],"outputs":[]},
	{"name":"swapExactTokensForETHSupportingFeeOnTransferTokens","type":"function","stateMutability":"nonpayable","inputs":[{"type":"uint256"},{"type":"uint256"},
	  {"type":"tuple[]","components":[{"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"stable","type":"bool"},{"name":"factory","type":"address"}]},
	  {"type":"address"},{"type":"uint256"}],"outputs":[]}]`)
)

// newVenue builds the venue for a pool: directly against WETH, or through the WETH/quote market for USDC and USDT
// pools.
func newVenue(net chain.Network, token common.Address, p facts.Pool) (venue, error) {
	weth := net.WETH
	viaQuote := p.QuoteToken != weth
	switch {
	case p.DEX == "uniswap-v3":
		if net.V3Router == (common.Address{}) {
			return nil, errors.New("no v3 router for " + net.Name)
		}
		buy := []any{weth}
		if viaQuote {
			// WETH/USDC and WETH/USDT trade deepest at 0.05% on both chains.
			buy = append(buy, uint32(500), p.QuoteToken)
		}
		buy = append(buy, p.Fee, token)
		return &v3Venue{router: net.V3Router, quoter: net.V3Quoter, buyPath: v3Path(buy...), sellPath: v3Path(reversePath(buy)...),
			name: fmt.Sprintf("uniswap-v3 %.2f%% pool %s", float64(p.Fee)/1e4, p.Address.Hex())}, nil
	case p.DEX == "aerodrome":
		if net.AerodromeRouter == (common.Address{}) {
			return nil, errors.New("no Aerodrome router for " + net.Name)
		}
		var buy []aeroRoute
		from := weth
		if viaQuote {
			buy = append(buy, aeroRoute{From: weth, To: p.QuoteToken, Stable: false, Factory: net.Aerodrome})
			from = p.QuoteToken
		}
		buy = append(buy, aeroRoute{From: from, To: token, Stable: p.Stable, Factory: net.Aerodrome})
		sell := make([]aeroRoute, len(buy))
		for i, r := range buy {
			sell[len(buy)-1-i] = aeroRoute{From: r.To, To: r.From, Stable: r.Stable, Factory: r.Factory}
		}
		return &aeroVenue{router: net.AerodromeRouter, buyRoutes: buy, sellRoutes: sell,
			name: "aerodrome pool " + p.Address.Hex()}, nil
	default:
		for _, d := range net.V2 {
			if d.Name != p.DEX {
				continue
			}
			path := []common.Address{weth}
			if viaQuote {
				path = append(path, p.QuoteToken)
			}
			path = append(path, token)
			sell := make([]common.Address, len(path))
			for i, a := range path {
				sell[len(path)-1-i] = a
			}
			return &v2Venue{router: d.Router, buyPath: path, sellPath: sell, name: d.Name + " pool " + p.Address.Hex()}, nil
		}
	}
	return nil, fmt.Errorf("no router for %s pools", p.DEX)
}

type v2Venue struct {
	router            common.Address
	buyPath, sellPath []common.Address
	name              string
}

func (v *v2Venue) describe() string        { return v.name }
func (v *v2Venue) spender() common.Address { return v.router }

func (v *v2Venue) quoteBuy(ctx context.Context, f *fork, wei *big.Int) (*big.Int, error) {
	return v.quote(ctx, f, wei, v.buyPath)
}

func (v *v2Venue) quoteSell(ctx context.Context, f *fork, tokens *big.Int) (*big.Int, error) {
	return v.quote(ctx, f, tokens, v.sellPath)
}

func (v *v2Venue) quote(ctx context.Context, f *fork, in *big.Int, path []common.Address) (*big.Int, error) {
	out, err := f.call(ctx, v.router, pack(v2RouterABI, "getAmountsOut", in, path))
	if err != nil {
		return nil, err
	}
	vals, err := v2RouterABI.Methods["getAmountsOut"].Outputs.Unpack(out)
	if err != nil {
		return nil, err
	}
	amounts := vals[0].([]*big.Int)
	return amounts[len(amounts)-1], nil
}

func (v *v2Venue) buy(to common.Address, wei *big.Int) []byte {
	return pack(v2RouterABI, "swapExactETHForTokensSupportingFeeOnTransferTokens", new(big.Int), v.buyPath, to, deadline)
}

func (v *v2Venue) sell(to common.Address, tokens *big.Int) []byte {
	return pack(v2RouterABI, "swapExactTokensForETHSupportingFeeOnTransferTokens", tokens, new(big.Int), v.sellPath, to, deadline)
}

type v3Venue struct {
	router, quoter    common.Address
	buyPath, sellPath []byte
	name              string
}

func (v *v3Venue) describe() string        { return v.name }
func (v *v3Venue) spender() common.Address { return v.router }

func (v *v3Venue) quoteBuy(ctx context.Context, f *fork, wei *big.Int) (*big.Int, error) {
	return v.quote(ctx, f, wei, v.buyPath)
}

func (v *v3Venue) quoteSell(ctx context.Context, f *fork, tokens *big.Int) (*big.Int, error) {
	return v.quote(ctx, f, tokens, v.sellPath)
}

// quote asks QuoterV2, which runs the swap and reverts with the result; through eth_call that reads as a return.
func (v *v3Venue) quote(ctx context.Context, f *fork, in *big.Int, path []byte) (*big.Int, error) {
	out, err := f.call(ctx, v.quoter, pack(v3QuoterABI, "quoteExactInput", path, in))
	if err != nil {
		return nil, err
	}
	vals, err := v3QuoterABI.Methods["quoteExactInput"].Outputs.Unpack(out)
	if err != nil {
		return nil, err
	}
	return vals[0].(*big.Int), nil
}

type exactInputParams struct {
	Path             []byte
	Recipient        common.Address
	AmountIn         *big.Int
	AmountOutMinimum *big.Int
}

func (v *v3Venue) buy(to common.Address, wei *big.Int) []byte {
	return pack(v3RouterABI, "exactInput", exactInputParams{v.buyPath, to, wei, new(big.Int)})
}

// sell pays out WETH: SwapRouter02 only unwraps in a multicall, and WETH counts the same for the measurement.
func (v *v3Venue) sell(to common.Address, tokens *big.Int) []byte {
	return pack(v3RouterABI, "exactInput", exactInputParams{v.sellPath, to, tokens, new(big.Int)})
}

// v3Path encodes a Uniswap v3 path: address, fee (3 bytes), address, ...
func v3Path(hops ...any) []byte {
	var b []byte
	for _, h := range hops {
		switch x := h.(type) {
		case common.Address:
			b = append(b, x[:]...)
		case uint32:
			var fee [4]byte
			binary.BigEndian.PutUint32(fee[:], x)
			b = append(b, fee[1:]...)
		}
	}
	return b
}

func reversePath(hops []any) []any {
	out := make([]any, len(hops))
	for i, h := range hops {
		out[len(hops)-1-i] = h
	}
	return out
}

type aeroRoute struct {
	From, To common.Address
	Stable   bool
	Factory  common.Address
}

type aeroVenue struct {
	router                common.Address
	buyRoutes, sellRoutes []aeroRoute
	name                  string
}

func (v *aeroVenue) describe() string        { return v.name }
func (v *aeroVenue) spender() common.Address { return v.router }

func (v *aeroVenue) quoteBuy(ctx context.Context, f *fork, wei *big.Int) (*big.Int, error) {
	return v.quote(ctx, f, wei, v.buyRoutes)
}

func (v *aeroVenue) quoteSell(ctx context.Context, f *fork, tokens *big.Int) (*big.Int, error) {
	return v.quote(ctx, f, tokens, v.sellRoutes)
}

func (v *aeroVenue) quote(ctx context.Context, f *fork, in *big.Int, routes []aeroRoute) (*big.Int, error) {
	out, err := f.call(ctx, v.router, pack(aerodromeRouterABI, "getAmountsOut", in, routes))
	if err != nil {
		return nil, err
	}
	vals, err := aerodromeRouterABI.Methods["getAmountsOut"].Outputs.Unpack(out)
	if err != nil {
		return nil, err
	}
	amounts := vals[0].([]*big.Int)
	return amounts[len(amounts)-1], nil
}

func (v *aeroVenue) buy(to common.Address, wei *big.Int) []byte {
	return pack(aerodromeRouterABI, "swapExactETHForTokensSupportingFeeOnTransferTokens", new(big.Int), v.buyRoutes, to, deadline)
}

func (v *aeroVenue) sell(to common.Address, tokens *big.Int) []byte {
	return pack(aerodromeRouterABI, "swapExactTokensForETHSupportingFeeOnTransferTokens", tokens, new(big.Int), v.sellRoutes, to, deadline)
}

// pack encodes a call and panics on error: every call site passes arguments of the ABI's own types.
func pack(a abi.ABI, method string, args ...any) []byte {
	data, err := a.Pack(method, args...)
	if err != nil {
		panic(err)
	}
	return data
}
