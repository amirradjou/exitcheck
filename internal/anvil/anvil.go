// Package anvil runs a throwaway local fork of a live chain. Simulated trades happen there and nowhere else:
// nothing rugcheck does on a fork is ever sent to the real network.
package anvil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Fork says what to fork.
type Fork struct {
	URL   string // RPC that serves state at Block
	Block uint64
	// ComputeUnits caps anvil's requests to URL (anvil's --compute-units-per-second; a storage read is about 17
	// units). 0 means no cap.
	ComputeUnits int
}

// Node is a running anvil fork. Every account can send transactions without a key (--auto-impersonate).
type Node struct {
	URL    string
	RPC    *rpc.Client
	Client *ethclient.Client
	cmd    *exec.Cmd
	done   chan struct{} // closed when the process exits
	stderr *lockedBuffer
}

// FindBinary looks for anvil on PATH, then in Foundry's default install directory.
func FindBinary() (string, error) {
	if p, err := exec.LookPath("anvil"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".foundry", "bin", "anvil")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("anvil not found on PATH or in ~/.foundry/bin; install Foundry from https://getfoundry.sh")
}

// Start forks f on a free local port and waits until the fork answers.
func Start(ctx context.Context, f Fork) (*Node, error) {
	bin, err := FindBinary()
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := []string{
		"--port", fmt.Sprint(port),
		"--fork-url", f.URL,
		"--fork-block-number", fmt.Sprint(f.Block),
		"--auto-impersonate",
		"--retries", "10",
		"--silent",
	}
	if f.ComputeUnits > 0 {
		args = append(args, "--compute-units-per-second", fmt.Sprint(f.ComputeUnits))
	} else {
		args = append(args, "--no-rate-limit")
	}
	cmd := exec.Command(bin, args...)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start anvil: %w", err)
	}
	n := &Node{URL: fmt.Sprintf("http://127.0.0.1:%d", port), cmd: cmd, done: make(chan struct{}), stderr: stderr}
	go func() {
		_ = cmd.Wait()
		close(n.done)
	}()

	deadline := time.Now().Add(60 * time.Second)
	for {
		if err := n.connect(ctx); err == nil {
			return n, nil
		} else if time.Now().After(deadline) || n.exited() {
			n.Close()
			return nil, fmt.Errorf("anvil fork of block %d did not come up: %v %s", f.Block, err, strings.TrimSpace(stderr.String()))
		}
		select {
		case <-ctx.Done():
			n.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (n *Node) connect(ctx context.Context) error {
	c, err := rpc.DialContext(ctx, n.URL)
	if err != nil {
		return err
	}
	var id hexutil.Big
	if err := c.CallContext(ctx, &id, "eth_chainId"); err != nil {
		c.Close()
		return err
	}
	n.RPC = c
	n.Client = ethclient.NewClient(c)
	return nil
}

func (n *Node) exited() bool {
	select {
	case <-n.done:
		return true
	default:
		return false
	}
}

// Close stops the fork.
func (n *Node) Close() {
	if n.RPC != nil {
		n.RPC.Close()
	}
	if n.cmd != nil && n.cmd.Process != nil {
		_ = n.cmd.Process.Kill()
		<-n.done
	}
}

// Snapshot records the fork's state. A snapshot id can be reverted to once.
func (n *Node) Snapshot(ctx context.Context) (string, error) {
	var id string
	err := n.RPC.CallContext(ctx, &id, "evm_snapshot")
	return id, err
}

// Revert restores a snapshot taken with Snapshot.
func (n *Node) Revert(ctx context.Context, id string) error {
	var ok bool
	if err := n.RPC.CallContext(ctx, &ok, "evm_revert", id); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("evm_revert %s: snapshot not found", id)
	}
	return nil
}

// SetBalance overwrites an account's ETH balance.
func (n *Node) SetBalance(ctx context.Context, addr common.Address, wei *big.Int) error {
	return n.RPC.CallContext(ctx, nil, "anvil_setBalance", addr, hexutil.EncodeBig(wei))
}

// Advance moves the clock forward and mines blocks, so time- and block-based cooldowns pass.
func (n *Node) Advance(ctx context.Context, d time.Duration, blocks int) error {
	if err := n.RPC.CallContext(ctx, nil, "evm_increaseTime", int64(d/time.Second)); err != nil {
		return err
	}
	for range max(blocks, 1) {
		if err := n.RPC.CallContext(ctx, nil, "evm_mine"); err != nil {
			return err
		}
	}
	return nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// lockedBuffer collects anvil's stderr for error messages; the process writes while Start reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
