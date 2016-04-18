package consumer

import (
	"fmt"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/mailgun/kafka-pixy/actor"
	"github.com/mailgun/kafka-pixy/mapper"
	"github.com/mailgun/kafka-pixy/none"
	"github.com/mailgun/log"
)

// ConsumerMessage encapsulates a Kafka message returned by the consumer.
type ConsumerMessage struct {
	Key, Value    []byte
	Topic         string
	Partition     int32
	Offset        int64
	HighWaterMark int64
}

// ConsumerError is what is provided to the user when an error occurs.
// It wraps an error and includes the topic and partition.
type ConsumerError struct {
	Topic     string
	Partition int32
	Err       error
}

func (ce ConsumerError) Error() string {
	return fmt.Sprintf("kafka: error while consuming %s/%d: %s", ce.Topic, ce.Partition, ce.Err)
}

// ConsumerErrors is a type that wraps a batch of errors and implements the Error interface.
// It can be returned from the PartitionConsumer's Close methods to avoid the need to manually drain errors
// when stopping.
type ConsumerErrors []*ConsumerError

func (ce ConsumerErrors) Error() string {
	return fmt.Sprintf("kafka: %d errors while consuming", len(ce))
}

// Consumer manages PartitionConsumers which process Kafka messages from brokers. You MUST call Close()
// on a consumer to avoid leaks, it will not be garbage-collected automatically when it passes out of
// scope.
type Consumer interface {
	// ConsumePartition creates a PartitionConsumer on the given topic/partition
	// with the given offset. It will return an error if this Consumer is
	// already consuming on the given topic/partition. Offset can be a
	// literal offset, or OffsetNewest or OffsetOldest.
	//
	// If offset is smaller then the oldest offset then the oldest offset is
	// returned. If offset is larger then the newest offset then the newest
	// offset is returned. If offset is either sarama.OffsetNewest or
	// sarama.OffsetOldest constant, then the actual offset value is returned.
	// otherwise offset is returned.
	ConsumePartition(topic string, partition int32, offset int64) (PartitionConsumer, int64, error)

	// Close shuts down the consumer. It must be called after all child PartitionConsumers have already been closed.
	Close() error
}

type consumer struct {
	baseCID      *actor.ID
	config       *sarama.Config
	client       sarama.Client
	ownClient    bool
	children     map[topicPartition]*partitionConsumer
	childrenLock sync.Mutex
	mapper       *mapper.T
}

type topicPartition struct {
	topic     string
	partition int32
}

// NewConsumer creates a new consumer using the given broker addresses and configuration.
func NewConsumer(addrs []string, config *sarama.Config) (Consumer, error) {
	client, err := sarama.NewClient(addrs, config)
	if err != nil {
		return nil, err
	}

	c, err := NewConsumerFromClient(client)
	if err != nil {
		return nil, err
	}
	c.(*consumer).ownClient = true
	return c, nil
}

// NewConsumerFromClient creates a new consumer using the given client. It is still
// necessary to call Close() on the underlying client when shutting down this consumer.
func NewConsumerFromClient(client sarama.Client) (Consumer, error) {
	// Check that we are not dealing with a closed Client before processing any other arguments
	if client.Closed() {
		return nil, sarama.ErrClosedClient
	}
	c := &consumer{
		baseCID:  actor.RootID.NewChild("consumer"),
		client:   client,
		config:   client.Config(),
		children: make(map[topicPartition]*partitionConsumer),
	}
	c.mapper = mapper.Spawn(c.baseCID, c)
	return c, nil
}

func (c *consumer) Close() error {
	c.childrenLock.Lock()
	for _, pc := range c.children {
		close(pc.closingCh)
		<-pc.closedCh
		c.mapper.WorkerStopped() <- pc
	}
	c.childrenLock.Unlock()
	c.mapper.Stop()
	if c.ownClient {
		return c.client.Close()
	}
	return nil
}

func (c *consumer) ConsumePartition(topic string, partition int32, offset int64) (PartitionConsumer, int64, error) {
	concreteOffset, err := c.chooseStartingOffset(topic, partition, offset)
	if err != nil {
		return nil, sarama.OffsetNewest, err
	}

	c.childrenLock.Lock()
	defer c.childrenLock.Unlock()

	tp := topicPartition{topic, partition}
	if _, ok := c.children[tp]; ok {
		return nil, sarama.OffsetNewest, sarama.ConfigurationError("That topic/partition is already being consumed")
	}
	pc := c.spawnPartitionConsumer(tp, concreteOffset)
	c.mapper.WorkerSpawned() <- pc
	c.children[tp] = pc
	return pc, concreteOffset, nil
}

