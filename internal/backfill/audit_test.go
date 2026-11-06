package backfill

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/exitcheck/internal/launch"
)

func TestMissedIsWhatTheWatchDidNotRecord(t *testing.T) {
	a, b, c := common.HexToAddress("0xa1"), common.HexToAddress("0xb2"), common.HexToAddress("0xc3")
	funded := []Funded{{Pool: launch.Pool{Pool: a}}, {Pool: launch.Pool{Pool: b}}, {Pool: launch.Pool{Pool: c}}}
	got := Missed(funded, map[common.Address]bool{a: true, c: true})
	if len(got) != 1 || got[0].Pool.Pool != b {
		t.Errorf("Missed = %+v, want only %s", got, b)
	}
}
