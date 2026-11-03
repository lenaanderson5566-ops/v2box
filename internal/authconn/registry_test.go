package authconn

import (
	"net"
	"testing"
	"time"
)

func TestRevocationPreservesOtherTransports(t *testing.T) {
	var r Registry[string]
	r.Replace([]string{"a", "b"})
	a, pa := net.Pipe()
	defer pa.Close()
	b, pb := net.Pipe()
	defer pb.Close()
	ca, err := r.Wrap("a", a)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	cb, err := r.Wrap("b", b)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	r.Replace([]string{"a"})
	_ = pb.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = pb.Read(make([]byte, 1)); err == nil {
		t.Fatal("revoked transport remains open")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("transport was not closed")
	}
	done := make(chan error, 1)
	go func() { _, e := ca.Write([]byte{1}); done <- e }()
	_ = pa.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = pa.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	c, pc := net.Pipe()
	defer pc.Close()
	if _, err = r.Wrap("b", c); err == nil {
		t.Fatal("revoked in-flight handshake admitted")
	}
	ca.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessions) != 0 {
		t.Fatal("closed sessions retained")
	}
}