// implements `mapper.Resolver.ResolveBroker()`.
func (c *consumer) ResolveBroker(pw mapper.Worker) (*sarama.Broker, error) {
	pc := pw.(*partitionConsumer)
	if err := c.client.RefreshMetadata(pc.tp.topic); err != nil {
		return nil, err
	}
	return c.client.Leader(pc.tp.topic, pc.tp.partition)
}

// implements `mapper.Resolver.Executor()`
func (c *consumer) SpawnExecutor(brokerConn *sarama.Broker) mapper.Executor {
	bc := &brokerConsumer{
		baseCID:         c.baseCID.NewChild(fmt.Sprintf("broker:%d", brokerConn.ID())),
		config:          c.config,
		conn:            brokerConn,
		requestsCh:      make(chan fetchRequest),
		batchRequestsCh: make(chan []fetchRequest),
	}
	spawn(&bc.wg, bc.batchRequests)
	spawn(&bc.wg, bc.executeBatches)
	return bc
}

// chooseStartingOffset takes an offset value that may be either an actual
// offset of two constants (`OffsetNewest` and `OffsetOldest`) and return an
// offset value. It checks if the offset value belongs to the current range.
//
// FIXME: The offset values corresponding to `OffsetNewest` and `OffsetOldest`
// may change during the function execution (e.g. an old log chunk gets
// deleted), so the offset value returned by the function may be incorrect.
func (c *consumer) chooseStartingOffset(topic string, partition int32, offset int64) (int64, error) {
	newestOffset, err := c.client.GetOffset(topic, partition, sarama.OffsetNewest)
	if err != nil {
		return 0, err
	}
	oldestOffset, err := c.client.GetOffset(topic, partition, sarama.OffsetOldest)
	if err != nil {
		return 0, err
	}

	switch {
	case offset == sarama.OffsetNewest || offset > newestOffset:
		return newestOffset, nil
	case offset == sarama.OffsetOldest || offset < oldestOffset:
		return oldestOffset, nil
	default:
		return offset, nil
	}
}

// PartitionConsumer processes Kafka messages from a given topic and partition. You MUST call Close()
// or AsyncClose() on a PartitionConsumer to avoid leaks, it will not be garbage-collected automatically
// when it passes out of scope.
//
// The simplest way of using a PartitionConsumer is to loop over its Messages channel using a for/range
// loop. The PartitionConsumer will only stop itself in one case: when the offset being consumed is reported
// as out of range by the brokers. In this case you should decide what you want to do (try a different offset,
// notify a human, etc) and handle it appropriately. For all other error cases, it will just keep retrying.
// By default, it logs these errors to sarama.Logger; if you want to be notified directly of all errors, set
// your config's Consumer.Return.Errors to true and read from the Errors channel, using a select statement
// or a separate goroutine. Check out the Consumer examples to see implementations of these different approaches.
type PartitionConsumer interface {
	// Close stops the PartitionConsumer from fetching messages. It is required to call this function
	// (or AsyncClose) before a consumer object passes out of scope, as it will otherwise leak memory. You must
	// call this before calling Close on the underlying client.
	Close() error

	// Messages returns the read channel for the messages that are returned by the broker.
	Messages() <-chan *ConsumerMessage

	// Errors returns a read channel of errors that occured during consuming, if enabled. By default,
	// errors are logged and not returned over this channel. If you want to implement any custom error
	// handling, set your config's Consumer.Return.Errors setting to true, and read from this channel.
	Errors() <-chan *ConsumerError
}

// implements `mapper.Worker`.
type partitionConsumer struct {
	consumer *consumer
	tp       topicPartition
	baseCID  *actor.ID

	assignmentCh chan mapper.Executor
	initErrorCh  chan error
	messagesCh   chan *ConsumerMessage
	errorsCh     chan *ConsumerError
	closingCh    chan none.T
	closedCh     chan none.T

	fetchSize int32
	offset    int64
	lag       int64
}

