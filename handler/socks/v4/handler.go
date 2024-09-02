package v4

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/go-gost/core/handler"
	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/limiter/traffic"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	"github.com/go-gost/core/observer/stats"
	"github.com/go-gost/gosocks4"
	ctxvalue "github.com/go-gost/x/ctx"
	netpkg "github.com/go-gost/x/internal/net"
	limiter_util "github.com/go-gost/x/internal/util/limiter"
	stats_util "github.com/go-gost/x/internal/util/stats"
	traffic_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	stats_wrapper "github.com/go-gost/x/observer/stats/wrapper"
	"github.com/go-gost/x/registry"
)

var (
	ErrUnknownCmd    = errors.New("socks4: unknown command")
	ErrUnimplemented = errors.New("socks4: unimplemented")
)

func init() {
	registry.HandlerRegistry().Register("socks4", NewHandler)
	registry.HandlerRegistry().Register("socks4a", NewHandler)
}

type socks4Handler struct {
	md      metadata
	options handler.Options
	stats   *stats_util.HandlerStats
	limiter traffic.TrafficLimiter
	cancel  context.CancelFunc
}

func NewHandler(opts ...handler.Option) handler.Handler {
	options := handler.Options{}
	for _, opt := range opts {
		opt(&options)
	}

	return &socks4Handler{
		options: options,
	}
}

func (h *socks4Handler) Init(md md.Metadata) (err error) {
	if err := h.parseMetadata(md); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel

	if h.options.Observer != nil {
		h.stats = stats_util.NewHandlerStats(h.options.Service)
		go h.observeStats(ctx)
	}

	if limiter := h.options.Limiter; limiter != nil {
		h.limiter = limiter_util.NewCachedTrafficLimiter(limiter, 30*time.Second, 60*time.Second)
	}

	return nil
}

func (h *socks4Handler) Handle(ctx context.Context, conn net.Conn, opts ...handler.HandleOption) error {
	defer conn.Close()

	start := time.Now()

	log := h.options.Logger.WithFields(map[string]any{
		"remote": conn.RemoteAddr().String(),
		"local":  conn.LocalAddr().String(),
	})

	log.Infof("%s <> %s", conn.RemoteAddr(), conn.LocalAddr())
	defer func() {
		log.WithFields(map[string]any{
			"duration": time.Since(start),
		}).Infof("%s >< %s", conn.RemoteAddr(), conn.LocalAddr())
	}()

	if !h.checkRateLimit(conn.RemoteAddr()) {
		return nil
	}

	if h.md.readTimeout > 0 {
		conn.SetReadDeadline(time.Now().Add(h.md.readTimeout))
	}

	req, err := gosocks4.ReadRequest(conn)
	if err != nil {
		log.Error(err)
		return err
	}
	log.Trace(req)

	conn.SetReadDeadline(time.Time{})

	if h.options.Auther != nil {
		id, ok := h.options.Auther.Authenticate(ctx, string(req.Userid), "")
		if !ok {
			resp := gosocks4.NewReply(gosocks4.RejectedUserid, nil)
			log.Trace(resp)
			return resp.Write(conn)
		}
		ctx = ctxvalue.ContextWithClientID(ctx, ctxvalue.ClientID(id))
	}

	switch req.Cmd {
	case gosocks4.CmdConnect:
		return h.handleConnect(ctx, conn, req, log)
	case gosocks4.CmdBind:
		return h.handleBind(ctx, conn, req)
	default:
		err = ErrUnknownCmd
		log.Error(err)
		return err
	}
}

func (h *socks4Handler) Close() error {
	if h.cancel != nil {
		h.cancel()
	}
	return nil
}

func (h *socks4Handler) handleConnect(ctx context.Context, conn net.Conn, req *gosocks4.Request, log logger.Logger) error {
	addr := req.Addr.String()

	log = log.WithFields(map[string]any{
		"dst": addr,
	})
	log.Debugf("%s >> %s", conn.RemoteAddr(), addr)

	if h.options.Bypass != nil && h.options.Bypass.Contains(ctx, "tcp", addr) {
		resp := gosocks4.NewReply(gosocks4.Rejected, nil)
		log.Trace(resp)
		log.Debug("bypass: ", addr)
		return resp.Write(conn)
	}

	switch h.md.hash {
	case "host":
		ctx = ctxvalue.ContextWithHash(ctx, &ctxvalue.Hash{Source: addr})
	}

	cc, err := h.options.Router.Dial(ctx, "tcp", addr)
	if err != nil {
		resp := gosocks4.NewReply(gosocks4.Failed, nil)
		log.Trace(resp)
		resp.Write(conn)
		return err
	}

	defer cc.Close()

	resp := gosocks4.NewReply(gosocks4.Granted, nil)
	log.Trace(resp)
	if err := resp.Write(conn); err != nil {
		log.Error(err)
		return err
	}

	clientID := ctxvalue.ClientIDFromContext(ctx)
	rw := traffic_wrapper.WrapReadWriter(
		h.limiter,
		conn,
		string(clientID),
		limiter.ScopeOption(limiter.ScopeClient),
		limiter.ServiceOption(h.options.Service),
		limiter.NetworkOption("tcp"),
		limiter.AddrOption(addr),
		limiter.ClientOption(string(clientID)),
		limiter.SrcOption(conn.RemoteAddr().String()),
	)
	if h.options.Observer != nil {
		pstats := h.stats.Stats(string(clientID))
		pstats.Add(stats.KindTotalConns, 1)
		pstats.Add(stats.KindCurrentConns, 1)
		defer pstats.Add(stats.KindCurrentConns, -1)
		rw = stats_wrapper.WrapReadWriter(rw, pstats)
	}

	t := time.Now()
	log.Infof("%s <-> %s", conn.RemoteAddr(), addr)
	netpkg.Transport(rw, cc)
	log.WithFields(map[string]any{
		"duration": time.Since(t),
	}).Infof("%s >-< %s", conn.RemoteAddr(), addr)

	return nil
}

func (h *socks4Handler) handleBind(ctx context.Context, conn net.Conn, req *gosocks4.Request) error {
	// TODO: bind
	return ErrUnimplemented
}

func (h *socks4Handler) checkRateLimit(addr net.Addr) bool {
	if h.options.RateLimiter == nil {
		return true
	}
	host, _, _ := net.SplitHostPort(addr.String())
	if limiter := h.options.RateLimiter.Limiter(host); limiter != nil {
		return limiter.Allow(1)
	}

	return true
}

func (h *socks4Handler) observeStats(ctx context.Context) {
	if h.options.Observer == nil {
		return
	}

	d := h.md.observePeriod
	if d < time.Millisecond {
		d = 5 * time.Second
	}
	ticker := time.NewTicker(d)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.options.Observer.Observe(ctx, h.stats.Events())
		case <-ctx.Done():
			return
		}
	}
}
