package naive

import (
	"context"
	"errors"

	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/internal/authconn"
	"github.com/lenaanderson5566-ops/v2box/internal/landing"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/common/uot"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/transport/v2rayhttp"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	aTLS "github.com/sagernet/sing/common/tls"
	sHttp "github.com/sagernet/sing/protocol/http"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c" //nolint:staticcheck
)

type User struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	Password string `json:"password"`
}
type InboundOptions struct {
	LandingPage conf.LandingPage `json:"landing_page,omitempty"`
	option.ListenOptions
	Users   []User             `json:"users,omitempty"`
	Network option.NetworkList `json:"network,omitempty"`
	option.InboundTLSOptionsContainer
}

var WrapError func(error) error

type transportKey struct{}
type transportState struct {
	mu        sync.Mutex
	conn      net.Conn
	identity  string
	release   func()
	closed    bool
	idleTimer *time.Timer
}

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[InboundOptions](registry, C.TypeNaive, NewInbound)
}

type Inbound struct {
	inbound.Adapter
	ctx         context.Context
	router      adapter.ConnectionRouterEx
	logger      logger.ContextLogger
	listener    *listener.Listener
	network     []string
	connections sync.Map
	mu          sync.RWMutex
	users       map[string]User
	transports  authconn.Registry[string]
	tlsConfig   tls.ServerConfig
	httpServer  *http.Server
	landingPage http.Handler
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options InboundOptions) (adapter.Inbound, error) {
	inbound := &Inbound{
		Adapter: inbound.NewAdapter(C.TypeNaive, tag),
		ctx:     ctx,
		router:  uot.NewRouter(router, logger),
		logger:  logger,
		listener: listener.New(listener.Options{
			Context: ctx,
			Logger:  logger,
			Listen:  options.ListenOptions,
		}),
		network: options.Network.Build(),
	}
	if options.Network != "tcp" {
		return nil, E.New("Naive requires network tcp (HTTPS/HTTP2)")
	}
	if options.TLS == nil || !options.TLS.Enabled {
		return nil, E.New("Naive requires TLS")
	}
	if err := inbound.UpdateUsers(options.Users); err != nil {
		return nil, err
	}
	if options.LandingPage.Enabled {
		handler, err := landing.New(options.LandingPage.Directory)
		if err != nil {
			return nil, err
		}
		inbound.landingPage = handler
	}
	if options.TLS != nil {
		tlsConfig, err := tls.NewServer(ctx, logger, common.PtrValueOrDefault(options.TLS))
		if err != nil {
			return nil, err
		}
		inbound.tlsConfig = tlsConfig
	}
	return inbound, nil
}

func (n *Inbound) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	if n.tlsConfig != nil {
		err := n.tlsConfig.Start()
		if err != nil {
			return E.Cause(err, "create TLS config")
		}
		scope.Add(n.tlsConfig.Close)
	}
	scope.Add(func() error {
		err := n.listener.Close()
		// http.Server also owns the TCP listener after Serve starts.
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	})
	if common.Contains(n.network, N.NetworkTCP) {
		tcpListener, err := n.listener.ListenTCP()
		if err != nil {
			return err
		}
		n.httpServer = &http.Server{
			//nolint:staticcheck
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				state := r.Context().Value(transportKey{}).(*transportState)
				defer n.releaseTransport(state.conn)
				// Own the H2 idle timer. Go 1.27 can rearm its internal timer
				// while closing remaining streams, after its deferred Stop ran,
				// retaining the closed connection and buffers for another 90s.
				h2Base := &http.Server{
					ReadHeaderTimeout: 10 * time.Second,
					ConnState: func(_ net.Conn, status http.ConnState) {
						switch status {
						case http.StateIdle:
							state.setIdle(true, 90*time.Second)
						case http.StateActive, http.StateClosed:
							state.setIdle(false, 0)
						}
					},
				}
				r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, h2Base))
				h2c.NewHandler(n, &http2.Server{}).ServeHTTP(w, r)
			}),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       90 * time.Second,
			ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
				state := &transportState{conn: conn}
				n.connections.Store(conn, state)
				return context.WithValue(ctx, transportKey{}, state)
			},
			ConnState: func(conn net.Conn, status http.ConnState) {
				if status == http.StateClosed {
					n.releaseTransport(conn)
				}
			},
			BaseContext: func(listener net.Listener) context.Context {
				return n.ctx
			},
		}
		listener := net.Listener(tcpListener)
		if n.tlsConfig != nil {
			if len(n.tlsConfig.NextProtos()) == 0 {
				n.tlsConfig.SetNextProtos([]string{http2.NextProtoTLS, "http/1.1"})
			} else if !common.Contains(n.tlsConfig.NextProtos(), http2.NextProtoTLS) {
				n.tlsConfig.SetNextProtos(append([]string{http2.NextProtoTLS}, n.tlsConfig.NextProtos()...))
			}
			listener = aTLS.NewListener(tcpListener, n.tlsConfig)
		}
		go func() {
			sErr := n.httpServer.Serve(listener)
			if sErr != nil && !errors.Is(sErr, http.ErrServerClosed) {
				n.logger.Error("http server serve error: ", sErr)
			}
		}()
		scope.Add(func() error {
			err := n.httpServer.Close()
			n.connections.Range(func(key, value any) bool { _ = key.(net.Conn).Close(); n.releaseTransport(key.(net.Conn)); return true })
			return err
		})
	}

	return nil
}

