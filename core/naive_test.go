package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/limiter"
)

// A small wire client exercises the real HTTPS/HTTP2 CONNECT and Naive padding
// without a platform-specific Chromium binary. Never used in production.
func naiveClient(t *testing.T, n *panel.NodeInfo) *http.ClientConn {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(n.Common.CertInfo.TlsCert))
	// Go 1.27's x/net/http2 compatibility ClientConn can reenter its own mutex
	// through the Reserve state hook under concurrent requests. Use the native
	// API so that client-side wrapper deadlocks do not mask server soak results.
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	// Native manually managed ClientConns otherwise retain a closed connection
	// for up to five seconds in the transport's unused-connection error cache.
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost"}, Protocols: protocols, IdleConnTimeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := transport.NewClientConn(ctx, "https", net.JoinHostPort("127.0.0.1", strconv.Itoa(n.Common.ServerPort)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

type naiveStream struct {
	reader             io.ReadCloser
	writer             *io.PipeWriter
	cancel             context.CancelFunc
	reads, writes      int
	remaining, padding int
}

func naiveConnect(client *http.ClientConn, uuid string, destination string) (*naiveStream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	reader, writer := io.Pipe()
	request := &http.Request{Method: "CONNECT", URL: &url.URL{Scheme: "https", Host: destination}, Host: destination, Header: make(http.Header), Body: reader}
	request = request.WithContext(ctx)
	request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(uuid+":"+uuid)))
	request.Header.Set("Padding", "test-padding")
	request.Header.Set("X-Forwarded-For", "192.0.2.99")
	response, err := client.RoundTrip(request)
	if err == nil && response.StatusCode != http.StatusOK {
		response.Body.Close()
		err = fmt.Errorf("CONNECT status %d", response.StatusCode)
	}
	if err != nil {
		cancel()
		writer.Close()
		reader.Close()
		return nil, err
	}
	return &naiveStream{reader: response.Body, writer: writer, cancel: cancel}, nil
}
func (s *naiveStream) Close() error { s.cancel(); s.writer.Close(); return s.reader.Close() }
func (s *naiveStream) Write(payload []byte) (int, error) {
	total := 0
	for len(payload) > 0 {
		size := len(payload)
		if size > 32768 {
			size = 32768
		}
		frame := payload[:size]
		if s.writes < 8 {
			header := make([]byte, 3)
			binary.BigEndian.PutUint16(header, uint16(size))
			header[2] = 5
			frame = append(header, frame...)
			frame = append(frame, make([]byte, 5)...)
		}
		if _, err := s.writer.Write(frame); err != nil {
			return total, err
		}
		s.writes++
		total += size
		payload = payload[size:]
	}
	return total, nil
}
func (s *naiveStream) Read(p []byte) (int, error) {
	if s.remaining > 0 {
		if len(p) > s.remaining {
			p = p[:s.remaining]
		}
		count, err := s.reader.Read(p)
		s.remaining -= count
		return count, err
	}
	if s.padding > 0 {
		if _, err := io.CopyN(io.Discard, s.reader, int64(s.padding)); err != nil {
			return 0, err
		}
		s.padding = 0
	}
	if s.reads >= 8 {
		return s.reader.Read(p)
	}
	header := make([]byte, 3)
	if _, err := io.ReadFull(s.reader, header); err != nil {
		return 0, err
	}
	s.reads++
	s.remaining = int(binary.BigEndian.Uint16(header))
	s.padding = int(header[2])
	return s.Read(p)
}
func naiveEcho(stream *naiveStream, payload []byte) error {
	done := make(chan error, 1)
	go func() { _, err := stream.Write(payload); done <- err }()
	result := make([]byte, len(payload))
	_, err := io.ReadFull(stream, result)
	if err != nil {
		stream.Close()
		return err
	}
	if err = <-done; err != nil {
		return err
	}
	if !bytes.Equal(payload, result) {
		return fmt.Errorf("echo mismatch")
	}
	return nil
}

