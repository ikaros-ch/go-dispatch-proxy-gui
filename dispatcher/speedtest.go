// speedtest.go
package dispatcher

import (
	"context"
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultSpeedTestURL is a public, no-auth endpoint made specifically for
// download-speed testing (https://developers.cloudflare.com/speed/).
const DefaultSpeedTestURL = "https://speed.cloudflare.com/__down?bytes=10000000"

// DefaultTestDuration bounds how long the throughput test downloads for.
const DefaultTestDuration = 3 * time.Second

// TestResult holds the outcome of testing one interface's connection.
type TestResult struct {
	IP            string
	InterfaceName string
	LatencyMs     float64
	ThroughputBps float64
	Error         string
}

// clientBoundToIP builds an http.Client whose outbound connections are
// bound to the given local IP, mirroring the source-IP binding used when
// actually dispatching traffic in server_response.go.
func clientBoundToIP(ip string) *http.Client {
	dialer := &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)},
		Timeout:   5 * time.Second,
	}
	transport := &http.Transport{
		DialContext: dialer.DialContext,
	}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second}
}

// TestInterface measures latency and download throughput for the interface
// with the given local IP, routing all test traffic strictly over it.
// testURL and duration override the defaults when non-zero/non-empty.
func TestInterface(ctx context.Context, ip string, testURL string, duration time.Duration) TestResult {
	if testURL == "" {
		testURL = DefaultSpeedTestURL
	}
	if duration <= 0 {
		duration = DefaultTestDuration
	}

	result := TestResult{IP: ip}
	client := clientBoundToIP(ip)

	latencyStart := time.Now()
	headReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://speed.cloudflare.com/__down?bytes=0", nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	headResp, err := client.Do(headReq)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	headResp.Body.Close()
	result.LatencyMs = float64(time.Since(latencyStart).Microseconds()) / 1000.0

	dlCtx, cancel := context.WithTimeout(ctx, duration+10*time.Second)
	defer cancel()

	dlReq, err := http.NewRequestWithContext(dlCtx, http.MethodGet, testURL, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	start := time.Now()
	dlResp, err := client.Do(dlReq)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer dlResp.Body.Close()

	var total int64
	buf := make([]byte, 32*1024)
	deadline := start.Add(duration)
	for time.Now().Before(deadline) {
		n, rerr := dlResp.Body.Read(buf)
		total += int64(n)
		if rerr != nil {
			break
		}
	}

	elapsed := time.Since(start).Seconds()
	if elapsed > 0 {
		result.ThroughputBps = float64(total) / elapsed
	}
	return result
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

// SuggestRatios converts measured throughput per interface into a
// contention ratio: the slowest working interface gets a ratio of 1, and
// every other interface gets round(its throughput / slowest throughput),
// clamped to [1, 10]. Interfaces that failed to test default to 1.
func SuggestRatios(results []TestResult) map[string]int {
	minSpeed := math.MaxFloat64
	for _, r := range results {
		if r.Error == "" && r.ThroughputBps > 0 && r.ThroughputBps < minSpeed {
			minSpeed = r.ThroughputBps
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
		if r.Error != "" || r.ThroughputBps <= 0 {
			out[r.IP] = 1
			continue
		}
		ratio := int(math.Round(r.ThroughputBps / minSpeed))
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
