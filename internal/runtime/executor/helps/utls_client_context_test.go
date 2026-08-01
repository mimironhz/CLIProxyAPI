package helps

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

var errTestDialReleased = errors.New("test dial released")

type controlledContextDialer struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

type controlledLegacyDialer struct {
	started    chan struct{}
	release    chan struct{}
	connection net.Conn
}

type staticContextDialer struct {
	started    chan struct{}
	connection net.Conn
}

func (d *staticContextDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *staticContextDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	close(d.started)
	return d.connection, nil
}

func (d *controlledLegacyDialer) Dial(_, _ string) (net.Conn, error) {
	close(d.started)
	<-d.release
	return d.connection, nil
}

func (d *controlledContextDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *controlledContextDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	d.calls.Add(1)
	select {
	case d.started <- struct{}{}:
	default:
	}
	select {
	case <-d.release:
		return nil, errTestDialReleased
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestUtlsConnectionAcquisitionHonorsCreatorCancellation(t *testing.T) {
	dialer := &controlledContextDialer{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	roundTripper := newUtlsRoundTripper("")
	roundTripper.dialer = dialer
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, errConnection := roundTripper.getOrCreateConnection(ctx, "chatgpt.com", "chatgpt.com:443")
		result <- errConnection
	}()
	waitForUtlsTestSignal(t, dialer.started)
	cancel()
	if errConnection := waitForUtlsTestResult(t, result); !errors.Is(errConnection, context.Canceled) {
		t.Fatalf("connection error = %v, want context canceled", errConnection)
	}
}

func TestUtlsConnectionAcquisitionHonorsWaiterCancellation(t *testing.T) {
	dialer := &controlledContextDialer{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	roundTripper := newUtlsRoundTripper("")
	roundTripper.dialer = dialer
	creatorResult := make(chan error, 1)
	go func() {
		_, errConnection := roundTripper.getOrCreateConnection(context.Background(), "chatgpt.com", "chatgpt.com:443")
		creatorResult <- errConnection
	}()
	waitForUtlsTestSignal(t, dialer.started)

	waiterContext, cancelWaiter := context.WithCancel(context.Background())
	waiterResult := make(chan error, 1)
	go func() {
		_, errConnection := roundTripper.getOrCreateConnection(waiterContext, "chatgpt.com", "chatgpt.com:443")
		waiterResult <- errConnection
	}()
	cancelWaiter()
	if errConnection := waitForUtlsTestResult(t, waiterResult); !errors.Is(errConnection, context.Canceled) {
		t.Fatalf("waiter error = %v, want context canceled", errConnection)
	}
	if got := dialer.calls.Load(); got != 1 {
		t.Fatalf("dial calls while waiter was pending = %d, want 1", got)
	}

	close(dialer.release)
	if errConnection := waitForUtlsTestResult(t, creatorResult); !errors.Is(errConnection, errTestDialReleased) {
		t.Fatalf("creator error = %v, want released dial", errConnection)
	}
}

func TestUtlsConnectionAcquisitionHonorsHandshakeCancellation(t *testing.T) {
	clientConnection, serverConnection := net.Pipe()
	t.Cleanup(func() {
		_ = clientConnection.Close()
		_ = serverConnection.Close()
	})
	dialer := &staticContextDialer{
		started:    make(chan struct{}),
		connection: clientConnection,
	}
	roundTripper := newUtlsRoundTripper("")
	roundTripper.dialer = dialer
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, errConnection := roundTripper.getOrCreateConnection(ctx, "chatgpt.com", "chatgpt.com:443")
		result <- errConnection
	}()
	waitForUtlsTestSignal(t, dialer.started)
	cancel()
	if errConnection := waitForUtlsTestResult(t, result); !errors.Is(errConnection, context.Canceled) {
		t.Fatalf("handshake error = %v, want context canceled", errConnection)
	}
}

func TestDialProxyContextClosesLateLegacyConnection(t *testing.T) {
	clientConnection, serverConnection := net.Pipe()
	t.Cleanup(func() {
		_ = clientConnection.Close()
		_ = serverConnection.Close()
	})
	if errDeadline := serverConnection.SetReadDeadline(time.Now().Add(2 * time.Second)); errDeadline != nil {
		t.Fatalf("set peer read deadline: %v", errDeadline)
	}
	dialer := &controlledLegacyDialer{
		started:    make(chan struct{}),
		release:    make(chan struct{}),
		connection: clientConnection,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, errDial := dialProxyContext(ctx, dialer, "tcp", "chatgpt.com:443")
		result <- errDial
	}()
	waitForUtlsTestSignal(t, dialer.started)
	cancel()
	close(dialer.release)
	if errDial := waitForUtlsTestResult(t, result); !errors.Is(errDial, context.Canceled) {
		t.Fatalf("legacy dial error = %v, want context canceled", errDial)
	}
	if _, errRead := serverConnection.Read(make([]byte, 1)); errRead == nil {
		t.Fatal("late legacy connection remained open after cancellation")
	} else if netError, ok := errRead.(net.Error); ok && netError.Timeout() {
		t.Fatalf("late legacy connection was not closed: %v", errRead)
	}
}

func waitForUtlsTestSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for uTLS test signal")
	}
}

func waitForUtlsTestResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case errResult := <-result:
		return errResult
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for uTLS test result")
		return nil
	}
}
