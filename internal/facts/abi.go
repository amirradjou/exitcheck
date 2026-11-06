package facts

import (
	"math/big"
	"strings"
	"unicode"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/exitcheck/internal/chain"
)

var (
	erc20ABI = chain.MustABI(`[
	{"name":"name","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"string"}]},
	{"name":"symbol","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"string"}]},
	{"name":"decimals","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"uint8"}]},
	{"name":"totalSupply","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
	{"name":"balanceOf","type":"function","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
	{"name":"owner","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
	{"name":"getOwner","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}]`)
	v2FactoryABI = chain.MustABI(`[
	{"name":"getPair","type":"function","stateMutability":"view","inputs":[{"type":"address"},{"type":"address"}],"outputs":[{"type":"address"}]},
	{"name":"feeTo","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}]`)
	v3FactoryABI = chain.MustABI(`[
	{"name":"getPool","type":"function","stateMutability":"view","inputs":[{"type":"address"},{"type":"address"},{"type":"uint24"}],"outputs":[{"type":"address"}]}]`)
	aerodromeFactoryABI = chain.MustABI(`[
	{"name":"getPool","type":"function","stateMutability":"view","inputs":[{"type":"address"},{"type":"address"},{"type":"bool"}],"outputs":[{"type":"address"}]}]`)
	beaconABI = chain.MustABI(`[
	{"name":"implementation","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}]`)
)

// pack encodes a call and panics on error: every call site passes arguments of the ABI's own types.
func pack(a abi.ABI, method string, args ...any) []byte {
	data, err := a.Pack(method, args...)
	if err != nil {
		panic(err)
	}
	return data
}

func balanceOfCall(token, holder common.Address) chain.Call {
	return chain.Call{To: token, Data: pack(erc20ABI, "balanceOf", holder)}
}

// word decodes a single 32-byte return value as an integer; ok is false if the call failed or returned too little.
func word(r chain.Result) (*big.Int, bool) {
	if !r.OK || len(r.Data) < 32 {
		return nil, false
	}
	return new(big.Int).SetBytes(r.Data[:32]), true
}

// addressOf decodes a single address return value.
func addressOf(r chain.Result) (common.Address, bool) {
	if !r.OK || len(r.Data) < 32 {
		return common.Address{}, false
	}
	return common.BytesToAddress(r.Data[12:32]), true
}

// text decodes a string return value (name, symbol). Some old tokens (MKR, SAI) return bytes32 instead. The
// result is made printable: the contract's author chooses it, and a name with terminal escape codes could rewrite
// the report it appears in.
func text(r chain.Result) string {
	if !r.OK {
		return ""
	}
	if vals, err := erc20ABI.Methods["name"].Outputs.Unpack(r.Data); err == nil {
		return printable(vals[0].(string), maxNameRunes)
	}
	if len(r.Data) == 32 {
		return printable(strings.TrimRight(string(r.Data), "\x00"), maxNameRunes)
	}
	return ""
}

const maxNameRunes = 64

// printable replaces control and other non-printing characters with U+FFFD and cuts s to max runes.
func printable(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == max {
			b.WriteString("…")
			break
		}
		if !unicode.IsPrint(r) {
			r = unicode.ReplacementChar
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
