package dispatcher

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// startLoopbackProxy starts an HTTP proxy dispatching over the loopback
// address, so the test needs no real interface or internet access.
func startLoopbackProxy(t *testing.T) (*Dispatcher, string) {
	t.Helper()

	d := New([]LoadBalancer{{Address: "127.0.0.1:0", ContentionRatio: 1}}, false)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	if err := d.StartHTTP("127.0.0.1", port); err != nil {
		t.Fatalf("StartHTTP failed: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	return d, d.HTTPAddr()
}

// proxyClient builds an http.Client that routes through the given proxy.
func proxyClient(t *testing.T, proxyAddr string) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		t.Fatalf("bad proxy address: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 20 * time.Second,
	}
}

// TestHTTPProxyForwardsPlainHTTP covers the absolute-form request path that
// browsers use for http:// URLs through a proxy.
func TestHTTPProxyForwardsPlainHTTP(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The origin server must receive an origin-form request line, not
		// the absolute-form the proxy was given.
		if strings.HasPrefix(r.RequestURI, "http://") {
			t.Errorf("origin received absolute-form request %q; it was not rewritten", r.RequestURI)
		}
		if r.Header.Get("Proxy-Connection") != "" {
			t.Error("hop-by-hop Proxy-Connection header was forwarded to the origin")
		}
		w.Write([]byte("hello through proxy"))
	}))
	defer origin.Close()

	d, proxyAddr := startLoopbackProxy(t)
	client := proxyClient(t, proxyAddr)

	resp, err := client.Get(origin.URL + "/path")
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello through proxy" {
		t.Errorf("got body %q, want %q", body, "hello through proxy")
	}

	if got := d.Stats()[0].ConnectionsHandled; got != 1 {
		t.Errorf("dispatcher handled %d connections, want 1", got)
	}
}

// TestHTTPProxyTunnelsHTTPS covers the CONNECT path, which is how all HTTPS
// traffic reaches an HTTP proxy.
func TestHTTPProxyTunnelsHTTPS(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secure hello"))
	}))
	defer origin.Close()

	d, proxyAddr := startLoopbackProxy(t)
	client := proxyClient(t, proxyAddr)

	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("HTTPS request through proxy failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "secure hello" {
		t.Errorf("got body %q, want %q", body, "secure hello")
	}

	if got := d.Stats()[0].ConnectionsHandled; got != 1 {
		t.Errorf("dispatcher handled %d connections, want 1", got)
	}
}

// TestHTTPProxyRejectsDirectRequest checks that a browser pointed straight at
// the proxy port gets a clean error rather than a dropped connection.
func TestHTTPProxyRejectsDirectRequest(t *testing.T) {
	_, proxyAddr := startLoopbackProxy(t)

	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("could not connect to proxy: %v", err)
	}
	defer conn.Close()

	// Origin-form request: valid HTTP, but meaningless to a proxy.
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	response := make([]byte, 32)
	n, err := conn.Read(response)
	if err != nil {
		t.Fatalf("proxy closed without responding: %v", err)
	}
	if !strings.Contains(string(response[:n]), "400") {
		t.Errorf("got response %q, want a 400 status", response[:n])
	}
}

// TestWithDefaultPort covers the address normalisation used for both request
// shapes, including IPv6 literals.
func TestWithDefaultPort(t *testing.T) {
	cases := []struct {
		host, defaultPort, want string
	}{
		{"example.com", "443", "example.com:443"},
		{"example.com:8443", "443", "example.com:8443"},
		{"10.0.0.1", "80", "10.0.0.1:80"},
		{"[::1]:8080", "80", "[::1]:8080"},
		{"::1", "80", "[::1]:80"},
		{"", "80", ""},
	}

	for _, c := range cases {
		if got := withDefaultPort(c.host, c.defaultPort); got != c.want {
			t.Errorf("withDefaultPort(%q, %q) = %q, want %q", c.host, c.defaultPort, got, c.want)
		}
	}
}
