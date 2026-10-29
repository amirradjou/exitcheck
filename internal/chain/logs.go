package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
)

// ErrBudget is returned when a log scan needs more requests than it was allowed.
var ErrBudget = errors.New("log scan budget exhausted")

// LogScan reads logs over a block range in windows. Free RPCs cap the range of one eth_getLogs call at different
// sizes (and some report it as a generic 400), so the window halves on any error and grows back after successes.
type LogScan struct {
	Query    ethereum.FilterQuery // FromBlock/ToBlock are ignored; the scan sets them
	From, To uint64
	// MaxRequests bounds the number of eth_getLogs calls; 0 means no bound.
	MaxRequests int
}

const minLogWindow = 10

// ScanLogs calls fn with each window's logs in block order. It stops early when fn returns an error.
func (c *Client) ScanLogs(ctx context.Context, s LogScan, fn func([]types.Log) error) error {
	maxWindow := c.Net.LogWindow
	if maxWindow == 0 {
		maxWindow = 1_000
	}
	window, requests := maxWindow, 0
	if s.To >= s.From && s.MaxRequests > 0 && (s.To-s.From)/maxWindow+1 > uint64(s.MaxRequests) {
		return fmt.Errorf("%w: %d blocks need at least %d requests of %d blocks, budget is %d",
			ErrBudget, s.To-s.From+1, (s.To-s.From)/maxWindow+1, maxWindow, s.MaxRequests)
	}
	for from := s.From; from <= s.To; {
		to := min(from+window-1, s.To)
		if s.MaxRequests > 0 && requests >= s.MaxRequests {
			return fmt.Errorf("%w after %d requests at block %d of %d..%d", ErrBudget, requests, from, s.From, s.To)
		}
		requests++
		q := s.Query
		q.FromBlock, q.ToBlock = new(big.Int).SetUint64(from), new(big.Int).SetUint64(to)
		var logs []types.Log
		err := c.archiveDo(ctx, 1, func() (err error) { logs, err = c.archiveEth.FilterLogs(ctx, q); return err })
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if window <= minLogWindow {
				return fmt.Errorf("eth_getLogs %d..%d: %w", from, to, err)
			}
			window = max(window/2, minLogWindow)
			continue
		}
		if err := fn(logs); err != nil {
			return err
		}
		from = to + 1
		window = min(window*2, maxWindow)
	}
	return nil
}