func (n *Inbound) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if n.landingPage != nil && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		// The outer handler owns one TLS transport. H1 browsing closes after
		// this response; H2 retains its normal multiplexed connection lifecycle.
		if request.ProtoMajor == 1 {
			writer.Header().Set("Connection", "close")
		}
		n.landingPage.ServeHTTP(writer, request)
		return
	}
	ctx := log.ContextWithNewID(request.Context())
	if request.ProtoMajor != 2 || request.Method != "CONNECT" {
		rejectHTTP(writer, http.StatusBadRequest)
		n.badRequest(ctx, request, E.New("not CONNECT request"))
		return
	} else if request.Header.Get("Padding") == "" {
		rejectHTTP(writer, http.StatusBadRequest)
		n.badRequest(ctx, request, E.New("missing naive padding"))
		return
	}
	userName, password, authOk := sHttp.ParseBasicAuth(request.Header.Get("Proxy-Authorization"))
	if authOk {
		userName, authOk = n.authenticate(request, userName, password)
	}
	if !authOk {
		rejectHTTP(writer, http.StatusProxyAuthRequired)
		n.badRequest(ctx, request, E.New("authorization failed"))
		return
	}
	writer.Header().Set("Padding", generatePaddingHeader())
	writer.WriteHeader(http.StatusOK)
	writer.(http.Flusher).Flush()

	hostPort := request.Header.Get("-connect-authority")
	if hostPort == "" {
		hostPort = request.URL.Host
		if hostPort == "" {
			hostPort = request.Host
		}
	}
	source := M.ParseSocksaddr(request.RemoteAddr)
	destination := M.ParseSocksaddr(hostPort).Unwrap()

	n.newConnection(ctx, &naiveH2Conn{
		reader: request.Body, writer: writer, flusher: writer.(http.Flusher), remoteAddress: source,
	}, userName, source, destination)
}

func (n *Inbound) newConnection(ctx context.Context, conn net.Conn, userName string, source M.Socksaddr, destination M.Socksaddr) {
	var metadata adapter.InboundContext
	metadata.Inbound = n.Tag()
	metadata.InboundType = n.Type()
	//nolint:staticcheck
	metadata.InboundDetour = n.listener.ListenOptions().Detour
	//nolint:staticcheck
	metadata.Source = source
	metadata.Destination = destination
	metadata.OriginDestination = M.SocksaddrFromNet(conn.LocalAddr()).Unwrap()
	metadata.User = userName
	done := make(chan struct{})
	wrapper := v2rayhttp.NewHTTP2Wrapper(conn)
	n.router.RouteConnectionEx(ctx, wrapper, metadata, N.OnceClose(func(it error) { close(done) }))
	<-done
	wrapper.CloseWrapper()
}

func (n *Inbound) badRequest(ctx context.Context, request *http.Request, err error) {
	n.logger.ErrorContext(ctx, E.Cause(err, "process connection from ", request.RemoteAddr))
}

func rejectHTTP(writer http.ResponseWriter, statusCode int) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		writer.WriteHeader(statusCode)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		writer.WriteHeader(statusCode)
		return
	}
	if tcpConn, isTCP := common.Cast[*net.TCPConn](conn); isTCP {
		tcpConn.SetLinger(0)
	}
	conn.Close()
}
