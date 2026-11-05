package check

import (
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/rugcheck/internal/verdict"
)

func TestRender(t *testing.T) {
	r := &Report{
		Chain: "base", Address: common.HexToAddress("0xEF77fB35d6e0503faf3cbe9565193195Aa4c6eF8"),
		Name: "ZiG", Symbol: "ZiG", Block: 7, Elapsed: 9700 * time.Millisecond,
		Verdict: verdict.Verdict{
			Level: verdict.Scam, Headline: "Honeypot: you can buy, but you cannot sell",
			Reasons: []verdict.Reason{
				// A revert reason is chosen by the token's author: control characters must not reach the chat.
				{Level: verdict.Scam, Text: "the sale failed (\x1b[31mgotcha\nline)"},
				{Level: verdict.Clear, Text: "Ownership is renounced."},
			},
			NotChecked: []string{
				"holders: too much history to replay (budget exhausted)",
				"holders: explorer refused",
				"LP holders: same",
			},
		},
	}
	got := Render(r)
	for _, want := range []string{
		"🔴 Honeypot: you can buy, but you cannot sell\nZiG on Base\n0xEF77fB35d6e0503faf3cbe9565193195Aa4c6eF8\n",
		"• the sale failed (",
		"✓ Ownership is renounced.",
		"Not checked: holders; LP holders.",
		"checked in 9.7 s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render lacks %q:\n%s", want, got)
		}
	}
	if strings.ContainsAny(got, "\x1b") || strings.Contains(got, "\nline)") {
		t.Errorf("control characters reached the output:\n%q", got)
	}
}

func TestDefangStopsAuthorTextBeingALink(t *testing.T) {
	for in, want := range map[string]string{
		"Claim at evil.xyz":          "Claim at evil[.]xyz",
		"go to https://a.evil.ru/x":  "go to https[:]//a[.]evil[.]ru/x",
		"join t.me/evilgroup":        "join t[.]me/evilgroup",
		"ask @evilbot now":           "ask (at)evilbot now",
		"@evilbot":                   "(at)evilbot",
		"$106.2M in pools, 9.1 s":    "$106.2M in pools, 9.1 s",
		"see Token.sol:12-30":        "see Token.sol:12-30",
		"Block 26301188 · checked.":  "Block 26301188 · checked.",
		"UniswapV2Library: REVERTED": "UniswapV2Library: REVERTED",
	} {
		if got := Defang(in); got != want {
			t.Errorf("Defang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderDefangsNamesAndReasons(t *testing.T) {
	r := &Report{Chain: "base", Name: "Claim at evil.xyz", Symbol: "t.me/evil", Verdict: verdict.Verdict{
		Level: verdict.Scam, Headline: "Honeypot",
		Reasons: []verdict.Reason{{Level: verdict.Scam, Text: "The sale failed (visit https://evil.ru)."}},
	}}
	got := Render(r)
	for _, bad := range []string{"evil.xyz", "t.me/", "https://", "evil.ru"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q is still live:\n%s", bad, got)
		}
	}
}
