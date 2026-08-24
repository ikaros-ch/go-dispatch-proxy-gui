package dispatcher

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// TestSocks5DialAgainstOwnServer drives the client handshake in proxytest.go
// against this package's own SOCKS5 server code, so the two halves are
// verified to agree on the wire format.
func TestSocks5DialAgainstOwnServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not listen: %v", err)
	}
	defer listener.Close()

	type serverOutcome struct {
		address string
		err     error
	}
	outcomes := make(chan serverOutcome, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			outcomes <- serverOutcome{err: err}
			return
		}
		defer conn.Close()

		address, err := handleSocksConnection(conn)
		if err != nil {
			outcomes <- serverOutcome{err: err}
			return
		}

		// Mirror the success reply the real server sends before piping.
		if _, err := conn.Write([]byte{5, SUCCESS, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			outcomes <- serverOutcome{err: err}
			return
		}
		outcomes <- serverOutcome{address: address}

		// Echo so the client can confirm the stream is usable afterwards.
		io.Copy(conn, conn)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := socks5Dial(ctx, listener.Addr().String(), "example.com:443")
	if err != nil {
		t.Fatalf("socks5Dial failed: %v", err)
	}
	defer conn.Close()

	outcome := <-outcomes
	if outcome.err != nil {
		t.Fatalf("server side failed: %v", outcome.err)
	}
	if outcome.address != "example.com:443" {
		t.Errorf("server saw destination %q, want %q", outcome.address, "example.com:443")
	}

	// The connection must be positioned at payload, with no reply bytes left
	// unconsumed by discardBoundAddr.
	payload := []byte("ping")
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write after handshake failed: %v", err)
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read after handshake failed: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("echoed %q, want %q -- handshake left unread bytes in the stream", got, payload)
	}
}

// TestSuggestRatiosUsesDownload confirms ratios scale with download speed and
// that failed or unmeasured interfaces fall back to 1.
func TestSuggestRatiosUsesDownload(t *testing.T) {
	results := []TestResult{
		{IP: "10.0.0.1", DownloadBps: 1_000_000, UploadBps: 10},
		{IP: "10.0.0.2", DownloadBps: 3_000_000, UploadBps: 10},
		{IP: "10.0.0.3", Error: "dial failed"},
		{IP: "10.0.0.4", DownloadBps: 0},
	}

	ratios := SuggestRatios(results)

	if ratios["10.0.0.1"] != 1 {
		t.Errorf("slowest interface got ratio %d, want 1", ratios["10.0.0.1"])
	}
	if ratios["10.0.0.2"] != 3 {
		t.Errorf("3x faster interface got ratio %d, want 3", ratios["10.0.0.2"])
	}
	if ratios["10.0.0.3"] != 1 {
		t.Errorf("failed interface got ratio %d, want 1", ratios["10.0.0.3"])
	}
	if ratios["10.0.0.4"] != 1 {
		t.Errorf("unmeasured interface got ratio %d, want 1", ratios["10.0.0.4"])
	}
}
