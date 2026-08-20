package cluster

import (
	"net"
	"strconv"
	"sync"
	"time"
)

// APIPort is the TCP port apid listens on. Every talosctl call connects to an
// endpoint on this port.
const APIPort = 50000

// DefaultProbeTimeout bounds one reachability probe. Short enough to fail fast
// on an unroutable address, long enough not to produce false negatives on a
// slow tunnel.
const DefaultProbeTimeout = 2 * time.Second

// Probe reports whether addr answers on the Talos API port.
//
// This exists because the failure it catches is otherwise unreadable. A Talos
// endpoint that is resolvable but not routable — the usual shape when a VPN or
// tunnel is down — surfaces from talosctl as
//
//	rpc error: code = Unavailable desc = connection error: desc =
//	"transport: Error while dialing: dial tcp 10.0.0.1:50000: i/o timeout"
//
// which reads as a broken cluster rather than as a broken network path from
// this machine. A dial on the same port says the same thing in one line, and
// says it before anything has been written.
func Probe(addr string, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr, strconv.Itoa(APIPort)), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Reachable returns the subset of addrs that answer on the Talos API port,
// probing them in parallel. Input order is preserved so the first reachable
// address stays the first choice.
func Reachable(addrs []string, timeout time.Duration) []string {
	if len(addrs) == 0 {
		return nil
	}
	ok := make([]bool, len(addrs))
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		go func(i int, a string) {
			defer wg.Done()
			ok[i] = Probe(a, timeout)
		}(i, a)
	}
	wg.Wait()

	out := make([]string, 0, len(addrs))
	for i, a := range addrs {
		if ok[i] {
			out = append(out, a)
		}
	}
	return out
}
