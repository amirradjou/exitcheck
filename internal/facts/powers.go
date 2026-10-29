package facts

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/evm"
)

// Kinds of privileged power a token can give its owner.
const (
	KindMint      = "mint"
	KindPause     = "pause"
	KindBlacklist = "blacklist"
	KindLimits    = "limits"
	KindFees      = "fees"
	KindTrading   = "trading"
	KindUpgrade   = "upgrade"
	KindReclaim   = "reclaim-ownership"
	KindSeize     = "seize"
)

// Kinds lists the power kinds in report order.
var Kinds = []string{KindMint, KindSeize, KindBlacklist, KindPause, KindTrading, KindFees, KindLimits, KindUpgrade, KindReclaim}

// knownPowers are privileged functions under the names token templates and well-known tokens use. A contract with
// non-standard names is still covered when its source is verified (see scanSource).
var knownPowers = map[string][]string{
	KindMint: {"mint(address,uint256)", "mint(uint256)", "mintTo(address,uint256)", "issue(uint256)",
		"configureMinter(address,uint256)", "addMinter(address)"},
	KindSeize: {"destroyBlackFunds(address)", "wipeFrozenAddress(address)", "seize(address,uint256)",
		"clawback(address,uint256)"},
	KindBlacklist: {"blacklist(address)", "blacklist(address,bool)", "addBlackList(address)", "addToBlacklist(address)",
		"setBlacklist(address,bool)", "blacklistAddress(address,bool)", "updateBlacklist(address,bool)",
		"setIsBlacklisted(address,bool)", "blacklistAccount(address)", "manageBlacklist(address[],bool)",
		"setBots(address[])", "addBots(address[])", "addBot(address)", "setBot(address,bool)", "blockBots(address[])",
		"freeze(address)", "freezeAccount(address,bool)"},
	KindPause:   {"pause()", "unpause()", "setPaused(bool)"},
	KindTrading: {"enableTrading()", "openTrading()", "startTrading()", "launch()", "setTradingEnabled(bool)", "setTrading(bool)", "tradingStatus(bool)", "enableTrading(uint256)"},
	KindFees: {"setFee(uint256)", "setFee(uint256,uint256)", "setFees(uint256,uint256)", "setTaxes(uint256,uint256)",
		"setBuyTax(uint256)", "setSellTax(uint256)", "setBuyFee(uint256)", "setSellFee(uint256)",
		"updateFees(uint256,uint256)", "updateBuyFees(uint256,uint256,uint256)", "updateSellFees(uint256,uint256,uint256)",
		"setTaxFeePercent(uint256)", "setLiquidityFeePercent(uint256)", "setTaxFee(uint256)", "setParams(uint256,uint256)",
		"reduceFee(uint256)", "excludeFromFee(address)", "excludeFromFees(address,bool)"},
	KindLimits: {"setMaxTxAmount(uint256)", "setMaxTxPercent(uint256)", "setMaxWalletSize(uint256)", "setMaxWallet(uint256)",
		"updateMaxTxnAmount(uint256)", "updateMaxWalletAmount(uint256)", "setMaxTransactionAmount(uint256)", "removeLimits()"},
	KindUpgrade: {"upgradeTo(address)", "upgradeToAndCall(address,bytes)", "deprecate(address)"},
	KindReclaim: {"lock(uint256)", "unlock()"},
}

// Power is a privileged function the contract exposes.
type Power struct {
	Kind     string `json:"kind"`
	Function string `json:"function"`
	// Where is "bytecode" (selector found in the dispatcher) or "File.sol:123" (found in verified source).
	Where string `json:"where"`
	// CallableBy lists who can call it with effect at the checked block: "owner", "deployer", "anyone".
	CallableBy []string `json:"callable_by,omitempty"`
	// NoEffectFor lists callers whose call is accepted but changes nothing: scam contracts make privileged
	// functions return quietly for everyone but a hidden operator.
	NoEffectFor []string `json:"no_effect_for,omitempty"`
	// Probed is false when the function was not simulated (upgrade functions, parameter types it cannot fill).
	Probed bool `json:"probed"`
}