func TestNaiveHotUsersAndAccounting(t *testing.T) {
	limiter.Init()
	n := testNode(t, "naive")
	n.Security = panel.Tls
	n.Common.CertInfo = testCertificate(t)
	v := startTestNode(t, n)
	server := v.nodes[n.Tag].server
	echo := echoServer(t)
	a := panel.UserInfo{Id: 42, Uuid: testUUID}
	b := panel.UserInfo{Id: 7, Uuid: "b8389d79-d204-4f90-b8f0-cb4878a14933"}
	l, _ := limiter.GetLimiter(n.Tag)
	apply := func(users []panel.UserInfo) {
		t.Helper()
		if err := v.SetUsersWithCommit(n.Tag, users, func() { l.UpdateUser(n.Tag, users, nil, nil) }); err != nil {
			t.Fatal(err)
		}
		if v.nodes[n.Tag].server != server {
			t.Fatal("listener replaced")
		}
	}
	connect := func(client *http.ClientConn, uuid string) *naiveStream {
		t.Helper()
		s, err := naiveConnect(client, uuid, echo.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	check := func(s *naiveStream) {
		t.Helper()
		if err := naiveEcho(s, []byte("persistent-naive-stream")); err != nil {
			t.Fatal(err)
		}
	}
	denied := func(client *http.ClientConn, uuid string) {
		t.Helper()
		s, err := naiveConnect(client, uuid, echo.Addr().String())
		if err == nil {
			defer s.Close()
			if naiveEcho(s, []byte("denied")) == nil {
				t.Fatal("revoked credentials forwarded")
			}
		}
	}
	ca := naiveClient(t, n)
	sa := connect(ca, a.Uuid)
	payload := bytes.Repeat([]byte("naive-accounting"), 20000)
	if err := naiveEcho(sa, payload); err != nil {
		t.Fatal(err)
	}
	traffic, _ := v.GetUserTrafficSlice(n.Tag, 0)
	if len(traffic) != 1 || traffic[0].UID != a.Id || traffic[0].Upload != int64(len(payload)) || traffic[0].Download != int64(len(payload)) {
		t.Fatalf("incorrect Naive traffic: %+v", traffic)
	}
	devices, _ := l.GetOnlineDevice()
	if len(*devices) != 1 || (*devices)[0].UID != a.Id || (*devices)[0].IP != "127.0.0.1" {
		t.Fatalf("Naive trusted a forwarded IP or lost account identity: %+v", devices)
	}
	apply([]panel.UserInfo{a, b})
	check(sa)
	cb := naiveClient(t, n)
	sb := connect(cb, b.Uuid)
	check(sb)
	if err := v.SetUsers(n.Tag, []panel.UserInfo{a, {Id: 0, Uuid: b.Uuid}}); err == nil {
		t.Fatal("invalid update accepted")
	}
	check(sb)
	apply([]panel.UserInfo{a})
	if naiveEcho(sb, []byte("revoked")) == nil {
		t.Fatal("revoked stream remained open")
	}
	check(sa)
	denied(naiveClient(t, n), b.Uuid)
	apply([]panel.UserInfo{a, b})
	denied(cb, b.Uuid)
	fresh := connect(naiveClient(t, n), b.Uuid)
	check(fresh)
	b.Uuid = "c8389d79-d204-4f90-b8f0-cb4878a14933"
	apply([]panel.UserInfo{a, b})
	if naiveEcho(fresh, []byte("rotated")) == nil {
		t.Fatal("old UUID remained active")
	}
	idle := naiveClient(t, n)
	idleStream := connect(idle, b.Uuid)
	check(idleStream)
	idleStream.Close()
	apply([]panel.UserInfo{a})
	apply([]panel.UserInfo{a, b})
	denied(idle, b.Uuid)
	var wg sync.WaitGroup
	errors := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			s, err := naiveConnect(ca, a.Uuid, echo.Addr().String())
			if err == nil {
				err = naiveEcho(s, []byte("concurrent"))
				s.Close()
			}
			if err != nil {
				errors <- err
				return
			}
		}
	}()
	for i := 0; i < 8; i++ {
		apply([]panel.UserInfo{a})
		apply([]panel.UserInfo{a, b})
	}
	wg.Wait()
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
	check(sa)
	apply(nil)
	if naiveEcho(sa, []byte("empty")) == nil {
		t.Fatal("empty table accepted user")
	}
	denied(naiveClient(t, n), a.Uuid)
	apply([]panel.UserInfo{a})
	check(connect(naiveClient(t, n), a.Uuid))
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	denied(ca, a.Uuid)
}

func TestNaiveRequiresTLSAndPlainTCP(t *testing.T) {
	n := testNode(t, "naive")
	if _, err := buildInbound(n, nil); err == nil {
		t.Fatal("plaintext Naive accepted")
	}
	n.Security = panel.Reality
	if _, err := buildInbound(n, nil); err == nil {
		t.Fatal("Reality accepted")
	}
	n.Security = panel.Tls
	n.Common.CertInfo = testCertificate(t)
	for _, transport := range []string{"ws", "grpc", "httpupgrade", "udp"} {
		n.Common.Network = transport
		if _, err := buildInbound(n, nil); err == nil {
			t.Fatalf("accepted %s", transport)
		}
	}
	n.Common.Network = "tcp"
	if _, err := buildInbound(n, nil); err != nil {
		t.Fatal(err)
	}
}
