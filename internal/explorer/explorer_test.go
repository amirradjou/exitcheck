package explorer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/chain"
)

func TestEtherscanFilesUnpacksAllThreeShapes(t *testing.T) {
	flat := etherscanFiles("Tok", "pragma solidity ^0.8.0; contract Tok {}")
	if flat["Tok.sol"] == "" {
		t.Errorf("flat source: %v", flat)
	}
	multi := etherscanFiles("Tok", `{"a.sol":{"content":"A"},"b.sol":{"content":"B"}}`)
	if multi["a.sol"] != "A" || multi["b.sol"] != "B" {
		t.Errorf("multi-file source: %v", multi)
	}
	std := etherscanFiles("Tok", `{{"language":"Solidity","sources":{"src/T.sol":{"content":"T"}}}}`)
	if std["src/T.sol"] != "T" {
		t.Errorf("standard-json source: %v", std)
	}
}

// fakeExplorers serves Sourcify, Etherscan and Blockscout from one server.
func fakeExplorers(t *testing.T, sourcify, etherscan, blockscout http.HandlerFunc) (*Client, chain.Network) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/sourcify/"):
			sourcify(w, r)
		case strings.HasPrefix(r.URL.Path, "/etherscan"):
			etherscan(w, r)
		default:
			blockscout(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.SourcifyURL = srv.URL + "/sourcify"
	c.EtherscanURL = srv.URL + "/etherscan"
	net := chain.Ethereum
	net.Blockscout = srv.URL
	return c, net
}

func notFound(w http.ResponseWriter, _ *http.Request) { http.Error(w, `{"message":"Not found"}`, 404) }

func TestSourceWithoutEtherscanKeyIsUnknownNotNo(t *testing.T) {
	c, net := fakeExplorers(t, notFound, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("etherscan called without a key")
	}, notFound)
	c.EtherscanKey = ""
	_, err := c.Source(context.Background(), net, common.Address{1})
	if err == nil || errors.Is(err, ErrNotVerified) || !strings.Contains(err.Error(), "ETHERSCAN_API_KEY") {
		t.Fatalf("err = %v, want an unknown that names ETHERSCAN_API_KEY", err)
	}
}

func TestSourceNotVerifiedWhenEtherscanSaysSo(t *testing.T) {
	c, net := fakeExplorers(t, notFound, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "k" || r.URL.Query().Get("chainid") != "1" {
			t.Errorf("etherscan query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"status":"1","message":"OK","result":[{"SourceCode":"","ContractName":""}]}`))
	}, notFound)
	c.EtherscanKey = "k"
	if _, err := c.Source(context.Background(), net, common.Address{1}); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("err = %v, want ErrNotVerified", err)
	}
}

func TestSourceFallsThroughToBlockscoutAndRejectsHTML(t *testing.T) {
	c, net := fakeExplorers(t, notFound, notFound, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"is_verified":true,"name":"Tok","file_path":"Tok.sol","source_code":"contract Tok {}",
			"additional_sources":[{"file_path":"lib/Ownable.sol","source_code":"contract Ownable {}"}]}`))
	})
	c.EtherscanKey = ""
	src, err := c.Source(context.Background(), net, common.Address{1})
	if err != nil {
		t.Fatal(err)
	}
	if src.By != "blockscout" || src.Name != "Tok" || len(src.Files) != 2 {
		t.Fatalf("source = %+v", src)
	}

	c, net = fakeExplorers(t, notFound, notFound, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><title>Just a moment...</title>`))
	})
	c.EtherscanKey = ""
	if _, err := c.Source(context.Background(), net, common.Address{1}); err == nil || !strings.Contains(err.Error(), "HTML") {
		t.Fatalf("err = %v, want the HTML challenge reported", err)
	}
}

func TestErrorsNeverCarryTheEtherscanKey(t *testing.T) {
	t.Setenv("ETHERSCAN_API_KEY", "SECRETKEY\r") // a CRLF .env leaves a carriage return behind
	c := New()
	if c.EtherscanKey != "SECRETKEY" {
		t.Fatalf("key not trimmed: %q", c.EtherscanKey)
	}
	// A key with a control character makes the URL unparsable; url.Error would quote it in full.
	c.EtherscanKey = "SECRET\x7fKEY"
	c.EtherscanURL = "http://127.0.0.1:1/api"
	_, err := c.etherscan(context.Background(), chain.Ethereum, common.Address{1})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v, want an error without the key", err)
	}
	// Unreachable host: the transport error quotes the URL, which must be cut to the host.
	c.EtherscanKey = "SECRETKEY"
	_, err = c.etherscan(context.Background(), chain.Ethereum, common.Address{1})
	if err == nil || strings.Contains(err.Error(), "SECRETKEY") || strings.Contains(err.Error(), "apikey") {
		t.Fatalf("err = %v, want an error without the key", err)
	}
}