func selectorOf(sig string) evm.Selector { return evm.Selector(crypto.Keccak256([]byte(sig))[:4]) }

// findPowers lists the known privileged functions present in code, then adds source-only ones.
func findPowers(code evm.Code, src map[string]string) []Power {
	bySel := map[evm.Selector]*Power{}
	var order []evm.Selector
	add := func(p Power) {
		s := selectorOf(p.Function)
		if old, ok := bySel[s]; ok {
			if old.Where == "bytecode" && p.Where != "bytecode" {
				old.Where = p.Where
			}
			return
		}
		bySel[s] = &p
		order = append(order, s)
	}
	for _, kind := range Kinds {
		for _, sig := range knownPowers[kind] {
			if code.Has(selectorOf(sig)) {
				add(Power{Kind: kind, Function: sig, Where: "bytecode"})
			}
		}
	}
	for _, p := range scanSource(src) {
		// A public function always has its selector in the dispatcher; one that does not is dead or misparsed.
		if code.Has(selectorOf(p.Function)) {
			add(p)
		}
	}
	out := make([]Power, 0, len(order))
	for _, s := range order {
		out = append(out, *bySel[s])
	}
	sort.SliceStable(out, func(i, j int) bool { return slices.Index(Kinds, out[i].Kind) < slices.Index(Kinds, out[j].Kind) })
	return out
}

// sourceKinds maps words in a function name to a power kind, checked in order.
var sourceKinds = []struct {
	kind  string
	words []string
}{
	{KindSeize, []string{"destroyblackfunds", "wipe", "seize", "confiscate", "clawback"}},
	{KindBlacklist, []string{"blacklist", "blocklist", "bot", "sniper", "freeze", "ban"}},
	{KindMint, []string{"mint"}},
	{KindPause, []string{"pause"}},
	{KindLimits, []string{"maxtx", "maxtransaction", "maxwallet", "maxbuy", "maxsell", "limit"}},
	{KindFees, []string{"fee", "tax"}},
	{KindTrading, []string{"trading", "opentrade", "enabletrade", "starttrade", "launch"}},
	{KindUpgrade, []string{"upgrade", "setimplementation"}},
}

var funcDecl = regexp.MustCompile(`\bfunction\s+([A-Za-z_]\w*)\s*\(([^)]*)\)([^{;]*)`)

// scanSource finds state-changing external functions in verified source whose names suggest a privileged power.
func scanSource(files map[string]string) []Power {
	var out []Power
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		text := blankComments(files[path])
		skip := declarationsOnly(text)
		for _, m := range funcDecl.FindAllStringSubmatchIndex(text, -1) {
			if inSpans(skip, m[0]) {
				continue
			}
			name, params, mods := text[m[2]:m[3]], text[m[4]:m[5]], text[m[6]:m[7]]
			// "constant" is how Solidity before 0.5 spelled view.
			if containsWord(mods, "view", "pure", "constant", "internal", "private") {
				continue
			}
			kind := kindOfName(name)
			if kind == "" {
				continue
			}
			sig, ok := abiSignature(name, params)
			if !ok {
				continue
			}
			line := strings.Count(text[:m[0]], "\n") + 1
			out = append(out, Power{Kind: kind, Function: sig, Where: fmt.Sprintf("%s:%d", baseName(path), line)})
		}
	}
	return out
}

func kindOfName(name string) string {
	n := strings.ToLower(name)
	for _, k := range sourceKinds {
		for _, w := range k.words {
			if strings.Contains(n, w) {
				return k.kind
			}
		}
	}
	return ""
}

var elementary = regexp.MustCompile(`^(address|bool|string|bytes([1-9]|[12][0-9]|3[0-2])?|u?int(8|16|24|32|40|48|56|64|72|80|88|96|104|112|120|128|136|144|152|160|168|176|184|192|200|208|216|224|232|240|248|256)?)(\[\d*\])*$`)