func (c *consumer) spawnPartitionConsumer(tp topicPartition, offset int64) *partitionConsumer {
	cp := &partitionConsumer{
		consumer:     c,
		tp:           tp,
		baseCID:      c.baseCID.NewChild(fmt.Sprintf("%s:%d", tp.topic, tp.partition)),
		assignmentCh: make(chan mapper.Executor, 1),
		initErrorCh:  make(chan error),
		messagesCh:   make(chan *ConsumerMessage, c.config.ChannelBufferSize),
		errorsCh:     make(chan *ConsumerError, c.config.ChannelBufferSize),
		closingCh:    make(chan none.T, 1),
		closedCh:     make(chan none.T),
		offset:       offset,
		fetchSize:    c.config.Consumer.Fetch.Default,
	}
	go cp.pullMessages()
	return cp
}

func (pc *partitionConsumer) Messages() <-chan *ConsumerMessage {
	return pc.messagesCh
}

func (pc *partitionConsumer) Errors() <-chan *ConsumerError {
	return pc.errorsCh
}

func (pc *partitionConsumer) Close() error {
	close(pc.closingCh)
	<-pc.closedCh

	var errors ConsumerErrors
	for err := range pc.errorsCh {
		errors = append(errors, err)
	}

	pc.consumer.childrenLock.Lock()
	delete(pc.consumer.children, pc.tp)
	pc.consumer.childrenLock.Unlock()

	pc.consumer.mapper.WorkerStopped() <- pc

	if len(errors) > 0 {
		return errors
	}
	return nil
}

// implements `mapper.Worker`.
func (pc *partitionConsumer) Assignment() chan<- mapper.Executor {
	return pc.assignmentCh
}

// pullMessages sends fetched requests to the broker consumer assigned by the
// redispatch goroutine; parses broker fetch responses and pushes parsed
// `ConsumerMessages` to the message channel. It tries to keep the message
// channel buffer full making fetch requests to the assigned broker as needed.
func (pc *partitionConsumer) pullMessages() {
	cid := pc.baseCID.NewChild("pullMessages")
	defer cid.LogScope()()
	var (
		assignedFetchRequestCh    chan<- fetchRequest
		nilOrFetchRequestsCh      chan<- fetchRequest
		fetchResultCh             = make(chan fetchResult, 1)
		nilOrFetchResultsCh       <-chan fetchResult
		nilOrMessagesCh           chan<- *ConsumerMessage
		nilOrReassignRetryTimerCh <-chan time.Time
		fetchedMessages           []*ConsumerMessage
		err                       error
		currMessage               *ConsumerMessage
		currMessageIdx            int
		lastReassignTime          time.Time
	)
	triggerOrScheduleReassign := func(reason string) {
		assignedFetchRequestCh = nil
		now := time.Now().UTC()
		if now.Sub(lastReassignTime) > pc.consumer.config.Consumer.Retry.Backoff {
			log.Infof("<%s> trigger reassign: reason=(%s)", cid, reason)
			lastReassignTime = now
			pc.consumer.mapper.WorkerReassign() <- pc
		} else {
			log.Infof("<%s> schedule reassign: reason=(%s)", cid, reason)
		}
		nilOrReassignRetryTimerCh = time.After(pc.consumer.config.Consumer.Retry.Backoff)
	}
pullMessagesLoop:
	for {
		select {
		case bw := <-pc.assignmentCh:
			log.Infof("<%s> assigned %s", cid, bw)
			if bw == nil {
				triggerOrScheduleReassign("no broker assigned")
				continue pullMessagesLoop
			}
			bc := bw.(*brokerConsumer)
			// A new leader broker has been assigned for the partition.
			assignedFetchRequestCh = bc.requestsCh
			// Cancel the reassign retry timer.
			nilOrReassignRetryTimerCh = nil
			// If there is a fetch request pending, then let it complete,
			// otherwise trigger one.
			if nilOrFetchResultsCh == nil && nilOrMessagesCh == nil {
				nilOrFetchRequestsCh = assignedFetchRequestCh
			}

		case nilOrFetchRequestsCh <- fetchRequest{pc.tp.topic, pc.tp.partition, pc.offset, pc.fetchSize, pc.lag, fetchResultCh}:
			nilOrFetchRequestsCh = nil
			nilOrFetchResultsCh = fetchResultCh

		case result := <-nilOrFetchResultsCh:
			nilOrFetchResultsCh = nil
			if fetchedMessages, err = pc.parseFetchResult(cid, result); err != nil {
				log.Infof("<%s> fetch failed: err=%s", cid, err)
				pc.reportError(err)
				if err == sarama.ErrOffsetOutOfRange {
					// There's no point in retrying this it will just fail the
					// same way, therefore is nothing to do but give up.
					goto done
				}
				triggerOrScheduleReassign("fetch error")
				continue pullMessagesLoop
			}
			// If no messages has been fetched, then trigger another request.
			if len(fetchedMessages) == 0 {
				nilOrFetchRequestsCh = assignedFetchRequestCh
				continue pullMessagesLoop
			}
			// Some messages have been fetched, start pushing them to the user.
			currMessageIdx = 0
			currMessage = fetchedMessages[currMessageIdx]
			nilOrMessagesCh = pc.messagesCh

		case nilOrMessagesCh <- currMessage:
			pc.offset = currMessage.Offset + 1
			currMessageIdx++
			if currMessageIdx < len(fetchedMessages) {
				currMessage = fetchedMessages[currMessageIdx]
				continue pullMessagesLoop
			}
			// All messages have been pushed, trigger a new fetch request.
			nilOrMessagesCh = nil
			nilOrFetchRequestsCh = assignedFetchRequestCh

		case <-nilOrReassignRetryTimerCh:
			pc.consumer.mapper.WorkerReassign() <- pc
			log.Infof("<%s> reassign triggered by timeout", cid)
			nilOrReassignRetryTimerCh = time.After(pc.consumer.config.Consumer.Retry.Backoff)

		case <-pc.closingCh:
			goto done
		}
	}
done:
	close(pc.messagesCh)
	close(pc.errorsCh)
	close(pc.closedCh)
}

