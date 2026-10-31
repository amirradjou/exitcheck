package watch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/amirradjou/rugcheck/internal/chain"
)

// logServer answers eth_getLogs like a node whose head is at head: as publicnode does past it (an error), or as
// mevblocker does (an empty list). It records the ranges it was asked for.
type logServer struct {
	head       uint64
	emptyAhead bool
	mu         sync.Mutex
	ranges     [][2]uint64
}

func (s *logServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var q struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &q)
		out := map[string]any{"jsonrpc": "2.0", "id": q.ID}
		switch q.Method {
		case "eth_chainId":
			out["result"] = hexutil.EncodeUint64(uint64(chain.Ethereum.ChainID))
		case "eth_getLogs":
			var f struct{ FromBlock, ToBlock string }
			_ = json.Unmarshal(q.Params[0], &f)
			from, _ := hexutil.DecodeUint64(f.FromBlock)
			to, _ := hexutil.DecodeUint64(f.ToBlock)
			s.mu.Lock()
			s.ranges = append(s.ranges, [2]uint64{from, to})
			s.mu.Unlock()
			if to > s.head && !s.emptyAhead {
				out["error"] = map[string]any{"code": -32602, "message": "block range extends beyond current head block"}
			} else {
				out["result"] = []any{}
			}
		default:
			out["error"] = map[string]any{"code": -32601, "message": "unexpected " + q.Method}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestABlockNoNodeHasYetIsRetriedNotSkipped(t *testing.T) {
	// The latest-state node is a block behind and says so; the archive node is behind too and answers "no logs".
	latest := &logServer{head: 99}
	archive := &logServer{head: 99, emptyAhead: true}
	c, err := chain.DialURLs(context.Background(), chain.Ethereum, latest.start(t), archive.start(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if pools, err := NewRPC(c).Created(context.Background(), 100, 100); err == nil {
		t.Fatalf("Created = %v, nil: an empty answer for a block no node had would skip its launches", pools)
	}
	if len(archive.ranges) != 0 {
		t.Errorf("the archive was asked about the head's block: %v", archive.ranges)
	}
}

func TestLongRangesReadOldBlocksFromTheArchiveAndRecentOnesLive(t *testing.T) {
	latest := &logServer{head: 10_000}
	archive := &logServer{head: 10_000, emptyAhead: true}
	c, err := chain.DialURLs(context.Background(), chain.Ethereum, latest.start(t), archive.start(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := NewRPC(c).Created(context.Background(), 5_000, 10_000); err != nil {
		t.Fatal(err)
	}
	for _, r := range archive.ranges {
		if r[1] > 10_000-liveRange {
			t.Errorf("archive asked for %v, within %d blocks of the range's end", r, liveRange)
		}
	}
	if len(latest.ranges) != 1 || latest.ranges[0] != [2]uint64{10_000 - liveRange + 1, 10_000} {
		t.Errorf("latest-state RPC asked for %v, want the last %d blocks", latest.ranges, liveRange)
	}
}
