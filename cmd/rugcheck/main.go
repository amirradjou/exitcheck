// Command rugcheck runs the rugcheck watcher, analyzer and Telegram bot (see README.md).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/buildinfo"
	"github.com/amirradjou/rugcheck/internal/chain"
	"github.com/amirradjou/rugcheck/internal/eval"
	"github.com/amirradjou/rugcheck/internal/explorer"
	"github.com/amirradjou/rugcheck/internal/facts"
	"github.com/amirradjou/rugcheck/internal/sim"
)

const usage = `usage: rugcheck <command> [flags]

commands:
  facts <address>   contract, owner powers, holders and liquidity of a token
  sim <address>     buy then sell the token on a local fork (needs Foundry's anvil)
  eval <cases.json> check facts against hand-labelled tokens (testdata/tokens.json)
  version           print the version

Run "rugcheck <command> -h" for a command's flags.

environment:
  RUGCHECK_RPC_ETHEREUM, RUGCHECK_RPC_BASE   RPC endpoints (default: free public RPCs with archive state)
  ETHERSCAN_API_KEY                          optional; lets "verified source" answer no instead of unknown
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(buildinfo.String())
	case "facts":
		err = runFacts(ctx, os.Args[2:])
	case "sim":
		err = runSim(ctx, os.Args[2:])
	case "eval":
		err = runEval(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		var u usageError
		if errors.As(err, &u) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "rugcheck:", err)
		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func runFacts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("facts", flag.ContinueOnError)
	chainName := fs.String("chain", "ethereum", "chain: ethereum or base")
	asJSON := fs.Bool("json", false, "print JSON instead of the text report")
	timeout := fs.Duration("timeout", 3*time.Minute, "give up after this long")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: rugcheck facts [-chain ethereum|base] [-json] <token address>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(interleave(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError{err.Error()}
	}
	if fs.NArg() != 1 || !common.IsHexAddress(fs.Arg(0)) {
		fs.Usage()
		return usageError{"rugcheck facts: need one token address"}
	}
	net, err := chain.ByName(*chainName)
	if err != nil {
		return usageError{err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	c, err := chain.Dial(ctx, net)
	if err != nil {
		return err
	}
	defer c.Close()
	f, err := facts.Gather(ctx, c, explorer.New(), common.HexToAddress(fs.Arg(0)))
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(f)
	}
	return facts.Render(os.Stdout, f)
}

func runSim(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	chainName := fs.String("chain", "ethereum", "chain: ethereum or base")
	block := fs.Uint64("block", 0, "fork this block instead of the latest (uses the archive RPC)")
	asJSON := fs.Bool("json", false, "print JSON instead of the text report")
	timeout := fs.Duration("timeout", 5*time.Minute, "give up after this long")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: rugcheck sim [-chain ethereum|base] [-block N] [-json] <token address>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(interleave(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError{err.Error()}
	}
	if fs.NArg() != 1 || !common.IsHexAddress(fs.Arg(0)) {
		fs.Usage()
		return usageError{"rugcheck sim: need one token address"}
	}
	net, err := chain.ByName(*chainName)
	if err != nil {
		return usageError{err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	c, err := chain.Dial(ctx, net)
	if err != nil {
		return err
	}
	defer c.Close()
	r, err := sim.Run(ctx, c, common.HexToAddress(fs.Arg(0)), sim.Options{Block: *block})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	return sim.Render(os.Stdout, r)
}

func runEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	markdown := fs.Bool("markdown", false, "print a Markdown table of the results")
	pause := fs.Duration("pause", 2*time.Second, "wait between tokens, to stay under free RPC rate limits")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: rugcheck eval [-markdown] <cases.json>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(interleave(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError{err.Error()}
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return usageError{"rugcheck eval: need one cases file"}
	}
	cases, err := eval.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	clients := map[string]*chain.Client{}
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()
	ex := explorer.New()
	var rows []string
	passed, checks, failedChecks := 0, 0, 0
	for i, tc := range cases {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(*pause):
			}
		}
		c, ok := clients[tc.Chain]
		if !ok {
			net, err := chain.ByName(tc.Chain)
			if err != nil {
				return fmt.Errorf("%s: %w", tc.Name, err)
			}
			if c, err = chain.Dial(ctx, net); err != nil {
				return err
			}
			clients[tc.Chain] = c
		}
		tctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		f, err := facts.Gather(tctx, c, ex, tc.Address)
		cancel()
		var result string
		var failed int
		var elapsed time.Duration
		if err != nil {
			result, failed = "error: "+err.Error(), 1
		} else {
			cs := eval.CheckFacts(tc.Expect, f)
			checks += len(cs)
			for _, ch := range cs {
				if !ch.OK {
					failed++
				}
			}
			result, elapsed = eval.Summary(cs), f.Elapsed
		}
		failedChecks += failed
		status := "ok  "
		if failed == 0 {
			passed++
		} else {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "%s %-5s %-8s %-10s %s  %4.1fs  %s\n", status, tc.Label, tc.Chain, tc.Name, tc.Address.Hex(), elapsed.Seconds(), result)
		rows = append(rows, fmt.Sprintf("| %s | %s | %s | `%s` | %s | %.1f s |", tc.Label, tc.Chain, tc.Name, tc.Address.Hex(), map[bool]string{true: "correct", false: "WRONG: " + result}[failed == 0], elapsed.Seconds()))
	}
	fmt.Fprintf(os.Stderr, "\n%d/%d tokens correct; %d/%d checks passed\n", passed, len(cases), checks-failedChecks, checks)
	if *markdown {
		fmt.Println("| label | chain | token | address | facts | time |")
		fmt.Println("|---|---|---|---|---|---|")
		for _, r := range rows {
			fmt.Println(r)
		}
	}
	if passed != len(cases) {
		return fmt.Errorf("%d of %d tokens have wrong facts", len(cases)-passed, len(cases))
	}
	return nil
}

// valueFlags are the flags that take a value as the next argument.
var valueFlags = map[string]bool{"chain": true, "timeout": true, "pause": true, "block": true}

// interleave moves flags ahead of positional arguments, so "facts 0xabc -json" works like "facts -json 0xabc".
func interleave(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		case len(a) > 1 && a[0] == '-':
			flags = append(flags, a)
			if valueFlags[strings.TrimLeft(a, "-")] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		default:
			rest = append(rest, a)
		}
	}
	return append(flags, rest...)
}