// parseFetchResult parses a fetch response received a broker.
func (pc *partitionConsumer) parseFetchResult(cid *actor.ID, fetchResult fetchResult) ([]*ConsumerMessage, error) {
	if fetchResult.Err != nil {
		return nil, fetchResult.Err
	}

	response := fetchResult.Response
	if response == nil {
		return nil, sarama.ErrIncompleteResponse
	}

	block := response.GetBlock(pc.tp.topic, pc.tp.partition)
	if block == nil {
		return nil, sarama.ErrIncompleteResponse
	}

	if block.Err != sarama.ErrNoError {
		return nil, block.Err
	}

	if len(block.MsgSet.Messages) == 0 {
		// We got no messages. If we got a trailing one then we need to ask for more data.
		// Otherwise we just poll again and wait for one to be produced...
		if block.MsgSet.PartialTrailingMessage {
			if pc.consumer.config.Consumer.Fetch.Max > 0 && pc.fetchSize == pc.consumer.config.Consumer.Fetch.Max {
				// we can't ask for more data, we've hit the configured limit
				log.Infof("<%s> oversized message skipped: offset=%d", cid, pc.offset)
				pc.reportError(sarama.ErrMessageTooLarge)
				pc.offset++ // skip this one so we can keep processing future messages
			} else {
				pc.fetchSize *= 2
				if pc.consumer.config.Consumer.Fetch.Max > 0 && pc.fetchSize > pc.consumer.config.Consumer.Fetch.Max {
					pc.fetchSize = pc.consumer.config.Consumer.Fetch.Max
				}
			}
		}

		return nil, nil
	}

	// we got messages, reset our fetch size in case it was increased for a previous request
	pc.fetchSize = pc.consumer.config.Consumer.Fetch.Default
	var fetchedMessages []*ConsumerMessage
	for _, msgBlock := range block.MsgSet.Messages {
		for _, msg := range msgBlock.Messages() {
			if msg.Offset < pc.offset {
				continue
			}
			consumerMessage := &ConsumerMessage{
				Topic:         pc.tp.topic,
				Partition:     pc.tp.partition,
				Key:           msg.Msg.Key,
				Value:         msg.Msg.Value,
				Offset:        msg.Offset,
				HighWaterMark: block.HighWaterMarkOffset,
			}
			fetchedMessages = append(fetchedMessages, consumerMessage)
			pc.lag = block.HighWaterMarkOffset - msg.Offset
		}
	}

	if len(fetchedMessages) == 0 {
		return nil, sarama.ErrIncompleteResponse
	}
	return fetchedMessages, nil
}

