package dispatcher

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

func linkError() error {
	return &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
}

// TestAutoExcludeAfterRepeatedFailures covers the main promise: a link that
// keeps failing stops receiving traffic.
func TestAutoExcludeAfterRepeatedFailures(t *testing.T) {
	d := New([]LoadBalancer{
		{Address: "10.0.0.1:0", ContentionRatio: 1},
		{Address: "10.0.0.2:0", ContentionRatio: 1},
	}, false)
	d.SetAutoExclude(true)

	bad := &d.lbList[0]

	// Below the threshold the link stays in rotation.
	for i := 0; i < defaultFailureThreshold-1; i++ {
		d.noteFailure(bad, "boom")
	}
	if d.Health()[0].Excluded {
		t.Fatal("link was excluded before reaching the failure threshold")
	}

	d.noteFailure(bad, "boom")
	if !d.Health()[0].Excluded {
		t.Fatal("link was not excluded after reaching the failure threshold")
	}

	// Dispatching must now avoid it entirely.
	for i := 0; i < 6; i++ {
		lb, _, err := d.getLoadBalancerFor(true, false)
		if err != nil {
			t.Fatalf("dispatch failed while one link was excluded: %v", err)
		}
		if lb.Address == "10.0.0.1:0" {
			t.Fatal("an excluded link was still selected")
		}
	}
}

// TestSuccessResetsFailureStreak is what stops one dead destination from
// condemning a link that is otherwise fine.
func TestSuccessResetsFailureStreak(t *testing.T) {
	d := New([]LoadBalancer{{Address: "10.0.0.1:0", ContentionRatio: 1}}, false)
	d.SetAutoExclude(true)

	lb := &d.lbList[0]

	for i := 0; i < defaultFailureThreshold-1; i++ {
		d.noteFailure(lb, "boom")
	}
	d.recordDialSuccess(lb)

	if got := d.Health()[0].ConsecutiveFailures; got != 0 {
		t.Errorf("failure streak is %d after a success, want 0", got)
	}

	// The streak restarts, so one more failure must not exclude it.
	d.noteFailure(lb, "boom")
	if d.Health()[0].Excluded {
		t.Error("link was excluded even though a success had reset the streak")
	}
}

// TestAutoExcludeDisabledKeepsLinkInUse checks the "notify only" behaviour.
func TestAutoExcludeDisabledKeepsLinkInUse(t *testing.T) {
	d := New([]LoadBalancer{{Address: "10.0.0.1:0", ContentionRatio: 1}}, false)
	// Auto exclude deliberately left off.

	lb := &d.lbList[0]
	for i := 0; i < defaultFailureThreshold+2; i++ {
		d.noteFailure(lb, "boom")
	}

	if d.Health()[0].Excluded {
		t.Error("link was excluded even though automatic exclusion is off")
	}
	if got := d.Health()[0].ConsecutiveFailures; got == 0 {
		t.Error("failures were not counted while automatic exclusion is off")
	}
}

