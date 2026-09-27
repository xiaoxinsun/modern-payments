package projections

import (
	"context"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"gtb.local/backend/internal/eventing"
	"gtb.local/backend/internal/projections/storage/projectiondb"
)

// NewKafkaConsumer joins the projector's consumer group. The projections
// database, not Kafka, is the checkpoint authority: on assignment each partition
// resumes at the offset committed with its projection changes, or at the start
// of the log when none exists (the inbox absorbs the redelivery). Offsets are
// also committed to Kafka afterwards, for tooling only.
func NewKafkaConsumer(brokers []string, group, topic string, store *Store, consumer string) (*kgo.Client, error) {
	return kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ClientID("event-projector"),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topic),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxWait(500*time.Millisecond),
		// A crashed projector never leaves the group; its partitions are
		// reassigned once this session times out (the Kafka default is 45s).
		kgo.SessionTimeout(10*time.Second),
		kgo.AdjustFetchOffsetsFn(func(ctx context.Context, assigned map[string]map[int32]kgo.Offset) (map[string]map[int32]kgo.Offset, error) {
			return store.checkpointOffsets(ctx, consumer, assigned)
		}),
	)
}

func (store *Store) checkpointOffsets(ctx context.Context, consumer string, assigned map[string]map[int32]kgo.Offset) (map[string]map[int32]kgo.Offset, error) {
	for topic, partitions := range assigned {
		rows, err := projectiondb.New(store.pool).ListOffsets(ctx, projectiondb.ListOffsetsParams{Consumer: consumer, Topic: topic})
		if err != nil {
			return nil, err
		}
		stored := make(map[int32]int64, len(rows))
		for _, row := range rows {
			stored[row.Partition] = row.NextOffset
		}
		for partition := range partitions {
			if next, ok := stored[partition]; ok {
				partitions[partition] = kgo.NewOffset().At(next).WithEpoch(-1)
			} else {
				partitions[partition] = kgo.NewOffset().AtStart()
			}
		}
	}
	return assigned, nil
}

// Heartbeat records that the consumer reached its broker, for freshness.
func (store *Store) Heartbeat(ctx context.Context, consumer string) error {
	return projectiondb.New(store.pool).TouchConsumer(ctx, projectiondb.TouchConsumerParams{
		Consumer: consumer, BrokerSeenAt: timestamp(store.now().UTC()),
	})
}

// LiveConsumer projects Kafka records. A batch is acknowledged only after its
// projection transaction commits; a database failure retries the same batch.
type LiveConsumer struct {
	Name    string
	Topic   string
	Store   *Store
	Client  *kgo.Client
	Logger  *slog.Logger
	Effects []eventing.Consumer
}

var projectorMeter = otel.Meter("gtb.local/backend/internal/projections")

func (live *LiveConsumer) Run(ctx context.Context) {
	projected, _ := projectorMeter.Int64Counter("gtb.projector.projected", metric.WithDescription("Events newly projected"))
	duplicates, _ := projectorMeter.Int64Counter("gtb.projector.duplicates", metric.WithDescription("Redelivered events absorbed by the inbox"))
	buffered, _ := projectorMeter.Int64Counter("gtb.projector.buffered", metric.WithDescription("Events buffered behind an ordering gap"))
	poisoned, _ := projectorMeter.Int64Counter("gtb.projector.poisoned", metric.WithDescription("Events sent to the repair queue"))
	go live.heartbeat(ctx)
	// With BlockRebalanceOnPoll, a poll that is never followed by AllowRebalance
	// blocks leaving the group, so Close would hang on shutdown.
	defer live.Client.AllowRebalance()
	live.Logger.Info("projection consumer started", "consumer", live.Name, "topic", live.Topic)
	var lastFetchWarning time.Time
	for {
		fetches := live.Client.PollRecords(ctx, 500)
		if ctx.Err() != nil {
			live.Logger.Info("projection consumer stopped", "consumer", live.Name)
			return
		}
		for _, fetchErr := range fetches.Errors() {
			if time.Since(lastFetchWarning) > 10*time.Second {
				live.Logger.Warn("Kafka fetch failed", "topic", fetchErr.Topic, "partition", fetchErr.Partition, "error", fetchErr.Err)
				lastFetchWarning = time.Now()
			}
		}
		var items []Incoming
		var records []*kgo.Record
		fetches.EachPartition(func(partition kgo.FetchTopicPartition) {
			for _, record := range partition.Records {
				records = append(records, record)
				items = append(items, Decode(record.Value, eventing.DeliveryLive, &KafkaPosition{
					Topic: record.Topic, Partition: record.Partition, Offset: record.Offset, HighWatermark: partition.HighWatermark,
				}))
			}
		})
		if len(items) == 0 {
			live.Client.AllowRebalance()
			continue
		}
		result, ok := live.process(ctx, items)
		if !ok {
			return
		}
		projected.Add(ctx, int64(result.Projected))
		duplicates.Add(ctx, int64(result.Duplicates))
		buffered.Add(ctx, int64(result.Buffered))
		poisoned.Add(ctx, int64(result.Poisoned))
		if result.Buffered > 0 || result.Poisoned > 0 {
			live.Logger.Warn("projection batch had gaps or poison events", "consumer", live.Name,
				"projected", result.Projected, "buffered", result.Buffered, "poisoned", result.Poisoned)
		}
		live.Logger.Info("projection batch committed", "consumer", live.Name, "records", len(items),
			"projected", result.Projected, "duplicates", result.Duplicates)
		runEffects(ctx, live.Logger, live.Effects, result.Applied)
		if err := live.Client.CommitRecords(ctx, records...); err != nil && ctx.Err() == nil {
			live.Logger.Warn("Kafka offset commit failed; the database checkpoint is authoritative", "error", err)
		}
		live.Client.AllowRebalance()
	}
}

func (live *LiveConsumer) process(ctx context.Context, items []Incoming) (Result, bool) {
	backoff := 250 * time.Millisecond
	for {
		result, err := live.Store.Process(ctx, live.Name, items, nil)
		if err == nil {
			return result, true
		}
		if ctx.Err() != nil {
			return Result{}, false
		}
		live.Logger.Warn("projection batch failed; retrying", "consumer", live.Name, "error", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return Result{}, false
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (live *LiveConsumer) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		pingContext, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := live.Client.Ping(pingContext); err == nil {
			if err := live.Store.Heartbeat(ctx, live.Name); err != nil && ctx.Err() == nil {
				live.Logger.Warn("consumer heartbeat not recorded", "error", err)
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runEffects hands newly projected events to downstream consumers, withholding
// replayed events from effectful ones.
func runEffects(ctx context.Context, logger *slog.Logger, effects []eventing.Consumer, applied []Applied) {
	for _, event := range applied {
		for _, effect := range effects {
			if !effect.Accepts(event.Delivery) {
				logger.Info("replayed event withheld from effectful consumer", "consumer", effect.Name, "event_id", event.Event.EventID)
				continue
			}
			if err := effect.Handle(ctx, event.Event); err != nil {
				logger.Warn("event consumer failed", "consumer", effect.Name, "event_id", event.Event.EventID, "error", err)
			}
		}
	}
}
