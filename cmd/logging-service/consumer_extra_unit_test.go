package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"wallet-system/pkg/observability"
)

type mockChannel struct {
	exchangeErr     error
	exchangeErrCall int
	exchangeCalls   int
	queueErr        error
	queueErrCall    int
	queueCalls      int
	bindErr         error
	bindErrCall     int
	bindCalls       int
	qosErr          error
	consumeErr      error
	deliveryCh      chan amqp.Delivery
	closed          bool
}

func (m *mockChannel) ExchangeDeclare(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error {
	m.exchangeCalls++
	if m.exchangeErrCall > 0 && m.exchangeCalls == m.exchangeErrCall {
		return m.exchangeErr
	}
	if m.exchangeErrCall == 0 && m.exchangeErr != nil {
		return m.exchangeErr
	}
	return nil
}
func (m *mockChannel) QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error) {
	m.queueCalls++
	if m.queueErrCall > 0 && m.queueCalls == m.queueErrCall {
		return amqp.Queue{}, m.queueErr
	}
	if m.queueErrCall == 0 && m.queueErr != nil {
		return amqp.Queue{}, m.queueErr
	}
	return amqp.Queue{Name: name}, nil
}
func (m *mockChannel) QueueBind(name, key, exchange string, noWait bool, args amqp.Table) error {
	m.bindCalls++
	if m.bindErrCall > 0 && m.bindCalls == m.bindErrCall {
		return m.bindErr
	}
	if m.bindErrCall == 0 && m.bindErr != nil {
		return m.bindErr
	}
	return nil
}
func (m *mockChannel) Qos(prefetchCount, prefetchSize int, global bool) error {
	return m.qosErr
}
func (m *mockChannel) Consume(queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error) {
	if m.consumeErr != nil {
		return nil, m.consumeErr
	}
	return m.deliveryCh, nil
}
func (m *mockChannel) Close() error {
	m.closed = true
	return nil
}
func (m *mockChannel) IsClosed() bool {
	return m.closed
}

type mockConnection struct {
	ch     *mockChannel
	chErr  error
	closed bool
}

func (m *mockConnection) Channel() (AMQPChannel, error) {
	if m.chErr != nil {
		return nil, m.chErr
	}
	return m.ch, nil
}
func (m *mockConnection) IsClosed() bool {
	return m.closed
}
func (m *mockConnection) Close() error {
	m.closed = true
	return nil
}

func TestConsumer_HelpersAndBackoff(t *testing.T) {
	// 1. Test realChannel and realConnection nil guards and non-nil wrappers
	var rChan *realChannel
	if !rChan.IsClosed() {
		t.Errorf("Expected nil realChannel to return true for IsClosed")
	}
	rChanNilInner := &realChannel{}
	if !rChanNilInner.IsClosed() {
		t.Errorf("Expected realChannel with nil Channel to return true for IsClosed")
	}
	rChanVal := &realChannel{Channel: &amqp.Channel{}}
	_ = rChanVal.IsClosed()

	var rConn *realConnection
	if !rConn.IsClosed() {
		t.Errorf("Expected nil realConnection to return true for IsClosed")
	}
	rConnNilInner := &realConnection{}
	if !rConnNilInner.IsClosed() {
		t.Errorf("Expected realConnection with nil Connection to return true for IsClosed")
	}
	if _, err := rConnNilInner.Channel(); err == nil {
		t.Errorf("Expected error calling Channel on realConnection with nil Connection")
	}
	rConnVal := &realConnection{Connection: &amqp.Connection{}}
	_ = rConnVal.IsClosed()

	// 2. Test queueTypeArgs classic (default) vs quorum
	os.Unsetenv("LOGGING_QUEUE_TYPE")
	args := queueTypeArgs()
	if args != nil {
		t.Errorf("Expected nil args for classic queue, got %v", args)
	}

	os.Setenv("LOGGING_QUEUE_TYPE", "quorum")
	argsQuorum := queueTypeArgs()
	if argsQuorum == nil || argsQuorum["x-queue-type"] != "quorum" {
		t.Errorf("Expected quorum args for quorum queue, got %v", argsQuorum)
	}
	os.Unsetenv("LOGGING_QUEUE_TYPE")

	// 3. Test waitBackoff with context timeout vs cancelled
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled

	backoff := 10 * time.Millisecond
	ok := waitBackoff(ctx, &backoff, 100*time.Millisecond)
	if ok {
		t.Errorf("Expected waitBackoff to return false for cancelled context")
	}

	// Active waitBackoff
	ctxActive, cancelActive := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelActive()
	backoffActive := 1 * time.Millisecond
	okActive := waitBackoff(ctxActive, &backoffActive, 10*time.Millisecond)
	if !okActive {
		t.Errorf("Expected waitBackoff to return true for active timer")
	}
	if backoffActive != 2*time.Millisecond {
		t.Errorf("Expected backoff to double to 2ms, got %v", backoffActive)
	}

	// 4. Test Consumer cleanup and Close with nil connection
	c := &Consumer{}
	c.cleanup()
	if err := c.Close(); err != nil {
		t.Errorf("Expected nil error from Close on empty consumer, got %v", err)
	}

	// 5. Test NewConsumer error branch (invalid URI)
	if _, err := NewConsumer("amqp://invalid:12345", &mockRepo{}); err == nil {
		t.Errorf("Expected error from NewConsumer with invalid URI")
	}

	// 6. Test NewConsumerWithDialer success branch
	ch := &mockChannel{deliveryCh: make(chan amqp.Delivery, 5)}
	conn := &mockConnection{ch: ch}
	mockDialer := func(string) (AMQPConnection, error) {
		return conn, nil
	}
	cSuccess, err := NewConsumerWithDialer("amqp://mock", &mockRepo{}, mockDialer)
	if err != nil || cSuccess == nil {
		t.Errorf("Expected NewConsumerWithDialer to succeed with mock dialer, got %v, err=%v", cSuccess, err)
	}
}

