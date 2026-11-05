// Package stream carries launches from the watchers to the analyzers over Kafka. The producer is idempotent, so a
// retried send does not duplicate a record. The analyzers form one consumer group: each partition belongs to one of
// them at a time, and a crashed analyzer's partitions move to the others. Offsets are committed only after a batch is
// handled, so a crash replays work instead of losing it; the store turns a replay into a delivery count.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/amirradjou/exitcheck/internal/launch"
)

// Topic and Group name the launches topic and the analyzers' consumer group.
const (
	Topic = "exitcheck.launches"
	Group = "exitcheck-analyzers"
)

// Brokers splits a comma-separated broker list.
func Brokers(list string) []string {
	var out []string
	for b := range strings.SplitSeq(list, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// EnsureTopic creates the topic if it does not exist. Partitions bound how many analyzers can share the work.
func EnsureTopic(ctx context.Context, brokers []string, topic string, partitions int32) error {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return err
	}
	defer cl.Close()
	resp, err := kadm.NewClient(cl).CreateTopic(ctx, partitions, 1, nil, topic)
	if err == nil {
		err = resp.Err
	}
	if err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
		return fmt.Errorf("creating topic %s: %w", topic, err)
	}
	return nil
}

// Producer publishes launches.
type Producer struct {
	cl *kgo.Client
}

// NewProducer connects a producer. Records wait for every in-sync replica, and idempotence (franz-go's default)
// keeps retries from writing a record twice.
func NewProducer(brokers []string, topic string) (*Producer, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.DefaultProduceTopic(topic),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerLinger(0), // a launch is latency-bound, not throughput-bound
	)
	if err != nil {
		return nil, err
	}
	return &Producer{cl: cl}, nil
}

// Publish sends launches keyed by token, and returns once Kafka has them.
func (p *Producer) Publish(ctx context.Context, ls []launch.Launch) error {
	recs := make([]*kgo.Record, len(ls))
	for i, l := range ls {
		v, err := json.Marshal(l)
		if err != nil {
			return err
		}
		recs[i] = &kgo.Record{Key: l.Key(), Value: v}
	}
	return p.cl.ProduceSync(ctx, recs...).FirstErr()
}

// Close flushes and disconnects.
func (p *Producer) Close() { p.cl.Close() }

// Consumer reads launches as a member of the analyzers' group.
type Consumer struct {
	cl  *kgo.Client
	log *slog.Logger
}

// NewConsumer joins the group. Rebalances wait until the current batch is handled and committed, so a partition is
// never processed by two members at once.
func NewConsumer(brokers []string, topic, group string, log *slog.Logger) (*Consumer, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topic),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.OnPartitionsAssigned(func(_ context.Context, _ *kgo.Client, m map[string][]int32) {
			log.Info("partitions assigned", "partitions", m[topic])
		}),
		kgo.OnPartitionsRevoked(func(_ context.Context, _ *kgo.Client, m map[string][]int32) {
			log.Info("partitions revoked", "partitions", m[topic])
		}),
	)
	if err != nil {
		return nil, err
	}
	return &Consumer{cl: cl, log: log}, nil
}

// Close leaves the group. A batch blocked for rebalancing would block the leave forever, so it is released first.
func (c *Consumer) Close() {
	c.cl.AllowRebalance()
	c.cl.Close()
}

// Run hands each launch to handle until ctx ends. Partitions of a batch are handled concurrently, the records of one
// partition in order (one token's launches stay serial). handle's error means "try again": it is retried with backoff
// until it succeeds or ctx ends, and nothing is committed meanwhile. A record that does not decode is logged and
// skipped.
func (c *Consumer) Run(ctx context.Context, handle func(context.Context, launch.Launch) error) error {
	// Whatever way Run ends, a rebalance must not stay blocked on a batch nobody will finish.
	defer c.cl.AllowRebalance()
	for {
		fetches := c.cl.PollRecords(ctx, 64)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return ctx.Err()
		}
		fetches.EachError(func(topic string, p int32, err error) {
			c.log.Warn("fetch", "topic", topic, "partition", p, "err", err)
		})
		var wg sync.WaitGroup
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			wg.Go(func() {
				for _, r := range p.Records {
					c.handle(ctx, r, handle)
				}
			})
		})
		wg.Wait()
		if ctx.Err() != nil {
			return ctx.Err() // uncommitted: the next owner of these partitions replays them
		}
		if err := c.cl.CommitRecords(ctx, fetches.Records()...); err != nil {
			c.log.Warn("commit", "err", err)
		}
		c.cl.AllowRebalance()
	}
}

func (c *Consumer) handle(ctx context.Context, r *kgo.Record, handle func(context.Context, launch.Launch) error) {
	var l launch.Launch
	if err := json.Unmarshal(r.Value, &l); err != nil {
		c.log.Warn("skipping an undecodable record", "partition", r.Partition, "offset", r.Offset, "err", err)
		return
	}
	backoff := time.Second
	for ctx.Err() == nil {
		err := handle(ctx, l)
		if err == nil {
			return
		}
		c.log.Warn("handling a launch; retrying", "id", l.ID(), "err", err, "retry_in", backoff)
		t := time.NewTimer(backoff)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
		backoff = min(2*backoff, time.Minute)
	}
}
