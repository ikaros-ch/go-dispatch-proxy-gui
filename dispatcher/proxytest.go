// proxytest.go
package dispatcher

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// TestThroughProxy measures latency, download and upload as seen by a client
// actually using the running proxy, rather than by binding to one interface
// directly. This is the end-to-end figure: it exercises dispatching, so the
// result reflects the combined capacity across all load balancers.
func TestThroughProxy(ctx context.Context, proxyAddr string, testURL string, duration time.Duration) TestResult {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return socks5Dial(ctx, proxyAddr, addr)
			},
		},
		Timeout: 60 * time.Second,
	}
	return measureClient(ctx, client, testURL, duration)
}

// socks5Dial opens a connection to addr ("host:port") through the SOCKS5
// proxy listening at proxyAddr, using the no-authentication method. This is
// the client side of the handshake implemented in socks.go.
func socks5Dial(ctx context.Context, proxyAddr, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid destination port %q", portStr)
	}
	if len(host) > 255 {
		return nil, fmt.Errorf("destination host too long: %q", host)
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("could not reach proxy at %s: %w", proxyAddr, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	if err := socks5Handshake(conn, host, port); err != nil {
		conn.Close()
		return nil, err
	}

	// Clear the handshake deadline; the caller manages transfer timeouts.
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// socks5Handshake performs greeting and CONNECT against an open proxy conn.
func socks5Handshake(conn net.Conn, host string, port int) error {
	// Greeting: version 5, one method, no authentication.
	if _, err := conn.Write([]byte{socksVersion5, 1, NOAUTH}); err != nil {
		return fmt.Errorf("proxy greeting failed: %w", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("proxy did not answer greeting: %w", err)
	}
	if reply[0] != socksVersion5 {
		return fmt.Errorf("unexpected SOCKS version %d from proxy", reply[0])
	}
	if reply[1] != NOAUTH {
		return fmt.Errorf("proxy requires authentication method %d", reply[1])
	}

	// CONNECT request. A name is sent as-is so the proxy resolves it on the
	// interface it selects; literals are sent in their own address form,
	// since the domain form cannot represent an IPv6 address unambiguously.
	req := []byte{socksVersion5, CONNECT, 0x00}
	switch ip := net.ParseIP(host); {
	case ip == nil:
		req = append(req, DOMAIN, byte(len(host)))
		req = append(req, host...)
	case ip.To4() != nil:
		req = append(req, IPV4)
		req = append(req, ip.To4()...)
	default:
		req = append(req, IPV6)
		req = append(req, ip.To16()...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("proxy CONNECT failed: %w", err)
	}

	// Reply header: version, status, reserved, address type.
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("proxy did not answer CONNECT: %w", err)
	}
	if head[1] != SUCCESS {
		return fmt.Errorf("proxy refused connection to %s:%d (status %d)", host, port, head[1])
	}

	// Consume the bound address so the stream is positioned at payload.
	return discardBoundAddr(conn, head[3])
}

// discardBoundAddr reads and drops the variable-length bound address that
// closes a SOCKS5 reply.
func discardBoundAddr(conn net.Conn, addrType byte) error {
	var n int
	switch addrType {
	case IPV4:
		n = net.IPv4len
	case IPV6:
		n = net.IPv6len
	case DOMAIN:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return err
		}
		n = int(length[0])
	default:
		return fmt.Errorf("unsupported address type %d in proxy reply", addrType)
	}
	// Address bytes plus the two-byte port.
	_, err := io.ReadFull(conn, make([]byte, n+2))
	return err
}