// TestExclusionEmitsEvents covers the notifications the UI and toasts use.
func TestExclusionEmitsEvents(t *testing.T) {
	d := New([]LoadBalancer{{Address: "10.0.0.1:0", ContentionRatio: 1}}, false)
	d.SetAutoExclude(true)

	events := make(chan HealthEvent, 8)
	d.OnHealthChange(func(e HealthEvent) { events <- e })

	lb := &d.lbList[0]
	for i := 0; i < defaultFailureThreshold; i++ {
		d.noteFailure(lb, "connection timed out")
	}

	select {
	case e := <-events:
		if !e.Excluded || e.Address != "10.0.0.1:0" {
			t.Errorf("unexpected exclusion event: %+v", e)
		}
		if e.Reason == "" {
			t.Error("exclusion event carried no reason")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event was emitted when the link was excluded")
	}

	// Restoring must be reported too.
	if err := d.SetExcluded("10.0.0.1:0", false); err != nil {
		t.Fatalf("SetExcluded failed: %v", err)
	}
	select {
	case e := <-events:
		if e.Excluded {
			t.Errorf("expected a restore event, got %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event was emitted when the link was restored")
	}
}

// TestAllExcludedStillDispatches checks the fallback: when every usable link
// is excluded, traffic is attempted anyway rather than refused outright.
func TestAllExcludedStillDispatches(t *testing.T) {
	d := New([]LoadBalancer{
		{Address: "10.0.0.1:0", ContentionRatio: 1},
		{Address: "10.0.0.2:0", ContentionRatio: 1},
	}, false)
	d.SetAutoExclude(true)

	for i := range d.lbList {
		lb := &d.lbList[i]
		for f := 0; f < defaultFailureThreshold; f++ {
			d.noteFailure(lb, "boom")
		}
	}

	for i := range d.Health() {
		if !d.Health()[i].Excluded {
			t.Fatalf("link %d was not excluded", i)
		}
	}

	lb, _, err := d.getLoadBalancerFor(true, false)
	if err != nil {
		t.Fatalf("dispatch refused when every link was excluded: %v", err)
	}
	if lb == nil {
		t.Fatal("no load balancer returned")
	}
}

// TestSetAutoExcludeOffRestoresLinks checks that turning the option off puts
// excluded links back to work immediately.
func TestSetAutoExcludeOffRestoresLinks(t *testing.T) {
	d := New([]LoadBalancer{{Address: "10.0.0.1:0", ContentionRatio: 1}}, false)
	d.SetAutoExclude(true)

	lb := &d.lbList[0]
	for i := 0; i < defaultFailureThreshold; i++ {
		d.noteFailure(lb, "boom")
	}
	if !d.Health()[0].Excluded {
		t.Fatal("link was not excluded")
	}

	d.SetAutoExclude(false)
	if d.Health()[0].Excluded {
		t.Error("link stayed excluded after automatic exclusion was turned off")
	}
}

// TestSetExcludedUnknownAddress checks the error path for a stale UI request.
func TestSetExcludedUnknownAddress(t *testing.T) {
	d := New([]LoadBalancer{{Address: "10.0.0.1:0", ContentionRatio: 1}}, false)

	if err := d.SetExcluded("10.9.9.9:0", true); !errors.Is(err, errUnknownLoadBalancer) {
		t.Errorf("got %v, want errUnknownLoadBalancer", err)
	}
}

// TestHealthProbeRestoresRecoveredLink drives the recovery path against a
// real listener standing in for the probe target.
func TestHealthProbeRestoresRecoveredLink(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not start probe target: %v", err)
	}
	defer probe.Close()
	go func() {
		for {
			conn, err := probe.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	d := New([]LoadBalancer{{Address: "127.0.0.1:0", ContentionRatio: 1}}, false)
	d.SetAutoExclude(true)
	d.probeTargetOverride = probe.Addr().String()

	lb := &d.lbList[0]
	for i := 0; i < defaultFailureThreshold; i++ {
		d.noteFailure(lb, "boom")
	}
	if !d.Health()[0].Excluded {
		t.Fatal("link was not excluded")
	}

	d.probeExcluded(context.Background())

	if d.Health()[0].Excluded {
		t.Error("link was not restored even though the probe succeeded")
	}
}

// TestHealthProbeKeepsFailingLinkExcluded is the other half: a link that is
// still broken must stay out of rotation.
func TestHealthProbeKeepsFailingLinkExcluded(t *testing.T) {
	// Port 1 on loopback has nothing listening, so the probe fails.
	d := New([]LoadBalancer{{Address: "127.0.0.1:0", ContentionRatio: 1}}, false)
	d.SetAutoExclude(true)
	d.probeTargetOverride = "127.0.0.1:1"

	lb := &d.lbList[0]
	for i := 0; i < defaultFailureThreshold; i++ {
		d.noteFailure(lb, "boom")
	}

	d.probeExcluded(context.Background())

	if !d.Health()[0].Excluded {
		t.Error("a link that is still failing was restored")
	}
}

// TestExclusionSurvivesReconfiguration checks that auto mode replacing the
// load balancer set does not silently un-exclude a failing link.
func TestExclusionSurvivesReconfiguration(t *testing.T) {
	d := New([]LoadBalancer{
		{Address: "10.0.0.1:0", ContentionRatio: 1},
		{Address: "10.0.0.2:0", ContentionRatio: 1},
	}, false)
	d.SetAutoExclude(true)

	lb := &d.lbList[0]
	for i := 0; i < defaultFailureThreshold; i++ {
		d.noteFailure(lb, "boom")
	}

	err := d.UpdateLoadBalancers([]LoadBalancer{
		{Address: "10.0.0.1:0", ContentionRatio: 2},
		{Address: "10.0.0.2:0", ContentionRatio: 1},
	})
	if err != nil {
		t.Fatalf("UpdateLoadBalancers failed: %v", err)
	}

	if !d.Health()[0].Excluded {
		t.Error("exclusion was lost when the configuration was updated")
	}
}
