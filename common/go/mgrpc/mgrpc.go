package mgrpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"cesanta.com/common/go/mgrpc/codec"
	"cesanta.com/common/go/mgrpc/frame"
	"github.com/cesanta/errors"
	"github.com/golang/glog"
)

type MgRPC interface {
	Call(
		ctx context.Context, dst string, cmd *frame.Command,
	) (*frame.Response, error)
	Disconnect(ctx context.Context) error
}

type mgRPCImpl struct {
	codec codec.Codec

	// Map of outgoing requests, and its lock
	reqs     map[int64]req
	reqsLock sync.Mutex

	opts *connectOptions
}

type req struct {
	respChan chan *frame.Response
	errChan  chan error
}

const tcpKeepAliveInterval = 3 * time.Minute

// ErrorResponse is an error type for failed commands. Intended for use by
// wrappers around Call() method, like ones generated by clubbygen.
type ErrorResponse struct {
	// Status is the numerical status code.
	Status int
	// Msg is a human-readable description of the error.
	Msg string
}

func (e ErrorResponse) Error() string {
	return fmt.Sprintf("(%d) %s", e.Status, e.Msg)
}

func New(ctx context.Context, connectAddr string, opts ...ConnectOption) (MgRPC, error) {

	opts = append(opts, connectTo(connectAddr))

	rpc := mgRPCImpl{
		reqs: make(map[int64]req),
	}
	if err := rpc.connect(ctx, opts...); err != nil {
		return nil, errors.Trace(err)
	}

	go rpc.recvLoop(ctx, rpc.codec)

	return &rpc, nil
}

// wsDialConfig does the same thing as websocket.DialConfig, but also enables
// TCP keep-alive.
func wsDialConfig(config *websocket.Config) (*websocket.Conn, error) {
	host, port, err := net.SplitHostPort(config.Location.Host)
	if err != nil {
		// Assuming that no port specified.
		host = config.Location.Host
		port = ""
	}

	switch config.Location.Scheme {
	case "ws":
		if port == "" {
			port = "80"
		}
	case "wss":
		if port == "" {
			port = "443"
		}
	default:
		return nil, errors.Trace(websocket.ErrBadScheme)
	}
	addr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, errors.Annotate(err, "net.ResolveTCPAddr")
	}
	tc, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		return nil, errors.Annotate(err, "net.DialTCP")
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(tcpKeepAliveInterval)
	var nc net.Conn = tc

	if config.Location.Scheme == "wss" {
		nc = tls.Client(nc, config.TlsConfig)
	}

	conn, err := websocket.NewClient(config, nc)
	return conn, errors.Trace(err)
}

func (r *mgRPCImpl) wsConnect(url string, opts *connectOptions) (codec.Codec, error) {
	// TODO(imax): figure out what we should use as origin and what to check on the server side.
	const origin = "https://api.cesanta.com/"
	config, err := websocket.NewConfig(url, origin)
	if err != nil {
		return nil, errors.Trace(err)
	}
	encodings := []string{"json"}
	if opts.enableUBJSON {
		encodings = append([]string{"ubjson"}, encodings...)
	}
	s := strings.Join(encodings, "|")
	config.Protocol = []string{codec.WSProtocol}
	config.OutboundExtensions = []string{fmt.Sprintf("%s; in=%s; out=%s", codec.WSEncodingExtension, s, s)}
	config.TlsConfig = &tls.Config{
		RootCAs:    opts.caPool,
		ServerName: opts.serverHost,
	}
	if opts.cert != nil {
		config.TlsConfig.Certificates = []tls.Certificate{*opts.cert}
	}

	conn, err := wsDialConfig(config)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return codec.WebSocket(conn), nil
}

func (r *mgRPCImpl) tcpConnect(tcpAddress string, opts *connectOptions) (codec.Codec, error) {
	// TODO(imax): add TLS support.
	conn, err := net.Dial("tcp", tcpAddress)
	if err != nil {
		return nil, errors.Trace(err)
	}
	conn.(*net.TCPConn).SetKeepAlive(true)
	conn.(*net.TCPConn).SetKeepAlivePeriod(tcpKeepAliveInterval)
	return codec.TCP(conn), nil
}
func (r *mgRPCImpl) serialConnect(
	ctx context.Context, portName string, opts *connectOptions,
) (codec.Codec, error) {
	sc, err := codec.Serial(ctx, portName, opts.junkHandler)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return sc, nil
}

