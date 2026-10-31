// Package explorer reads what block explorers know and the chain does not: verified source code and indexed token
// holders. Sources are Sourcify and Blockscout (keyless) and Etherscan's V2 API (only with ETHERSCAN_API_KEY, since
// it refuses keyless calls). Every answer is optional: callers fall back to the RPC or record a gap.
package explorer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
)

// Source is a contract's verified source code.
type Source struct {
	By    string // "sourcify", "etherscan" or "blockscout"
	Name  string // contract name
	Files map[string]string
}

// ErrNotVerified means every explorer consulted answered and none has verified source.
var ErrNotVerified = errors.New("source not verified")

// Client queries block explorers over HTTP.
type Client struct {
	HTTP         *http.Client
	EtherscanKey string
	// Base URLs, overridable in tests.
	SourcifyURL  string
	EtherscanURL string
}

// New returns a client that reads ETHERSCAN_API_KEY from the environment.
func New() *Client {
	return &Client{
		HTTP:         &http.Client{Timeout: 20 * time.Second},
		EtherscanKey: strings.TrimSpace(os.Getenv("ETHERSCAN_API_KEY")),
		SourcifyURL:  "https://sourcify.dev/server",
		EtherscanURL: "https://api.etherscan.io/v2/api",
	}
}

// Offline returns a client whose every lookup fails, as for a contract no explorer has indexed yet. A check of a past
// block uses it: explorers only know the present (today's holders, source verified since), which would leak the
// future into the check.
func Offline() *Client {
	return &Client{HTTP: &http.Client{Transport: offline{}}}
}

type offline struct{}

func (offline) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("explorer offline for a check of a past block")
}

// Source finds verified source for addr. It returns ErrNotVerified only when Etherscan (where most tokens are
// verified) was consulted; without a key, "not on Sourcify or Blockscout" is reported as an unknown, not a no.
func (c *Client) Source(ctx context.Context, net chain.Network, addr common.Address) (*Source, error) {
	var errs []error
	lookups := []struct {
		name string
		fn   func(context.Context, chain.Network, common.Address) (*Source, error)
		skip bool
	}{
		{"sourcify", c.sourcify, false},
		{"etherscan", c.etherscan, c.EtherscanKey == ""},
		{"blockscout", c.blockscout, net.Blockscout == ""},
	}
	for _, l := range lookups {
		if l.skip {
			continue
		}
		src, err := l.fn(ctx, net, addr)
		if err == nil {
			return src, nil
		}
		if !errors.Is(err, ErrNotVerified) {
			errs = append(errs, fmt.Errorf("%s: %w", l.name, err))
		}
	}
	switch {
	case len(errs) > 0:
		return nil, fmt.Errorf("verification unknown: %w", errors.Join(errs...))
	case c.EtherscanKey == "":
		return nil, errors.New("not verified on Sourcify or Blockscout; Etherscan not checked (set ETHERSCAN_API_KEY)")
	}
	return nil, ErrNotVerified
}

func (c *Client) sourcify(ctx context.Context, net chain.Network, addr common.Address) (*Source, error) {
	url := fmt.Sprintf("%s/v2/contract/%d/%s?fields=sources,compilation", c.SourcifyURL, net.ChainID, addr.Hex())
	var body struct {
		Match       *string `json:"match"`
		Compilation struct {
			Name string `json:"name"`
		} `json:"compilation"`
		Sources map[string]struct {
			Content string `json:"content"`
		} `json:"sources"`
	}
	status, err := c.getJSON(ctx, url, &body)
	if status == http.StatusNotFound {
		return nil, ErrNotVerified
	}
	if err != nil {
		return nil, err
	}
	if body.Match == nil || len(body.Sources) == 0 {
		return nil, ErrNotVerified
	}
	src := &Source{By: "sourcify", Name: body.Compilation.Name, Files: map[string]string{}}
	for path, f := range body.Sources {
		src.Files[path] = f.Content
	}
	return src, nil
}

func (c *Client) etherscan(ctx context.Context, net chain.Network, addr common.Address) (*Source, error) {
	url := fmt.Sprintf("%s?chainid=%d&module=contract&action=getsourcecode&address=%s&apikey=%s",
		c.EtherscanURL, net.ChainID, addr.Hex(), c.EtherscanKey)
	var body struct {
		Status  string          `json:"status"`
		Message string          `json:"message"`
		Result  json.RawMessage `json:"result"`
	}
	if _, err := c.getJSON(ctx, url, &body); err != nil {
		return nil, err
	}
	var rows []struct {
		SourceCode   string `json:"SourceCode"`
		ContractName string `json:"ContractName"`
	}
	if body.Status != "1" || json.Unmarshal(body.Result, &rows) != nil {
		var msg string
		_ = json.Unmarshal(body.Result, &msg)
		return nil, fmt.Errorf("%s: %s", body.Message, msg)
	}
	if len(rows) == 0 || rows[0].SourceCode == "" {
		return nil, ErrNotVerified
	}
	return &Source{By: "etherscan", Name: rows[0].ContractName, Files: etherscanFiles(rows[0].ContractName, rows[0].SourceCode)}, nil
}