// reportError sends partition consumer errors to the error channel if the user
// configured the consumer to do so via `Config.Consumer.Return.Errors`.
func (pc *partitionConsumer) reportError(err error) {
	if !pc.consumer.config.Consumer.Return.Errors {
		return
	}
	ce := &ConsumerError{
		Topic:     pc.tp.topic,
		Partition: pc.tp.partition,
		Err:       err,
	}
	select {
	case pc.errorsCh <- ce:
	default:
	}
}

func (pc *partitionConsumer) String() string {
	return pc.baseCID.String()
}

// brokerConsumer maintains a connection with a particular Kafka broker. It
// processes fetch requests from partition consumers and sends responses back.
// The dispatcher goroutine of the master consumer is responsible for keeping
// a broker consumer alive it is assigned to at least one partition consumer.
//
// implements `mapper.Executor`.
type brokerConsumer struct {
	baseCID         *actor.ID
	config          *sarama.Config
	conn            *sarama.Broker
	requestsCh      chan fetchRequest
	batchRequestsCh chan []fetchRequest
	wg              sync.WaitGroup
}

type fetchRequest struct {
	Topic     string
	Partition int32
	Offset    int64
	MaxBytes  int32
	Lag       int64
	ReplyToCh chan<- fetchResult
}

type fetchResult struct {
	Response *sarama.FetchResponse
	Err      error
}

// implements `mapper.Executor`.
func (bc *brokerConsumer) BrokerConn() *sarama.Broker {
	return bc.conn
}

// implements `mapper.Executor`.
func (bc *brokerConsumer) Stop() {
	close(bc.requestsCh)
	bc.wg.Wait()
}

// batchRequests collects fetch requests from partition consumers into batches
// while the request executor goroutine is busy processing the previous batch.
// As soon as the executor is done, a new batch is handed over to it.
func (bc *brokerConsumer) batchRequests() {
	cid := bc.baseCID.NewChild("batchRequests")
	defer cid.LogScope()()
	defer close(bc.batchRequestsCh)

	var nilOrBatchRequestCh chan<- []fetchRequest
	var batchRequest []fetchRequest
	for {
		select {
		case fr, ok := <-bc.requestsCh:
			if !ok {
				return
			}
			batchRequest = append(batchRequest, fr)
			nilOrBatchRequestCh = bc.batchRequestsCh
		case nilOrBatchRequestCh <- batchRequest:
			batchRequest = nil
			// Disable batchRequestsCh until we have at least one fetch request.
			nilOrBatchRequestCh = nil
		}
	}
}

// executeBatches executes fetch request received from partition consumers.
func (bc *brokerConsumer) executeBatches() {
	cid := bc.baseCID.NewChild("executeBatches")
	defer cid.LogScope()()

	var lastErr error
	var lastErrTime time.Time
	for fetchRequests := range bc.batchRequestsCh {
		// Reject consume requests for awhile after a connection failure to
		// allow the Kafka cluster some time to recuperate.
		if time.Now().UTC().Sub(lastErrTime) < bc.config.Consumer.Retry.Backoff {
			for _, fr := range fetchRequests {
				fr.ReplyToCh <- fetchResult{nil, lastErr}
			}
			continue
		}
		// Make a batch fetch request for all hungry partition consumers.
		req := &sarama.FetchRequest{
			MinBytes:    bc.config.Consumer.Fetch.Min,
			MaxWaitTime: int32(bc.config.Consumer.MaxWaitTime / time.Millisecond),
		}
		for _, fr := range fetchRequests {
			req.AddBlock(fr.Topic, fr.Partition, fr.Offset, fr.MaxBytes)
		}
		var res *sarama.FetchResponse
		res, lastErr = bc.conn.Fetch(req)
		if lastErr != nil {
			lastErrTime = time.Now().UTC()
			bc.conn.Close()
			log.Infof("<%s> connection reset: err=(%s)", cid, lastErr)
		}
		// Fan the response out to the partition consumers.
		for _, fr := range fetchRequests {
			fr.ReplyToCh <- fetchResult{res, lastErr}
		}
	}
}

func (bc *brokerConsumer) String() string {
	if bc == nil {
		return "<nil>"
	}
	return bc.baseCID.String()
}