func (r *mgRPCImpl) connect(ctx context.Context, opts ...ConnectOption) error {
	r.opts = &connectOptions{enableUBJSON: true}

	for _, opt := range opts {
		if err := opt(r.opts); err != nil {
			return err
		}
	}

	glog.V(1).Infof("Connecting to %s over %s", r.opts.connectAddress, r.opts.proto)

	switch r.opts.proto {

	case tHTTP_POST:
		r.codec = codec.OutboundHTTP(r.opts.connectAddress, r.opts.serverHost, r.opts.cert, r.opts.caPool)
	case tWebSocket:
		r.codec = codec.NewReconnectWrapperCodec(
			r.opts.connectAddress,
			func(wsURL string) (codec.Codec, error) {
				c, err := r.wsConnect(wsURL, r.opts)
				return c, errors.Trace(err)
			})
	case tPlainTCP:
		r.codec = codec.NewReconnectWrapperCodec(
			r.opts.connectAddress,
			func(tcpAddress string) (codec.Codec, error) {
				c, err := r.tcpConnect(tcpAddress, r.opts)
				return c, errors.Trace(err)
			})
	case tSerial:
		if r.opts.enableReconnect {
			r.codec = codec.NewReconnectWrapperCodec(
				r.opts.connectAddress,
				func(serialAddress string) (codec.Codec, error) {
					c, err := r.serialConnect(ctx, serialAddress, r.opts)
					return c, errors.Trace(err)
				})
		} else {
			serialCodec, err := r.serialConnect(ctx, r.opts.connectAddress, r.opts)
			if err != nil {
				return errors.Trace(err)
			}
			r.codec = serialCodec
		}

	default:
		return fmt.Errorf("unknown transport %q", r.opts.proto)
	}

	return nil
}

func (r *mgRPCImpl) Disconnect(ctx context.Context) error {
	r.codec.Close()
	return nil
}

func (r *mgRPCImpl) recvLoop(ctx context.Context, c codec.Codec) {
	for {
		f, err := c.Recv(ctx)
		if err != nil {
			glog.Infof("error returned from codec Recv: %s, breaking out of the recvLoop", err)
			r.reqsLock.Lock()
			for k, v := range r.reqs {
				v.errChan <- err
				delete(r.reqs, k)
			}
			r.reqsLock.Unlock()
			return
		}

		if glog.V(2) {
			s := fmt.Sprintf("%+v", f)
			if len(s) > 1024 {
				s = fmt.Sprintf("%s... (%d)", s[:1024], len(s))
			}
			glog.V(2).Infof("Rec'd %s", s)
		}

		resp := frame.NewResponseFromFrame(f)
		r.reqsLock.Lock()
		if req, ok := r.reqs[resp.ID]; ok {
			req.respChan <- resp
			delete(r.reqs, resp.ID)
		} else {
			glog.Infof("ignoring unsolicited response: %v", resp)
		}
		r.reqsLock.Unlock()
	}
}

func (r *mgRPCImpl) Call(
	ctx context.Context, dst string, cmd *frame.Command,
) (*frame.Response, error) {
	if cmd.ID == 0 {
		cmd.ID = frame.CreateCommandUID()
	}

	respChan := make(chan *frame.Response)
	errChan := make(chan error)

	r.reqsLock.Lock()
	r.reqs[cmd.ID] = req{
		respChan: respChan,
		errChan:  errChan,
	}
	r.reqsLock.Unlock()
	glog.V(2).Infof("created a request with id %d", cmd.ID)

	f := frame.NewRequestFrame(r.opts.localID, dst, "", cmd)
	r.codec.Send(ctx, f)

	select {
	case resp := <-respChan:
		glog.V(2).Infof("got response on request %d: [%v]", cmd.ID, resp)
		return resp, nil
	case err := <-errChan:
		glog.V(2).Infof("got err on request %d: [%v]", cmd.ID, err)
		return nil, errors.Trace(err)
	case <-ctx.Done():
		glog.V(2).Infof("context for the request %d is done: %v", cmd.ID, ctx.Err())
		r.reqsLock.Lock()
		delete(r.reqs, cmd.ID)
		r.reqsLock.Unlock()
		return nil, errors.Trace(ctx.Err())
	}
}

func (r *mgRPCImpl) SendHello(dst string) {
	hello := &frame.Command{
		Cmd: "/v1/Hello",
	}
	glog.V(2).Infof("Sending hello to %q", dst)
	resp, err := r.Call(context.Background(), dst, hello)
	glog.V(2).Infof("Hello response: %+v, %s", resp, err)
}
