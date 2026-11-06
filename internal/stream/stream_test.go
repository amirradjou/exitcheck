package stream

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/amirradjou/exitcheck/internal/launch"
)

func TestBrokers(t *testing.T) {
	got := Brokers(" a:1, ,b:2 ")
	if len(got) != 2 || got[0] != "a:1" || got[1] != "b:2" {
		t.Errorf("Brokers = %q", got)
	}
}

// Integration: needs Kafka (docker compose up -d kafka); run with EXITCHECK_INTEGRATION=1. KAFKA_BROKERS overrides
// the compose default. Each run uses its own topic and group.
func TestPublishConsumeCommit(t *testing.T) {
	if os.Getenv("EXITCHECK_INTEGRATION") == "" {
		t.Skip("set EXITCHECK_INTEGRATION=1 (with Kafka from compose.yaml)")
	}
	brokers := Brokers(os.Getenv("KAFKA_BROKERS"))
	if len(brokers) == 0 {
		brokers = []string{"localhost:9094"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	topic, group := fmt.Sprintf("test.launches.%d", time.Now().UnixNano()), fmt.Sprintf("test-group-%d", time.Now().UnixNano())
	if err := EnsureTopic(ctx, brokers, topic, 3); err != nil {
		t.Fatal(err)
	}
	if err := EnsureTopic(ctx, brokers, topic, 3); err != nil {
		t.Fatalf("creating an existing topic: %v", err)
	}
	p, err := NewProducer(brokers, topic)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var sent []launch.Launch
	for i := range 6 {
		sent = append(sent, launch.Launch{Pool: launch.Pool{Chain: "base", DEX: "uniswap-v2",
			Pool: common.BytesToAddress([]byte{0xa0, byte(i)}), Token: common.BytesToAddress([]byte{0x70, byte(i % 2)})}, FundedBlock: uint64(i)})
	}
	if err := p.Publish(ctx, sent); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	consume := func(want int) map[string]int {
		c, err := NewConsumer(brokers, topic, group, log)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		var mu sync.Mutex
		got := map[string]int{}
		cctx, stop := context.WithTimeout(ctx, 15*time.Second)
		defer stop()
		go func() {
			_ = c.Run(cctx, func(_ context.Context, l launch.Launch) error {
				mu.Lock()
				defer mu.Unlock()
				got[l.ID()]++
				return nil
			})
		}()
		for {
			mu.Lock()
			n := len(got)
			mu.Unlock()
			if n >= want || cctx.Err() != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(2 * time.Second) // let the commit land before leaving the group
		stop()
		mu.Lock()
		defer mu.Unlock()
		return got
	}

	got := consume(len(sent))
	for _, l := range sent {
		if got[l.ID()] != 1 {
			t.Errorf("%s delivered %d times", l.ID(), got[l.ID()])
		}
	}
	// The same group again: everything was committed, so nothing is replayed.
	if again := consume(1); len(again) != 0 {
		t.Errorf("committed records delivered again: %v", again)
	}
}
