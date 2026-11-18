package naive

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestIdleTransportTimeoutAndRelease(t *testing.T) {
	for _, action := range []string{"timeout", "active", "released"} {
		t.Run(action, func(t *testing.T) {
			conn, peer := net.Pipe()
			defer conn.Close()
			defer peer.Close()
			state := &transportState{conn: conn}
			n := &Inbound{}
			n.connections.Store(conn, state)
			defer n.releaseTransport(conn)
			state.setIdle(true, 30*time.Millisecond)
			switch action {
			case "active":
				state.setIdle(false, 0)
			case "released":
				n.releaseTransport(conn)
				state.setIdle(true, time.Millisecond) // Closed states cannot rearm.
			}
			peer.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			_, err := peer.Read(make([]byte, 1))
			if action == "timeout" {
				if err != io.EOF {
					t.Fatalf("idle transport not closed: %v", err)
				}
			} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
				t.Fatalf("stopped timer closed transport: %v", err)
			}
		})
	}
}
