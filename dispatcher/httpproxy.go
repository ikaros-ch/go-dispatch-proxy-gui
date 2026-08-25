// httpproxy.go
package dispatcher

import (
	"bufio"
	"fmt"
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

	lb, i := d.getLoadBalancer()
	remoteConn, err := dialFromLB(lb, i, address)
	if err != nil {
		d.setLastError(lb, err.Error())
		log.Println("[WARN]", address, "->", lb.Address, fmt.Sprintf("{%s}", err), "LB:", i)
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

// handleHTTPForward relays a plain HTTP request. The request line is
// rewritten from absolute-form to origin-form, since that is what an origin
// server expects to receive.
func (d *Dispatcher) handleHTTPForward(conn net.Conn, reader *bufio.Reader, req *http.Request) {
	if req.URL == nil || req.URL.Host == "" {
		// Not a proxy-style request: a browser pointed straight at us, or a
		// probe. Nothing sensible to forward.
		writeHTTPError(conn, http.StatusBadRequest)
		conn.Close()
		return
	}

	address := withDefaultPort(req.URL.Host, "80")

	lb, i := d.getLoadBalancer()
	remoteConn, err := dialFromLB(lb, i, address)
	if err != nil {
		d.setLastError(lb, err.Error())
		log.Println("[WARN]", address, "->", lb.Address, fmt.Sprintf("{%s}", err), "LB:", i)
		writeHTTPError(conn, http.StatusBadGateway)
		conn.Close()
		return
	}

	// Hop-by-hop headers must not be forwarded to the origin server.
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")

	// Write returns the request in origin-form when RequestURI is empty and
	// URL carries the path, which is exactly what an origin server wants.
	if err := req.Write(remoteConn); err != nil {
		remoteConn.Close()
		conn.Close()
		return
	}

	atomic.AddUint64(&lb.ConnectionsHandled, 1)
	log.Println("[DEBUG]", address, "->", lb.Address, "LB:", i)

	// Anything the client already buffered past the request head still needs
	// to reach the server, so hand over the buffered reader rather than the
	// bare connection.
	pipeConnections(newBufferedConn(conn, reader), remoteConn, lb)
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
