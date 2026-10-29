package chain

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

// Creation is where and by whom a contract was deployed.
type Creation struct {
	Block uint64
	Tx    common.Hash
	// Deployer is the account that sent the creating transaction.
	Deployer common.Address
	// Factory is the contract the transaction called when the contract was created by another contract (CREATE2
	// launchpads, factories); nil for a direct deployment.
	Factory *common.Address
}

// CreationBlock finds the block a contract was deployed in by searching archive state. A contract's nonce becomes 1
// when it is created (EIP-161), so the search reads nonces, which are cheaper than code; contracts created before
// EIP-161 keep nonce 0 and are found by their code instead.
func (c *Client) CreationBlock(ctx context.Context, addr common.Address, head uint64) (uint64, error) {
	method := "eth_getTransactionCount"
	nonce, err := c.existsAt(ctx, method, addr, []uint64{head})
	if err != nil {
		return 0, err
	}
	if !nonce[0] {
		method = "eth_getCode"
		code, err := c.existsAt(ctx, method, addr, []uint64{head})
		if err != nil {
			return 0, err
		}
		if !code[0] {
			return 0, fmt.Errorf("%s has no code at block %d", addr, head)
		}
	}
	// Contracts in the genesis state (Base's predeploys) were never created by a transaction.
	if genesis, err := c.existsAt(ctx, method, addr, []uint64{0}); err != nil {
		return 0, err
	} else if genesis[0] {
		return 0, fmt.Errorf("%s is in the genesis state; it was not deployed by a transaction", addr)
	}
	// Invariant: absent at lo, present at hi. Each round checks fanout blocks in one batch; under pacing, a plain
	// binary search needs the fewest requests.
	lo, hi := uint64(0), head
	fanout := uint64(16)
	if c.pace != nil {
		fanout = 1
	}
	for hi-lo > 1 {
		var points []uint64
		for i := uint64(1); i <= fanout; i++ {
			p := lo + (hi-lo)*i/(fanout+1)
			if p > lo && p < hi && (len(points) == 0 || p != points[len(points)-1]) {
				points = append(points, p)
			}
		}
		if len(points) == 0 {
			break
		}
		present, err := c.existsAt(ctx, method, addr, points)
		if err != nil {
			return 0, err
		}
		newLo, newHi := lo, hi
		for i, p := range points {
			if present[i] {
				newHi = p
				break
			}
			newLo = p
		}
		lo, hi = newLo, newHi
	}
	return hi, nil
}

// existsAt reports, for each block, whether addr has a non-zero nonce (or non-empty code) there.
func (c *Client) existsAt(ctx context.Context, method string, addr common.Address, blocks []uint64) ([]bool, error) {
	elems := make([]rpc.BatchElem, len(blocks))
	nonces := make([]hexutil.Uint64, len(blocks))
	codes := make([]hexutil.Bytes, len(blocks))
	for i, b := range blocks {
		elems[i] = rpc.BatchElem{Method: method, Args: []any{addr, hexutil.EncodeUint64(b)}}
		if method == "eth_getCode" {
			elems[i].Result = &codes[i]
		} else {
			elems[i].Result = &nonces[i]
		}
	}
	if err := c.archiveBatch(ctx, elems); err != nil {
		return nil, err
	}
	out := make([]bool, len(blocks))
	for i, e := range elems {
		if e.Error != nil {
			return nil, fmt.Errorf("%s at block %d: %w", method, blocks[i], e.Error)
		}
		out[i] = nonces[i] > 0 || len(codes[i]) > 0
	}
	return out, nil
}

type receiptJSON struct {
	TransactionHash common.Hash     `json:"transactionHash"`
	From            common.Address  `json:"from"`
	To              *common.Address `json:"to"`
	ContractAddress *common.Address `json:"contractAddress"`
	Logs            []struct {
		Address common.Address `json:"address"`
		Topics  []common.Hash  `json:"topics"`
	} `json:"logs"`
}

// Events a contract emits from its constructor: an ERC-20 mint and an Ownable handover, both from the zero address.
var (
	transferTopic    = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	ownershipTopic   = crypto.Keccak256Hash([]byte("OwnershipTransferred(address,address)"))
	zeroAddressTopic = common.Hash{}
)

// Creator finds the transaction that deployed addr in block. A direct deployment shows up as the receipt's
// contractAddress. A contract created by a factory is found through a "birth" event it emitted in that block: a
// Transfer or OwnershipTransferred from the zero address, which constructors emit. Any later transaction in the
// same block can also make the new contract log, so a plain log match could name a stranger as the deployer; without
// a birth event the caller should ask an explorer, which traces internal creations.
func (c *Client) Creator(ctx context.Context, addr common.Address, block uint64) (*Creation, error) {
	var receipts []receiptJSON
	err := c.archiveDo(ctx, 1, func() error {
		return c.archive.CallContext(ctx, &receipts, "eth_getBlockReceipts", hexutil.EncodeUint64(block))
	})
	if err != nil {
		return nil, fmt.Errorf("eth_getBlockReceipts %d: %w", block, err)
	}
	for _, r := range receipts {
		if r.ContractAddress != nil && *r.ContractAddress == addr {
			return &Creation{Block: block, Tx: r.TransactionHash, Deployer: r.From}, nil
		}
	}
	for _, r := range receipts {
		for _, l := range r.Logs {
			if l.Address == addr && len(l.Topics) >= 2 && l.Topics[1] == zeroAddressTopic &&
				(l.Topics[0] == transferTopic || l.Topics[0] == ownershipTopic) {
				return &Creation{Block: block, Tx: r.TransactionHash, Deployer: r.From, Factory: r.To}, nil
			}
		}
	}
	return nil, errors.New("no transaction in the creation block shows the contract being created (factory-made without a constructor event)")
}

// TxSender returns the sender and block of a mined transaction.
func (c *Client) TxSender(ctx context.Context, hash common.Hash) (common.Address, uint64, error) {
	var tx *struct {
		From        common.Address `json:"from"`
		BlockNumber *hexutil.Big   `json:"blockNumber"`
	}
	// Old transactions count as archive data on some providers.
	err := c.archiveDo(ctx, 1, func() error { return c.archive.CallContext(ctx, &tx, "eth_getTransactionByHash", hash) })
	if err != nil {
		return common.Address{}, 0, err
	}
	if tx == nil || tx.BlockNumber == nil {
		return common.Address{}, 0, fmt.Errorf("transaction %s not found or not mined", hash)
	}
	return tx.From, tx.BlockNumber.ToInt().Uint64(), nil
}
