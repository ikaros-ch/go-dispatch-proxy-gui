package dispatcher

import (
	"errors"
	"net"
	"syscall"
	"testing"
)

// TestIsDestinationError separates faults of the requested destination from
// faults of the link we dispatched over.
func TestIsDestinationError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "unresolvable name",
			err:  &net.DNSError{Err: "no such host", Name: "disabled.invalid", IsNotFound: true},
			want: true,
		},
		{
			name: "no usable address for this network",
			err:  &net.AddrError{Err: "no suitable address found", Addr: "ipv6.msftconnecttest.com"},
			want: true,
		},
		{
			name: "wrapped DNS error",
			err:  &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}},
			want: true,
		},
		{
			name: "connection refused is a link or peer fault",
			err:  &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
			want: false,
		},
		{
			name: "network unreachable is a link fault",
			err:  &net.OpError{Op: "dial", Err: syscall.ENETUNREACH},
			want: false,
		},
		{
			name: "unclassified error",
			err:  errors.New("something else"),
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDestinationError(c.err); got != c.want {
				t.Errorf("isDestinationError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestRecordDialFailureLeavesHealthyLinkClean is the behaviour that matters:
// Windows probes hosts that cannot resolve, and those must not show up as an
// error against an interface that is working.
func TestRecordDialFailureLeavesHealthyLinkClean(t *testing.T) {
	d := New([]LoadBalancer{{Address: "10.0.0.1:0", ContentionRatio: 1}}, false)
	lb := &d.lbList[0]

	d.recordDialFailure(lb, 0, "ipv6.msftconnecttest.com:80",
		&net.DNSError{Err: "no such host", Name: "ipv6.msftconnecttest.com", IsNotFound: true})

	if got := d.Stats()[0].LastError; got != "" {
		t.Errorf("a destination that cannot resolve was blamed on the link: LastError = %q", got)
	}

	// A genuine link failure must still be recorded.
	d.recordDialFailure(lb, 0, "example.com:443", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH})

	if got := d.Stats()[0].LastError; got == "" {
		t.Error("a real link failure was not recorded against the load balancer")
	}
}
