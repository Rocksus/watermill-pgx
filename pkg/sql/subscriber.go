package sql

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"sync"
	"time"

	"github.com/oklog/ulid"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
)

var (
	ErrSubscriberClosed = errors.New("subscriber is closed")
)

type SubscriberConfig struct {
	ConsumerGroup string

	// AckDeadline is the time to wait for acking a message.
	// If message is not acked within this time, it will be nacked and re-delivered.
	//
	// When messages are read in bulk, this time is calculated for each message separately.
	//
	// If you want to disable ack deadline, set it to 0.
	// Warning: when ack deadline is disabled, messages which are not acked may block PostgreSQL subscriber from reading new messages
	// due to not increasing `pg_snapshot_xmin(pg_current_snapshot())` value.
	//
	// Must be non-negative. Nil value defaults to 30s.
	AckDeadline *time.Duration

	// PollInterval is the interval to wait between subsequent SELECT queries, if no more messages were found in the database (Prefer using the BackoffManager instead).
	// Must be non-negative. Defaults to 1s.
	PollInterval time.Duration

	// ResendInterval is the time to wait before resending a nacked message.
	// Must be non-negative. Defaults to 1s.
	ResendInterval time.Duration

	// RetryInterval is the time to wait before resuming querying for messages after an error (Prefer using the BackoffManager instead).
	// Must be non-negative. Defaults to 1s.
	RetryInterval time.Duration

	// BackoffManager defines how much to backoff when receiving errors.
	BackoffManager BackoffManager

	// SchemaAdapter provides the schema-dependent queries and arguments for them, based on topic/message etc.
	SchemaAdapter SchemaAdapter

	// OffsetsAdapter provides mechanism for saving acks and offsets of consumers.
	OffsetsAdapter OffsetsAdapter

	// InitializeSchema option enables initializing schema on making subscription.
	InitializeSchema bool
}

func (c *SubscriberConfig) setDefaults() {
	if c.AckDeadline == nil {
		timeout := time.Second * 30
		c.AckDeadline = &timeout
	}
	if c.PollInterval == 0 {
		c.PollInterval = time.Second
	}
	if c.ResendInterval == 0 {
		c.ResendInterval = time.Second
	}
	if c.RetryInterval == 0 {
		c.RetryInterval = time.Second
	}
	if c.BackoffManager == nil {
		c.BackoffManager = NewDefaultBackoffManager(c.PollInterval, c.RetryInterval)
	}
}

func (c SubscriberConfig) validate() error {
	if c.AckDeadline == nil {
		return errors.New("ack deadline is nil")
	}
	if c.AckDeadline != nil && *c.AckDeadline <= 0 {
		return errors.New("ack deadline must be a positive duration")
	}
	if c.PollInterval <= 0 {
		return errors.New("poll interval must be a positive duration")
	}
	if c.ResendInterval <= 0 {
		return errors.New("resend interval must be a positive duration")
	}
	if c.RetryInterval <= 0 {
		return errors.New("resend interval must be a positive duration")
	}
	if c.SchemaAdapter == nil {
		return errors.New("schema adapter is nil")
	}
	if c.OffsetsAdapter == nil {
		return errors.New("offsets adapter is nil")
	}

	return nil
}

// Subscriber makes SELECT queries on the chosen table with the interval defined in the config.
// The rows are unmarshaled into Watermill messages.
type Subscriber struct {
	consumerIdBytes  []byte
	consumerIdString string

	db     Beginner
	config SubscriberConfig

	subscribeWg *sync.WaitGroup
	closing     chan struct{}
	closed      bool

	logger watermill.LoggerAdapter
}

func NewSubscriber(db Beginner, config SubscriberConfig, logger watermill.LoggerAdapter) (*Subscriber, error) {
	if db == nil {
		return nil, errors.New("db is nil")
	}
	config.setDefaults()
	err := config.validate()
	if err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	if logger == nil {
		logger = watermill.NopLogger{}
	}

	idBytes, idStr, err := newSubscriberID()
	if err != nil {
		return &Subscriber{}, fmt.Errorf("cannot generate subscriber id: %w", err)
	}
	logger = logger.With(watermill.LogFields{"subscriber_id": idStr})

	sub := &Subscriber{
		consumerIdBytes:  idBytes,
		consumerIdString: idStr,

		db:     db,
		config: config,

		subscribeWg: &sync.WaitGroup{},
		closing:     make(chan struct{}),

		logger: logger,
	}

	return sub, nil
}