// etherscanFiles unpacks Etherscan's SourceCode field: a single flattened file, a JSON map of files, or a
// standard-JSON input wrapped in an extra pair of braces.
func etherscanFiles(name, code string) map[string]string {
	trimmed := strings.TrimSpace(code)
	if strings.HasPrefix(trimmed, "{{") && strings.HasSuffix(trimmed, "}}") {
		trimmed = trimmed[1 : len(trimmed)-1]
	}
	type file struct {
		Content string `json:"content"`
	}
	var std struct {
		Sources map[string]file `json:"sources"`
	}
	if json.Unmarshal([]byte(trimmed), &std) == nil && len(std.Sources) > 0 {
		return contents(std.Sources, func(f file) string { return f.Content })
	}
	var multi map[string]file
	if json.Unmarshal([]byte(trimmed), &multi) == nil && len(multi) > 0 {
		return contents(multi, func(f file) string { return f.Content })
	}
	return map[string]string{name + ".sol": code}
}

func contents[T any](m map[string]T, get func(T) string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = get(v)
	}
	return out
}

func (c *Client) blockscout(ctx context.Context, net chain.Network, addr common.Address) (*Source, error) {
	var body struct {
		IsVerified        bool   `json:"is_verified"`
		Name              string `json:"name"`
		FilePath          string `json:"file_path"`
		SourceCode        string `json:"source_code"`
		AdditionalSources []struct {
			FilePath   string `json:"file_path"`
			SourceCode string `json:"source_code"`
		} `json:"additional_sources"`
	}
	status, err := c.getJSON(ctx, net.Blockscout+"/api/v2/smart-contracts/"+addr.Hex(), &body)
	if status == http.StatusNotFound {
		return nil, ErrNotVerified
	}
	if err != nil {
		return nil, err
	}
	if !body.IsVerified || body.SourceCode == "" {
		return nil, ErrNotVerified
	}
	path := body.FilePath
	if path == "" {
		path = body.Name + ".sol"
	}
	src := &Source{By: "blockscout", Name: body.Name, Files: map[string]string{path: body.SourceCode}}
	for _, f := range body.AdditionalSources {
		src.Files[f.FilePath] = f.SourceCode
	}
	return src, nil
}

// Creation returns the account that created addr (a factory contract, for contracts created by contracts) and the
// creating transaction.
func (c *Client) Creation(ctx context.Context, net chain.Network, addr common.Address) (common.Address, common.Hash, error) {
	if net.Blockscout == "" {
		return common.Address{}, common.Hash{}, errors.New("no explorer for " + net.Name)
	}
	var body struct {
		Creator *common.Address `json:"creator_address_hash"`
		Tx      *common.Hash    `json:"creation_transaction_hash"`
	}
	if _, err := c.getJSON(ctx, net.Blockscout+"/api/v2/addresses/"+addr.Hex(), &body); err != nil {
		return common.Address{}, common.Hash{}, err
	}
	if body.Creator == nil || body.Tx == nil {
		return common.Address{}, common.Hash{}, errors.New("blockscout does not know the creator")
	}
	return *body.Creator, *body.Tx, nil
}

// Holder is one entry of an explorer's token holder list.
type Holder struct {
	Address common.Address
	Balance *big.Int
}

// TopHolders returns the largest holders of an ERC-20 token (up to 50) and the explorer's holder count.
func (c *Client) TopHolders(ctx context.Context, net chain.Network, token common.Address) ([]Holder, int, error) {
	if net.Blockscout == "" {
		return nil, 0, errors.New("no explorer with a holder index for " + net.Name)
	}
	var info struct {
		HoldersCount string `json:"holders_count"`
	}
	if _, err := c.getJSON(ctx, net.Blockscout+"/api/v2/tokens/"+token.Hex(), &info); err != nil {
		return nil, 0, err
	}
	var page struct {
		Items []struct {
			Address struct {
				Hash common.Address `json:"hash"`
			} `json:"address"`
			Value string `json:"value"`
		} `json:"items"`
	}
	if _, err := c.getJSON(ctx, net.Blockscout+"/api/v2/tokens/"+token.Hex()+"/holders", &page); err != nil {
		return nil, 0, err
	}
	out := make([]Holder, 0, len(page.Items))
	for _, it := range page.Items {
		v, ok := new(big.Int).SetString(it.Value, 10)
		if !ok {
			return nil, 0, fmt.Errorf("blockscout: bad holder balance %q", it.Value)
		}
		out = append(out, Holder{Address: it.Address.Hash, Balance: v})
	}
	var count int
	_, _ = fmt.Sscan(info.HoldersCount, &count)
	return out, count, nil
}

// getJSON fetches url and decodes a JSON body into v. It returns the HTTP status even on error so callers can tell
// "not found" from "unreachable". An HTML body (a bot challenge page) is an error. Errors never contain the URL's
// path or query, where the Etherscan key travels.
func (c *Client) getJSON(ctx context.Context, url string, v any) (int, error) {
	status, err := c.fetchJSON(ctx, url, v)
	return status, c.clean(err)
}

func (c *Client) fetchJSON(ctx context.Context, url string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "rugcheck (+https://github.com/amirradjou/rugcheck)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return resp.StatusCode, fmt.Errorf("%s answered with an HTML page (bot challenge?)", req.URL.Host)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %s: %w", req.URL.Host, err)
	}
	return resp.StatusCode, nil
}

// clean cuts URLs in an error down to scheme and host (net/http and url.Parse quote the whole URL) and, as a last
// resort, blanks out the Etherscan key wherever it still appears.
func (c *Client) clean(err error) error {
	if err == nil {
		return nil
	}
	var ue *neturl.Error
	if errors.As(err, &ue) {
		err = fmt.Errorf("%s %s: %w", ue.Op, chain.Host(ue.URL), ue.Err)
	}
	if c.EtherscanKey != "" && strings.Contains(err.Error(), c.EtherscanKey) {
		return errors.New(strings.ReplaceAll(err.Error(), c.EtherscanKey, "REDACTED"))
	}
	return err
}
