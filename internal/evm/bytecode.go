// Package evm reads facts out of runtime bytecode: which function selectors the dispatcher knows, which opcodes
// appear, and whether the code is a minimal proxy. It works on unverified contracts, which is where scams live.
package evm

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
)

const (
	opPush1        = 0x60
	opPush3        = 0x62
	opPush4        = 0x63
	opPush32       = 0x7f
	opCallcode     = 0xf2
	opDelegatecall = 0xf4
	opSelfdestruct = 0xff
)

// Selector is a 4-byte function selector.
type Selector [4]byte

// Code is analysed runtime bytecode.
type Code struct {
	// Selectors are the 4-byte constants the code pushes. Solidity's dispatcher compares calldata against one PUSH4
	// per external function (PUSH3 when the selector starts with a zero byte), so a selector that is absent here is
	// not a function of this contract. Other 4-byte constants land here too; they rarely collide with real selectors.
	Selectors    map[Selector]bool
	Delegatecall bool
	Selfdestruct bool
	Callcode     bool
}

// Analyze walks the opcodes of runtime code, skipping push data. Selectors come from the whole code: a contract
// cannot hide a second dispatcher behind a fake metadata trailer. Opcode flags skip the Solidity metadata, whose bytes
// would otherwise read as opcodes.
func Analyze(code []byte) Code {
	c := Code{Selectors: map[Selector]bool{}}
	body := len(stripMetadata(code))
	for pc := 0; pc < len(code); pc++ {
		op := code[pc]
		switch {
		case op == opPush4 && pc+4 < len(code):
			c.Selectors[Selector(code[pc+1:pc+5])] = true
		case op == opPush3 && pc+3 < len(code):
			c.Selectors[Selector{0, code[pc+1], code[pc+2], code[pc+3]}] = true
		case pc >= body:
		case op == opDelegatecall:
			c.Delegatecall = true
		case op == opSelfdestruct:
			c.Selfdestruct = true
		case op == opCallcode:
			c.Callcode = true
		}
		if op >= opPush1 && op <= opPush32 {
			pc += int(op - opPush1 + 1)
		}
	}
	return c
}

// Has reports whether the code knows the selector.
func (c Code) Has(s Selector) bool { return c.Selectors[s] }

// Merge adds another code's selectors and opcode flags (a proxy and its implementation act as one contract).
func (c Code) Merge(o Code) Code {
	out := Code{Selectors: make(map[Selector]bool, len(c.Selectors)+len(o.Selectors))}
	for s := range c.Selectors {
		out.Selectors[s] = true
	}
	for s := range o.Selectors {
		out.Selectors[s] = true
	}
	out.Delegatecall = c.Delegatecall || o.Delegatecall
	out.Selfdestruct = c.Selfdestruct || o.Selfdestruct
	out.Callcode = c.Callcode || o.Callcode
	return out
}

// maxMetadata is the longest metadata trailer solc writes (an ipfs hash, the compiler version and the experimental
// flag fit well under it).
const maxMetadata = 128

// stripMetadata drops the CBOR metadata solc appends to runtime code; its last two bytes give its length. The
// trailer is only dropped when it decodes completely as solc's metadata map (see solcMetadata): anything else is
// treated as code, so a contract cannot hide opcodes in a trailer that merely starts like metadata.
func stripMetadata(code []byte) []byte {
	if len(code) < 2 {
		return code
	}
	n := int(code[len(code)-2])<<8 | int(code[len(code)-1])
	if n < 2 || n > maxMetadata || n+2 > len(code) {
		return code
	}
	if !solcMetadata(code[len(code)-2-n : len(code)-2]) {
		return code
	}
	return code[:len(code)-2-n]
}

// solcMetadata reports whether b is exactly a CBOR map of solc's metadata entries: "ipfs" (34-byte multihash),
// "bzzr0"/"bzzr1" (32-byte swarm hash), "solc" (3 version bytes, or a version string) and "experimental" (bool).
func solcMetadata(b []byte) bool {
	if len(b) == 0 || b[0] < 0xa1 || b[0] > 0xa5 {
		return false
	}
	pairs, i := int(b[0]-0xa0), 1
	seen := map[string]bool{}
	for range pairs {
		// Key: a short text string.
		if i >= len(b) || b[i] < 0x60 || b[i] > 0x77 {
			return false
		}
		kl := int(b[i] - 0x60)
		if i+1+kl > len(b) {
			return false
		}
		key := string(b[i+1 : i+1+kl])
		i += 1 + kl
		if seen[key] || i >= len(b) {
			return false
		}
		seen[key] = true
		// Value: the type and length solc writes for that key.
		var vl int
		switch key {
		case "ipfs", "bzzr0", "bzzr1":
			want := map[string]int{"ipfs": 34, "bzzr0": 32, "bzzr1": 32}[key]
			if b[i] != 0x58 || i+1 >= len(b) || int(b[i+1]) != want {
				return false
			}
			vl = 2 + want
		case "solc":
			switch {
			case b[i] == 0x43: // 3 bytes: major, minor, patch
				vl = 4
			case b[i] >= 0x60 && b[i] <= 0x77: // version string
				vl = 1 + int(b[i]-0x60)
			default:
				return false
			}
		case "experimental":
			if b[i] != 0xf4 && b[i] != 0xf5 {
				return false
			}
			vl = 1
		default:
			return false
		}
		if i+vl > len(b) {
			return false
		}
		i += vl
	}
	return i == len(b)
}

var (
	minimalProxyPrefix = common.FromHex("363d3d373d3d3d363d73")
	minimalProxySuffix = common.FromHex("5af43d82803e903d91602b57fd5bf3")
)

// MinimalProxyTarget returns the implementation of an EIP-1167 minimal proxy.
func MinimalProxyTarget(code []byte) (common.Address, bool) {
	if len(code) != len(minimalProxyPrefix)+20+len(minimalProxySuffix) ||
		!bytes.HasPrefix(code, minimalProxyPrefix) || !bytes.HasSuffix(code, minimalProxySuffix) {
		return common.Address{}, false
	}
	return common.BytesToAddress(code[len(minimalProxyPrefix) : len(minimalProxyPrefix)+20]), true
}

var delegationPrefix = []byte{0xef, 0x01, 0x00}

// DelegatedTo returns the delegate of an EIP-7702 account. Such an account is a wallet (it signs transactions) whose
// code is a 23-byte pointer to another contract, so "has code" no longer means "is a contract".
func DelegatedTo(code []byte) (common.Address, bool) {
	if len(code) != 23 || !bytes.HasPrefix(code, delegationPrefix) {
		return common.Address{}, false
	}
	return common.BytesToAddress(code[3:]), true
}

// IsContract reports whether code belongs to a contract rather than a wallet (with or without a 7702 delegation).
func IsContract(code []byte) bool {
	_, delegated := DelegatedTo(code)
	return len(code) > 0 && !delegated
}
