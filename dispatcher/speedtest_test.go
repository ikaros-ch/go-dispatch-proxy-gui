package dispatcher

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestMeasureUpload checks that the upload leg actually transfers data for
// roughly the requested duration and reports a plausible rate.
func TestMeasureUpload(t *testing.T) {
	var received int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received = n
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Point the upload at the test server for the duration of this test.
	original := uploadURL
	uploadURL = server.URL
	defer func() { uploadURL = original }()

	duration := 300 * time.Millisecond
	start := time.Now()
	rate, err := measureUpload(context.Background(), server.Client(), duration)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("measureUpload failed: %v", err)
	}
	if received == 0 {
		t.Fatal("server received no data")
	}
	if rate <= 0 {
		t.Errorf("rate was %v, want a positive value", rate)
	}
	// The reader stops itself at the deadline, so the request must not run
	// far beyond it.
	if elapsed > duration+5*time.Second {
		t.Errorf("upload took %v, far longer than the %v budget", elapsed, duration)
	}
}

// TestMeasureDownload checks the download leg stops at its deadline rather
// than reading an endless body to completion.
func TestMeasureDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 32*1024)
		for {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	duration := 300 * time.Millisecond
	start := time.Now()
	rate, err := measureDownload(context.Background(), server.Client(), server.URL, duration)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("measureDownload failed: %v", err)
	}
	if rate <= 0 {
		t.Errorf("rate was %v, want a positive value", rate)
	}
	if elapsed > duration+5*time.Second {
		t.Errorf("download took %v, far longer than the %v budget", elapsed, duration)
	}
}

// TestUpdateLoadBalancersPreservesCounters confirms a live reconfiguration
// carries stats across for surviving addresses and drops removed ones.
func TestUpdateLoadBalancersPreservesCounters(t *testing.T) {
	d := New([]LoadBalancer{
		{Address: "10.0.0.1:0", ContentionRatio: 1},
		{Address: "10.0.0.2:0", ContentionRatio: 1},
	}, false)

	d.lbList[0].BytesSent = 4096
	d.lbList[0].ConnectionsHandled = 7
	d.lbList[1].BytesReceived = 512

	// 10.0.0.2 disappears, 10.0.0.3 appears, 10.0.0.1 stays with a new ratio.
	err := d.UpdateLoadBalancers([]LoadBalancer{
		{Address: "10.0.0.1:0", ContentionRatio: 5},
		{Address: "10.0.0.3:0", ContentionRatio: 2},
	})
	if err != nil {
		t.Fatalf("UpdateLoadBalancers failed: %v", err)
	}

	stats := d.Stats()
	if len(stats) != 2 {
		t.Fatalf("got %d load balancers, want 2", len(stats))
	}

	if stats[0].Address != "10.0.0.1:0" {
		t.Fatalf("first address is %q, want 10.0.0.1:0", stats[0].Address)
	}
	if stats[0].BytesSent != 4096 || stats[0].ConnectionsHandled != 7 {
		t.Errorf("surviving load balancer lost its counters: %+v", stats[0])
	}
	if stats[0].ContentionRatio != 5 {
		t.Errorf("ratio is %d, want the updated value 5", stats[0].ContentionRatio)
	}
	if stats[1].BytesSent != 0 || stats[1].BytesReceived != 0 {
		t.Errorf("new load balancer should start at zero: %+v", stats[1])
	}

	if err := d.UpdateLoadBalancers(nil); err == nil {
		t.Error("expected an error when updating to an empty set")
	}
}