// abiSignature turns a Solidity parameter list into the canonical signature used for the selector. Parameters of
// user-defined types (structs, interfaces, enums) cannot be mapped without the full AST, so those functions are
// skipped.
func abiSignature(name, params string) (string, bool) {
	var types []string
	for p := range strings.SplitSeq(params, ",") {
		fields := strings.Fields(p)
		if len(fields) == 0 {
			if strings.TrimSpace(params) == "" {
				break
			}
			return "", false
		}
		t := fields[0]
		if !elementary.MatchString(t) {
			return "", false
		}
		// "uint" and "int" are aliases for the 256-bit types; array suffixes stay as written.
		base, suffix := t, ""
		if i := strings.IndexByte(t, '['); i >= 0 {
			base, suffix = t[:i], t[i:]
		}
		switch base {
		case "uint":
			base = "uint256"
		case "int":
			base = "int256"
		}
		types = append(types, base+suffix)
	}
	return name + "(" + strings.Join(types, ",") + ")", true
}

func containsWord(s string, words ...string) bool {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' || r == '\r' || r == '(' || r == ')' }) {
		if slices.Contains(words, f) {
			return true
		}
	}
	return false
}

func baseName(path string) string {
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// Probe accounts: arbitrary addresses nobody controls, derived from fixed strings so runs are repeatable.
var (
	probeCaller = common.BytesToAddress(crypto.Keccak256([]byte("rugcheck probe caller"))[12:])
	probeTarget = common.BytesToAddress(crypto.Keccak256([]byte("rugcheck probe target"))[12:])
)

type caller struct {
	role string
	addr common.Address
}

// probePowers simulates each power as each caller and records who can use it. Nothing is sent to the chain. Each
// probe is one simulated block:
//
//	totalSupply, balanceOf(target), call, call again, totalSupply, balanceOf(target)
//
// The call had an effect if it emitted an event, moved the supply or the target's balance (mint, burn, seize), or
// changed any storage: then the identical repeat either fails (already paused) or costs a different amount of gas
// (it writes the value the first call left; refunds can make it either cheaper or dearer). Two identical calls on
// unchanged state cost exactly the same, so a call that matches in all of these was accepted and ignored. Functions
// with bool or integer parameters are probed twice (see probeArgs); a caller counts if either probe had an effect.
// Upgrade functions are not probed: they check the new implementation, so a probe would revert whoever sends it.
func probePowers(ctx context.Context, c simulator, token common.Address, supply *big.Int, powers []Power, callers []caller) error {
	readSupply := ethereum.CallMsg{To: &token, Data: pack(erc20ABI, "totalSupply")}
	readBalance := ethereum.CallMsg{To: &token, Data: pack(erc20ABI, "balanceOf", probeTarget)}
	var seqs [][]ethereum.CallMsg
	type slot struct{ power, caller int }
	var slots []slot
	var probed []int
	for i, p := range powers {
		if p.Kind == KindUpgrade {
			continue
		}
		sets := []probeArgs{{}}
		if hasSwitches(p.Function) {
			sets = append(sets, probeArgs{flip: true, large: supply})
		}
		var datas [][]byte
		for _, pa := range sets {
			if data, ok := probeCalldata(p.Function, pa); ok {
				datas = append(datas, data)
			}
		}
		if len(datas) == 0 {
			continue
		}
		probed = append(probed, i)
		for j, cl := range callers {
			for _, data := range datas {
				m := ethereum.CallMsg{From: cl.addr, To: &token, Data: data}
				seqs = append(seqs, []ethereum.CallMsg{readSupply, readBalance, m, m, readSupply, readBalance})
				slots = append(slots, slot{i, j})
			}
		}
	}
	if len(seqs) == 0 {
		return nil
	}
	res, err := c.Simulate(ctx, seqs)
	if err != nil {
		return err
	}
	type outcome struct{ accepted, effect bool }
	outcomes := map[slot]outcome{}
	for k, s := range slots {
		r := res[k]
		first, second := r[2], r[3]
		if !first.OK {
			continue
		}
		moved := !bytes.Equal(r[0].ReturnData, r[4].ReturnData) || !bytes.Equal(r[1].ReturnData, r[5].ReturnData)
		o := outcomes[s]
		o.accepted = true
		o.effect = o.effect || first.Logs > 0 || moved || !second.OK || first.GasUsed != second.GasUsed
		outcomes[s] = o
	}
	for _, i := range probed {
		powers[i].Probed = true
		for j, cl := range callers {
			switch o := outcomes[slot{i, j}]; {
			case o.effect:
				powers[i].CallableBy = append(powers[i].CallableBy, cl.role)
			case o.accepted:
				powers[i].NoEffectFor = append(powers[i].NoEffectFor, cl.role)
			}
		}
		if slices.Contains(powers[i].CallableBy, "anyone") {
			powers[i].CallableBy = []string{"anyone"}
		}
	}
	return nil
}

type simulator interface {
	Simulate(ctx context.Context, seqs [][]ethereum.CallMsg) ([][]chain.SimResult, error)
}

// probeArgs picks the arguments of one probe. The first set is harmless: a fresh address, amount 1, true. The second
// flips switches and uses a large amount, because a function can do nothing with the first set and still be live:
// setTradingEnabled(true) when trading is already on, or a max-transaction setter that rejects amounts below a
// share of supply.
type probeArgs struct {
	flip  bool
	large *big.Int
}

// probeCalldata encodes a call to sig with the given arguments, or reports false for parameter types it cannot fill.
func probeCalldata(sig string, pa probeArgs) ([]byte, bool) {
	open := strings.IndexByte(sig, '(')
	inner := strings.TrimSuffix(sig[open+1:], ")")
	var args abi.Arguments
	var vals []any
	if inner != "" {
		for t := range strings.SplitSeq(inner, ",") {
			typ, err := abi.NewType(t, "", nil)
			if err != nil {
				return nil, false
			}
			v, ok := probeValue(typ, pa)
			if !ok {
				return nil, false
			}
			args = append(args, abi.Argument{Type: typ})
			vals = append(vals, v)
		}
	}
	packed, err := args.Pack(vals...)
	if err != nil {
		return nil, false
	}
	sel := selectorOf(sig)
	return append(sel[:], packed...), true
}

// hasSwitches reports whether a second probe with flipped arguments would differ from the first.
func hasSwitches(sig string) bool {
	params := sig[strings.IndexByte(sig, '('):]
	return strings.Contains(params, "bool") || strings.Contains(params, "int")
}

func probeValue(t abi.Type, pa probeArgs) (any, bool) {
	switch t.T {
	case abi.AddressTy:
		return probeTarget, true
	case abi.BoolTy:
		return !pa.flip, true
	case abi.StringTy:
		return "x", true
	case abi.BytesTy:
		return []byte{}, true
	case abi.UintTy, abi.IntTy:
		n := big.NewInt(1)
		if pa.flip && pa.large != nil {
			// Clamp to the type: uint8 fee arguments get 2^7-1 or 2^8-1, not an overflow.
			limit := new(big.Int).Lsh(big.NewInt(1), uint(t.Size))
			if t.T == abi.IntTy {
				limit.Rsh(limit, 1)
			}
			n = new(big.Int).Sub(limit, big.NewInt(1))
			if pa.large.Cmp(n) < 0 {
				n = pa.large
			}
		}
		if t.GetType() == reflect.TypeFor[*big.Int]() {
			return n, true
		}
		return reflect.ValueOf(n.Uint64()).Convert(t.GetType()).Interface(), true
	case abi.FixedBytesTy:
		return reflect.New(t.GetType()).Elem().Interface(), true
	case abi.SliceTy:
		v, ok := probeValue(*t.Elem, pa)
		if !ok {
			return nil, false
		}
		s := reflect.MakeSlice(t.GetType(), 1, 1)
		s.Index(0).Set(reflect.ValueOf(v))
		return s.Interface(), true
	}
	return nil, false
}
