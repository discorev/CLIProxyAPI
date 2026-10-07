package executor

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// A SOCKS5 proxy that accepts the connection but never answers the handshake
// must not hold the dial once the request is cancelled; otherwise an abandoned
// pooled request keeps its goroutine, proxy socket and pool reservation.
func TestNewProxyAwareWebsocketDialerSOCKS5HonoursCancellation(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("net.Listen() error = %v", errListen)
	}
	t.Cleanup(func() { _ = listener.Close() })
	greeted := make(chan struct{})
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			return
		}
		t.Cleanup(func() { _ = conn.Close() })
		// Read the client greeting, then stall without replying.
		buf := make([]byte, 16)
		if _, errRead := conn.Read(buf); errRead == nil {
			close(greeted)
		}
	}()

	auth := &cliproxyauth.Auth{ProxyURL: "socks5://" + listener.Addr().String()}
	dialer := newProxyAwareWebsocketDialer(context.Background(), nil, auth)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, errDial := dialer.NetDialContext(ctx, "tcp", "chatgpt.com:443")
		if conn != nil {
			_ = conn.Close()
		}
		result <- errDial
	}()

	select {
	case <-greeted:
	case <-time.After(10 * time.Second):
		t.Fatal("SOCKS5 greeting never reached the proxy")
	}
	cancel()
	select {
	case errDial := <-result:
		if errDial == nil {
			t.Fatal("dial succeeded against a stalled proxy")
		}
		if !errors.Is(errDial, context.Canceled) {
			t.Logf("dial error = %v", errDial)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dial ignored cancellation while the SOCKS5 handshake stalled")
	}
}
