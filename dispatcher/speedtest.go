// speedtest.go
package dispatcher

import (
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultSpeedTestURL is a public, no-auth endpoint made specifically for
// download-speed testing (https://developers.cloudflare.com/speed/).
const DefaultSpeedTestURL = "https://speed.cloudflare.com/__down?bytes=10000000"

// DefaultUploadURL is the matching endpoint that accepts a POST body and
// discards it, used to measure upload throughput.
const DefaultUploadURL = "https://speed.cloudflare.com/__up"

// uploadURL is the endpoint measureUpload posts to. It is a variable so
// tests can point it at a local server.
var uploadURL = DefaultUploadURL

// latencyProbeURL returns immediately with an empty body, so the round trip
// measures connection setup rather than transfer time.
const latencyProbeURL = "https://speed.cloudflare.com/__down?bytes=0"

// DefaultTestDuration bounds how long each throughput direction transfers for.
const DefaultTestDuration = 3 * time.Second

// TestResult holds the outcome of testing one interface's connection.
type TestResult struct {
	IP            string
	InterfaceName string
	LatencyMs     float64
	// DownloadBps and UploadBps are measured independently, one direction
	// after the other, so neither competes with the other for bandwidth.
	DownloadBps float64
	UploadBps   float64
	Error       string
	// UploadError is reported separately: a failed upload leg should not
	// discard an otherwise valid download measurement.
	UploadError string
}

// clientBoundToIP builds an http.Client whose outbound connections are
// bound to the given local IP, mirroring the source-IP binding used when
// actually dispatching traffic in server_response.go.
func clientBoundToIP(ip string) *http.Client {
	parsed := net.ParseIP(ip)
	dialer := &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: parsed},
		Timeout:   5 * time.Second,
	}

	// Pin the dial to the source address's family: binding an IPv6 source
	// while resolving the test host to IPv4 (or the reverse) cannot connect.
	network := networkFor(parsed != nil && parsed.To4() == nil)

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second}
}

// TestInterface measures latency plus download and upload throughput for the
// interface with the given local IP, routing all test traffic strictly over
// it. testURL and duration override the defaults when non-zero/non-empty.
func TestInterface(ctx context.Context, ip string, testURL string, duration time.Duration) TestResult {
	result := measureClient(ctx, clientBoundToIP(ip), testURL, duration)
	result.IP = ip
	return result
}

// measureClient runs the full latency/download/upload sequence over an
// already-configured client, so the same measurement logic serves both
// per-interface tests and end-to-end tests through a running proxy.
func measureClient(ctx context.Context, client *http.Client, testURL string, duration time.Duration) TestResult {
	if testURL == "" {
		testURL = DefaultSpeedTestURL
	}
	if duration <= 0 {
		duration = DefaultTestDuration
	}

	var result TestResult

	latency, err := measureLatency(ctx, client)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.LatencyMs = latency

	down, err := measureDownload(ctx, client, testURL, duration)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.DownloadBps = down

	// Upload runs after download so the two never share bandwidth. Its
	// failure is recorded separately to preserve the download figure.
	up, err := measureUpload(ctx, client, duration)
	if err != nil {
		result.UploadError = err.Error()
		return result
	}
	result.UploadBps = up

	return result
}

// measureLatency times a request that returns an empty body.
func measureLatency(ctx context.Context, client *http.Client) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latencyProbeURL, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return float64(time.Since(start).Microseconds()) / 1000.0, nil
}

// measureDownload reads the response body for up to duration and reports the
// achieved bytes/second.
func measureDownload(ctx context.Context, client *http.Client, testURL string, duration time.Duration) (float64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, duration+10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, testURL, nil)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var total int64
	buf := make([]byte, 32*1024)
	deadline := start.Add(duration)
	for time.Now().Before(deadline) {
		n, rerr := resp.Body.Read(buf)
		total += int64(n)
		if rerr != nil {
			break
		}
	}

	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0, nil
	}
	return float64(total) / elapsed, nil
}

// measureUpload POSTs generated data for up to duration and reports the
// achieved bytes/second. The body length is unknown up front, so the request
// is sent chunked and the reader stops itself at the deadline.
func measureUpload(ctx context.Context, client *http.Client, duration time.Duration) (float64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, duration+10*time.Second)
	defer cancel()

	body := &deadlineReader{deadline: time.Now().Add(duration)}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, uploadURL, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0, nil
	}
	return float64(body.sent) / elapsed, nil
}

// deadlineReader produces filler bytes until its deadline passes, then
// reports EOF. It counts what it handed out so the caller can compute the
// achieved rate.
type deadlineReader struct {
	deadline time.Time
	sent     int64
}

func (r *deadlineReader) Read(p []byte) (int, error) {
	if !time.Now().Before(r.deadline) {
		return 0, io.EOF
	}
	for i := range p {
		p[i] = 'x'
	}
	r.sent += int64(len(p))
	return len(p), nil
}

// TestAllInterfaces runs TestInterface concurrently for every IP so the
// measured throughput figures are directly comparable to each other.
func TestAllInterfaces(ctx context.Context, ips []string, testURL string, duration time.Duration) []TestResult {
	results := make([]TestResult, len(ips))
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			results[i] = TestInterface(ctx, ip, testURL, duration)
		}(i, ip)
	}
	wg.Wait()
	return results
}

// SuggestRatios converts measured download throughput per interface into a
// contention ratio: the slowest working interface gets a ratio of 1, and
// every other interface gets round(its throughput / slowest throughput),
// clamped to [1, 10]. Interfaces that failed to test default to 1.
//
// Download is used rather than upload because it is the direction that
// dominates typical proxied traffic.
func SuggestRatios(results []TestResult) map[string]int {
	minSpeed := math.MaxFloat64
	for _, r := range results {
		if r.Error == "" && r.DownloadBps > 0 && r.DownloadBps < minSpeed {
			minSpeed = r.DownloadBps
		}
	}

	out := make(map[string]int, len(results))
	if minSpeed == math.MaxFloat64 {
		for _, r := range results {
			out[r.IP] = 1
		}
		return out
	}

	for _, r := range results {
		if r.Error != "" || r.DownloadBps <= 0 {
			out[r.IP] = 1
			continue
		}
		ratio := int(math.Round(r.DownloadBps / minSpeed))
		if ratio < 1 {
			ratio = 1
		}
		if ratio > 10 {
			ratio = 10
		}
		out[r.IP] = ratio
	}
	return out
}