func TestConsumer_EnsureConnection_CancelledContext(t *testing.T) {
	c := &Consumer{
		amqpURI: "amqp://invalid:12345",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	backoff := 1 * time.Millisecond
	err := c.ensureConnection(ctx, &backoff, 10*time.Millisecond)
	if err == nil {
		t.Errorf("Expected error from ensureConnection with cancelled context and invalid URI")
	}
}

func TestConsumer_MockedAMQP_FullLifecycle(t *testing.T) {
	ch := &mockChannel{
		deliveryCh: make(chan amqp.Delivery, 10),
	}
	conn := &mockConnection{ch: ch}

	c := &Consumer{
		amqpURI: "amqp://mock:5672",
		repo:    &mockRepo{},
		dialer: func(string) (AMQPConnection, error) {
			return conn, nil
		},
	}

	// 1. Test connect success
	os.Setenv("LOGGING_QUEUE_TYPE", "quorum")
	defer os.Unsetenv("LOGGING_QUEUE_TYPE")

	if err := c.connect(); err != nil {
		t.Fatalf("Expected connect to succeed with mock AMQP, got %v", err)
	}

	// 2. Test ensureConnection when connection is open
	backoff := 1 * time.Second
	if err := c.ensureConnection(context.Background(), &backoff, 5*time.Second); err != nil {
		t.Errorf("Expected ensureConnection to return nil for active connection, got %v", err)
	}

	// 3. Test registerConsumer
	msgs, err := c.registerConsumer(context.Background(), &backoff, 5*time.Second)
	if err != nil || msgs == nil {
		t.Errorf("Expected registerConsumer to succeed, got err=%v", err)
	}

	// 4. Test Start loop with pre-canceled context
	ctxCancel, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Start(ctxCancel); err != nil {
		t.Errorf("Expected nil error from Start on pre-canceled context, got %v", err)
	}

	// 5. Test Close with active channel and conn
	if err := c.Close(); err != nil {
		t.Errorf("Expected Close to succeed, got %v", err)
	}

	// 6. Test connect failure branches
	// a. Channel fail
	conn.chErr = errors.New("channel failed")
	if err := c.connect(); err == nil {
		t.Errorf("Expected connect to fail on channel error")
	}
	conn.chErr = nil

	// b. DLX ExchangeDeclare fail
	ch.exchangeErr = errors.New("dlx exchange failed")
	if err := c.connect(); err == nil {
		t.Errorf("Expected connect to fail on dlx exchange error")
	}
	ch.exchangeErr = nil

	// b2. Main ExchangeDeclare fail (call 2)
	ch.exchangeErr = errors.New("main exchange failed")
	ch.exchangeErrCall = 2
	ch.exchangeCalls = 0
	if err := c.connect(); err == nil {
		t.Errorf("Expected connect to fail on main exchange error")
	}
	ch.exchangeErr = nil
	ch.exchangeErrCall = 0

	// c. DLQ QueueDeclare fail
	ch.queueErr = errors.New("dlq queue failed")
	if err := c.connect(); err == nil {
		t.Errorf("Expected connect to fail on dlq queue error")
	}
	ch.queueErr = nil

	// c2. Main QueueDeclare fail (call 2)
	ch.queueErr = errors.New("main queue failed")
	ch.queueErrCall = 2
	ch.queueCalls = 0
	if err := c.connect(); err == nil {
		t.Errorf("Expected connect to fail on main queue error")
	}
	ch.queueErr = nil
	ch.queueErrCall = 0

	// d. DLQ QueueBind fail
	ch.bindErr = errors.New("dlq bind failed")
	if err := c.connect(); err == nil {
		t.Errorf("Expected connect to fail on dlq bind error")
	}
	ch.bindErr = nil

	// d2. Main QueueBind fail (call 2)
	ch.bindErr = errors.New("main bind failed")
	ch.bindErrCall = 2
	ch.bindCalls = 0
	if err := c.connect(); err == nil {
		t.Errorf("Expected connect to fail on main bind error")
	}
	ch.bindErr = nil
	ch.bindErrCall = 0

	// e. Qos fail
	ch.qosErr = errors.New("qos failed")
	if err := c.connect(); err == nil {
		t.Errorf("Expected connect to fail on qos error")
	}
	ch.qosErr = nil
}

func TestConsumer_ConsumeLoopAndProcessMessage(t *testing.T) {
	repo := &mockRepo{}
	c := &Consumer{
		repo: repo,
	}

	validEv := observability.Event{
		SchemaVersion: 1,
		EventID:       "ev-consume-loop-1",
		OccurredAt:    time.Now().UTC(),
		Service:       "test-service",
		Environment:   "test",
		Level:         observability.LevelInfo,
		EventType:     "test.event",
		AssociationID: "assoc-1",
	}
	validBytes, _ := json.Marshal(validEv)

	// 1. Process valid message
	ack1 := &mockAcknowledger{}
	c.processMessage(context.Background(), amqp.Delivery{Body: validBytes, Acknowledger: ack1})
	if ack1.ackCount != 1 {
		t.Errorf("Expected ack1 count 1, got %d", ack1.ackCount)
	}

	// 2. Process invalid JSON
	ack2 := &mockAcknowledger{}
	c.processMessage(context.Background(), amqp.Delivery{Body: []byte("not-json"), Acknowledger: ack2})
	if ack2.nackCount != 1 {
		t.Errorf("Expected ack2 nack count 1, got %d", ack2.nackCount)
	}

	// 3. Process invalid Schema
	invalidEv := validEv
	invalidEv.EventID = ""
	invalidBytes, _ := json.Marshal(invalidEv)
	ack3 := &mockAcknowledger{}
	c.processMessage(context.Background(), amqp.Delivery{Body: invalidBytes, Acknowledger: ack3})
	if ack3.nackCount != 1 {
		t.Errorf("Expected ack3 nack count 1, got %d", ack3.nackCount)
	}

	// 4. Process transient repo failure (retries)
	repo.errToReturn = errors.New("save failed")
	ack4 := &mockAcknowledger{}
	c.processMessage(context.Background(), amqp.Delivery{Body: validBytes, Acknowledger: ack4})
	if ack4.nackCount != 1 {
		t.Errorf("Expected ack4 nack count 1 after max retries, got %d", ack4.nackCount)
	}

	// 5. Test consumeLoop channel close
	deliveryCh := make(chan amqp.Delivery)
	close(deliveryCh)
	if err := c.consumeLoop(context.Background(), deliveryCh); err == nil {
		t.Errorf("Expected error when consumer channel closes")
	}
}

func TestConsumer_StartLoopCoverage(t *testing.T) {
	deliveryCh := make(chan amqp.Delivery, 5)
	ch := &mockChannel{
		deliveryCh: deliveryCh,
	}
	conn := &mockConnection{ch: ch}

	c := &Consumer{
		amqpURI: "amqp://mock:5672",
		repo:    &mockRepo{},
		dialer: func(string) (AMQPConnection, error) {
			return conn, nil
		},
	}

	// Close delivery channel immediately so consumeLoop exits with "consumer channel closed"
	close(deliveryCh)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Start will run ensureConnection -> registerConsumer -> consumeLoop (exits) -> select case <-ctx.Done()
	err := c.Start(ctx)
	if err != nil {
		t.Errorf("Expected nil error from Start on context done, got %v", err)
	}

	// 2. Test registerConsumer error branch
	cErr := &Consumer{
		ch: ch,
	}
	ch.consumeErr = errors.New("consume register failed")
	ctxRegErr, cancelRegErr := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelRegErr()
	bReg := 1 * time.Millisecond
	_, errReg := cErr.registerConsumer(ctxRegErr, &bReg, 10*time.Millisecond)
	if errReg == nil {
		t.Errorf("Expected registerConsumer to return error when consume fails")
	}

	// Test Start when registerConsumer fails and context is cancelled
	ctxStartReg, cancelStartReg := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelStartReg()
	_ = c.Start(ctxStartReg)
	ch.consumeErr = nil

	// 3. Test ensureConnection error branch when reconnect fails
	conn.chErr = errors.New("reconnect fail")
	ctxConnErr, cancelConnErr := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelConnErr()
	bConn := 1 * time.Millisecond
	errConn := c.ensureConnection(ctxConnErr, &bConn, 10*time.Millisecond)
	if errConn == nil {
		t.Errorf("Expected error from ensureConnection when reconnect fails")
	}

	// Test Start when ensureConnection fails and context is cancelled
	ctxStartConn, cancelStartConn := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelStartConn()
	_ = c.Start(ctxStartConn)
}