func newSubscriberID() ([]byte, string, error) {
	id := watermill.NewULID()
	idBytes, err := ulid.MustParseStrict(id).MarshalBinary()
	if err != nil {
		return nil, "", fmt.Errorf("cannot marshal subscriber id: %w", err)
	}

	return idBytes, id, nil
}

func (s *Subscriber) Subscribe(ctx context.Context, topic string) (o <-chan *message.Message, err error) {
	if s.closed {
		return nil, ErrSubscriberClosed
	}

	if err = validateTopicName(topic); err != nil {
		return nil, err
	}

	if s.config.InitializeSchema {
		if err := s.SubscribeInitialize(topic); err != nil {
			return nil, err
		}
	}

	bsq := s.config.OffsetsAdapter.BeforeSubscribingQueries(topic, s.config.ConsumerGroup)

	if len(bsq) >= 1 {
		err := runInTx(ctx, s.db, func(ctx context.Context, tx pgx.Tx) error {
			for _, q := range bsq {
				s.logger.Debug("Executing before subscribing query", watermill.LogFields{
					"query": q,
				})

				_, err := tx.Exec(ctx, q.Query, q.Args...)
				if err != nil {
					return fmt.Errorf("cannot execute before subscribing query: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// the information about closing the subscriber is propagated through ctx
	ctx, cancel := context.WithCancel(ctx)
	out := make(chan *message.Message)

	s.subscribeWg.Add(1)
	go func() {
		s.consume(ctx, topic, out)
		close(out)
		cancel()
	}()

	return out, nil
}

func (s *Subscriber) consume(ctx context.Context, topic string, out chan *message.Message) {
	defer s.subscribeWg.Done()

	logger := s.logger.With(watermill.LogFields{
		"topic":          topic,
		"consumer_group": s.config.ConsumerGroup,
	})

	var sleepTime time.Duration = 0
	for {
		select {
		case <-s.closing:
			logger.Info("Discarding queued message, subscriber closing", nil)
			return

		case <-ctx.Done():
			logger.Info("Stopping consume, context canceled", nil)
			return

		case <-time.After(sleepTime): // Wait if needed
		}

		noMsg, err := s.query(ctx, topic, out, logger)
		backoff := s.config.BackoffManager.HandleError(logger, noMsg, err)
		if backoff != 0 {
			if err != nil {
				logger = logger.With(watermill.LogFields{"err": err.Error()})
			}
			logger.Trace("Backing off querying", watermill.LogFields{
				"wait_time": backoff,
				"no_msg":    noMsg,
			})
		}
		sleepTime = backoff
	}
}

func (s *Subscriber) query(
	ctx context.Context,
	topic string,
	out chan *message.Message,
	logger watermill.LoggerAdapter,
) (noMsg bool, err error) {
	txOptions := pgx.TxOptions{
		IsoLevel: s.config.SchemaAdapter.SubscribeIsolationLevel(),
	}
	tx, err := s.db.BeginTx(ctx, txOptions)
	if err != nil {
		return false, fmt.Errorf("could not begin tx for querying: %w", err)
	}

	defer func() {
		if err != nil {
			rollbackErr := tx.Rollback(ctx)
			if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				logger.Error("could not rollback tx for querying message", rollbackErr, watermill.LogFields{
					"query_err": err,
				})
			}
		} else {
			commitErr := tx.Commit(ctx)
			if commitErr != nil && !errors.Is(commitErr, pgx.ErrTxClosed) {
				logger.Error("could not commit tx for querying message", commitErr, nil)
			}
		}
	}()

	selectQuery := s.config.SchemaAdapter.SelectQuery(
		topic,
		s.config.ConsumerGroup,
		s.config.OffsetsAdapter,
	)
	logger.Trace("Querying message", watermill.LogFields{
		"query":      selectQuery.Query,
		"query_args": sqlArgsToLog(selectQuery.Args),
	})
	rows, err := tx.Query(ctx, selectQuery.Query, selectQuery.Args...)
	if err != nil {
		return false, fmt.Errorf("could not query message: %w", err)
	}

	defer rows.Close()

	var lastOffset int64
	var lastRow Row

	messageRows := make([]Row, 0)

	for rows.Next() {
		row, err := s.config.SchemaAdapter.UnmarshalMessage(rows)
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		} else if err != nil {
			return false, fmt.Errorf("could not unmarshal message from query: %w", err)
		}

		messageRows = append(messageRows, row)
	}

	for _, row := range messageRows {
		acked, err := s.processMessage(ctx, topic, row, tx, out, logger)
		if err != nil {
			return false, fmt.Errorf("could not process message: %w", err)
		}
		if !acked {
			break
		}

		lastOffset = row.Offset
		lastRow = row
	}

	if lastOffset == 0 {
		return true, nil
	}

	ackQuery := s.config.OffsetsAdapter.AckMessageQuery(
		topic,
		lastRow,
		s.config.ConsumerGroup,
	)

	logger.Trace("Executing ack message query", watermill.LogFields{
		"query":      ackQuery.Query,
		"query_args": sqlArgsToLog(ackQuery.Args),
	})

	result, err := tx.Exec(ctx, ackQuery.Query, ackQuery.Args...)
	if err != nil {
		return false, fmt.Errorf("could not get args for acking the message: %w", err)
	}

	rowsAffected := result.RowsAffected()

	logger.Trace("Executed ack message query", watermill.LogFields{
		"rows_affected": rowsAffected,
	})

	return false, nil
}

func (s *Subscriber) processMessage(
	ctx context.Context,
	topic string,
	row Row,
	tx pgx.Tx,
	out chan *message.Message,
	logger watermill.LoggerAdapter,
) (bool, error) {
	if *s.config.AckDeadline != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *s.config.AckDeadline)
		defer cancel()
	}

	consumedQuery := s.config.OffsetsAdapter.ConsumedMessageQuery(
		topic,
		row,
		s.config.ConsumerGroup,
		s.consumerIdBytes,
	)
	if !consumedQuery.IsZero() {
		logger.Trace("Executing query to confirm message consumed", watermill.LogFields{
			"query":      consumedQuery.Args,
			"query_args": sqlArgsToLog(consumedQuery.Args),
		})

		_, err := tx.Exec(ctx, consumedQuery.Query, consumedQuery.Args...)
		if err != nil {
			return false, fmt.Errorf("cannot send consumed query: %w", err)
		}

		logger.Trace("Executed query to confirm message consumed", nil)
	}

	logger = logger.With(watermill.LogFields{
		"msg_uuid": row.Msg.UUID,
	})
	logger.Trace("Received message", nil)

	msgCtx := setTxToContext(ctx, tx)

	return s.sendMessage(msgCtx, row.Msg, out, logger), nil
}

// sendMessages sends messages on the output channel.
func (s *Subscriber) sendMessage(
	ctx context.Context,
	msg *message.Message,
	out chan *message.Message,
	logger watermill.LoggerAdapter,
) (acked bool) {
	msgCtx, cancel := context.WithCancel(ctx)
	msg.SetContext(msgCtx)
	defer cancel()

ResendLoop:
	for {

		select {
		case out <- msg:

		case <-s.closing:
			logger.Info("Discarding queued message, subscriber closing", nil)
			return false

		case <-ctx.Done():
			logger.Info("Discarding queued message, context canceled", nil)
			return false
		}

		select {
		case <-msg.Acked():
			logger.Debug("Message acked by subscriber", nil)
			return true

		case <-msg.Nacked():
			//message nacked, try resending
			logger.Debug("Message nacked, resending", nil)
			msg = msg.Copy()
			msg.SetContext(msgCtx)

			if s.config.ResendInterval != 0 {
				time.Sleep(s.config.ResendInterval)
			}

			continue ResendLoop

		case <-s.closing:
			logger.Info("Discarding queued message, subscriber closing", nil)
			return false

		case <-ctx.Done():
			logger.Info("Discarding queued message, context canceled", nil)
			return false
		}
	}
}

func (s *Subscriber) Close() error {
	if s.closed {
		return nil
	}

	s.closed = true

	close(s.closing)
	s.subscribeWg.Wait()

	return nil
}

func (s *Subscriber) SubscribeInitialize(topic string) error {
	return initializeSchema(
		context.Background(),
		topic,
		s.logger,
		s.db,
		s.config.SchemaAdapter,
		s.config.OffsetsAdapter,
	)
}
