// httpproxy.go
package dispatcher

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// handleHTTPConnection serves one client of the HTTP proxy listener.
//
// Windows' system proxy settings speak HTTP, not SOCKS5, so this is the
// protocol that makes the dispatcher usable system wide. Two request shapes
// arrive here:
//
//	CONNECT example.com:443 HTTP/1.1     (HTTPS and anything tunnelled)
//	GET http://example.com/x HTTP/1.1    (plain HTTP, absolute-form)
func (d *Dispatcher) handleHTTPConnection(conn net.Conn) {
	reader := bufio.NewReader(conn)

	req, err := http.ReadRequest(reader)
	if err != nil {
		// A client that connects and leaves is routine, not worth logging.
		conn.Close()
		return
	}

	if req.Method == http.MethodConnect {
		d.handleHTTPConnect(conn, req)
		return
	}
	d.handleHTTPForward(conn, reader, req)
}

// handleHTTPConnect establishes a blind tunnel, which is how HTTPS traffic
// passes through an HTTP proxy.
func (d *Dispatcher) handleHTTPConnect(conn net.Conn, req *http.Request) {
	address := withDefaultPort(req.Host, "443")

	lb, i, err := d.selectLoadBalancer(address)
	if err != nil {
		log.Println("[WARN]", address, "cannot be dispatched:", err)
		writeHTTPError(conn, http.StatusBadGateway)
		conn.Close()
		return
	}

	remoteConn, err := dialFromLB(lb, i, address)
	if err != nil {
		d.recordDialFailure(lb, i, address, err)
		writeHTTPError(conn, http.StatusBadGateway)
		conn.Close()
		return
	}

	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		remoteConn.Close()
		conn.Close()
		return
	}

	atomic.AddUint64(&lb.ConnectionsHandled, 1)
	log.Println("[DEBUG]", address, "->", lb.Address, "LB:", i)
	pipeConnections(conn, remoteConn, lb)
}

// handleHTTPForward relays plain HTTP requests, serving every request that
// arrives on this client connection rather than only the first.
//
// A client keeps one connection to the proxy alive and sends requests for
// different hosts down it, so the destination is resolved per request. An
// earlier version piped the connection straight through after the first
// request, which meant a later request for another host was answered by the
// first host -- the wrong site's content entirely.
func (d *Dispatcher) handleHTTPForward(conn net.Conn, reader *bufio.Reader, req *http.Request) {
	defer conn.Close()

	for {
		if !d.forwardOneRequest(conn, reader, req) {
			return
		}

		// Read the next request from the same client connection.
		next, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		req = next
	}
}

// forwardOneRequest relays a single request/response exchange. It reports
// whether the client connection may carry another request afterwards.
func (d *Dispatcher) forwardOneRequest(conn net.Conn, reader *bufio.Reader, req *http.Request) bool {
	if req.URL == nil || req.URL.Host == "" {
		// Not a proxy-style request: a browser pointed straight at us, or a
		// probe. Nothing sensible to forward.
		writeHTTPError(conn, http.StatusBadRequest)
		return false
	}

	address := withDefaultPort(req.URL.Host, "80")

	lb, i, err := d.selectLoadBalancer(address)
	if err != nil {
		log.Println("[WARN]", address, "cannot be dispatched:", err)
		writeHTTPError(conn, http.StatusBadGateway)
		return false
	}

	remoteConn, err := dialFromLB(lb, i, address)
	if err != nil {
		d.recordDialFailure(lb, i, address, err)
		writeHTTPError(conn, http.StatusBadGateway)
		return false
	}
	// Byte counters are maintained by the wrapper rather than by
	// pipeConnections, which is not used on this path.
	counted := newCountingConn(remoteConn, lb)
	defer counted.Close()

	atomic.AddUint64(&lb.ConnectionsHandled, 1)
	log.Println("[DEBUG]", address, "->", lb.Address, "LB:", i)

	// Whether the *client* connection may be reused has to be decided from
	// the request as it arrived, before req.Close is repurposed below for
	// the upstream connection.
	clientMayReuse := !req.Close &&
		!requestedClose(req) &&
		req.ProtoMajor == 1 && req.ProtoMinor >= 1

	// Hop-by-hop headers must not reach the origin server.
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")

	// One upstream connection per request: the client's connection is kept
	// alive independently, and this avoids having to track a pool of
	// upstream connections keyed by host and link.
	req.Close = true

	// Write emits origin-form when RequestURI is empty and URL carries the
	// path, which is what an origin server expects.
	if err := req.Write(counted); err != nil {
		return false
	}

	resp, err := http.ReadResponse(bufio.NewReader(counted), req)
	if err != nil {
		writeHTTPError(conn, http.StatusBadGateway)
		return false
	}
	defer resp.Body.Close()

	// A protocol upgrade (WebSocket and friends) turns the rest of the
	// exchange into an opaque tunnel.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		if err := resp.Write(conn); err != nil {
			return false
		}
		tunnel(newBufferedConn(conn, reader), counted)
		return false
	}

	// The upstream connection closes after this response, so tell the
	// client only what applies to its own connection.
	resp.Close = false
	resp.Header.Del("Connection")

	if err := resp.Write(conn); err != nil {
		return false
	}

	return clientMayReuse
}

// requestedClose reports whether the client asked for the connection to end
// after this exchange.
func requestedClose(req *http.Request) bool {
	for _, value := range req.Header.Values("Connection") {
		if strings.EqualFold(strings.TrimSpace(value), "close") {
			return true
		}
	}
	return false
}

// tunnel joins two connections and waits for either direction to finish,
// used once a connection stops being request/response shaped.
func tunnel(client, remote net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(remote, client)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(client, remote)
		done <- struct{}{}
	}()
	<-done
}

// withDefaultPort appends a port when the address carries none.
func withDefaultPort(host, defaultPort string) string {
	if host == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	// Bare IPv6 literals must be bracketed before a port is attached.
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return fmt.Sprintf("[%s]:%s", host, defaultPort)
	}
	return net.JoinHostPort(host, defaultPort)
}

// writeHTTPError reports a failure to the client in a form browsers show
// sensibly, instead of dropping the connection with no explanation.
func writeHTTPError(conn net.Conn, status int) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
		status, http.StatusText(status))
}

// bufferedConn presents a net.Conn whose reads come from a bufio.Reader that
// may already hold bytes read off the wire.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func newBufferedConn(conn net.Conn, reader *bufio.Reader) net.Conn {
	return &bufferedConn{Conn: conn, reader: reader}
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// countingConn tallies bytes against a load balancer for paths that do not
// use pipeConnections, so live stats stay accurate for plain HTTP too.
type countingConn struct {
	net.Conn
	lb *LoadBalancer
}

func newCountingConn(conn net.Conn, lb *LoadBalancer) net.Conn {
	return &countingConn{Conn: conn, lb: lb}
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	atomic.AddUint64(&c.lb.BytesReceived, uint64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	atomic.AddUint64(&c.lb.BytesSent, uint64(n))
	return n, err
}
