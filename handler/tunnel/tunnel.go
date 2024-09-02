package tunnel

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/limiter/traffic"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/sd"
	"github.com/go-gost/relay"
	"github.com/go-gost/x/internal/util/mux"

	"github.com/go-gost/core/observer/stats"
	traffic_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	stats_wrapper "github.com/go-gost/x/observer/stats/wrapper"
	"github.com/go-gost/x/selector"
	"github.com/google/uuid"
)

const (
	MaxWeight uint8 = 0xff
)

type ConnectorOptions struct {
	service string
	sd      sd.SD
	stats   *stats.Stats
	limiter traffic.TrafficLimiter
}

type Connector struct {
	id   relay.ConnectorID
	tid  relay.TunnelID
	node string
	s    *mux.Session
	t    time.Time
	opts *ConnectorOptions
}

func NewConnector(id relay.ConnectorID, tid relay.TunnelID, node string, s *mux.Session, opts *ConnectorOptions) *Connector {
	if opts == nil {
		opts = &ConnectorOptions{}
	}

	c := &Connector{
		id:   id,
		tid:  tid,
		node: node,
		s:    s,
		t:    time.Now(),
		opts: opts,
	}
	go c.accept()
	return c
}

func (c *Connector) accept() {
	for {
		conn, err := c.s.Accept()
		if err != nil {
			logger.Default().Errorf("connector %s: %v", c.id, err)
			c.s.Close()
			if c.opts.sd != nil {
				c.opts.sd.Deregister(context.Background(), &sd.Service{
					ID:   c.id.String(),
					Name: c.tid.String(),
					Node: c.node,
				})
			}
			return
		}
		conn.Close()
	}
}

func (c *Connector) ID() relay.ConnectorID {
	return c.id
}

func (c *Connector) GetConn() (net.Conn, error) {
	if c == nil || c.s == nil {
		return nil, nil
	}

	conn, err := c.s.GetConn()
	if err != nil {
		return nil, err
	}

	conn = stats_wrapper.WrapConn(conn, c.opts.stats)

	network := "tcp"
	if c.id.IsUDP() {
		network = "udp"
	}
	conn = traffic_wrapper.WrapConn(
		conn,
		c.opts.limiter,
		c.tid.String(),
		limiter.ScopeOption(limiter.ScopeClient),
		limiter.ServiceOption(c.opts.service),
		limiter.ClientOption(c.tid.String()),
		limiter.NetworkOption(network),
		limiter.SrcOption(conn.RemoteAddr().String()),
	)
	return conn, nil
}

func (c *Connector) Close() error {
	if c == nil || c.s == nil {
		return nil
	}

	return c.s.Close()
}

func (c *Connector) IsClosed() bool {
	if c == nil || c.s == nil {
		return true
	}

	return c.s.IsClosed()
}

type Tunnel struct {
	node       string
	id         relay.TunnelID
	connectors []*Connector
	t          time.Time
	close      chan struct{}
	mu         sync.RWMutex
	sd         sd.SD
	ttl        time.Duration
}

func NewTunnel(node string, tid relay.TunnelID, ttl time.Duration) *Tunnel {
	t := &Tunnel{
		node:  node,
		id:    tid,
		t:     time.Now(),
		close: make(chan struct{}),
		ttl:   ttl,
	}
	if t.ttl <= 0 {
		t.ttl = defaultTTL
	}
	go t.clean()
	return t
}

func (t *Tunnel) WithSD(sd sd.SD) {
	t.sd = sd
}

func (t *Tunnel) ID() relay.TunnelID {
	return t.id
}

func (t *Tunnel) AddConnector(c *Connector) {
	if c == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.connectors = append(t.connectors, c)
}

func (t *Tunnel) GetConnector(network string) *Connector {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if len(t.connectors) == 1 {
		return t.connectors[0]
	}

	rw := selector.NewRandomWeighted[*Connector]()

	found := false
	for _, c := range t.connectors {
		if c.IsClosed() {
			continue
		}

		weight := c.ID().Weight()
		if weight == 0 {
			weight = 1
		}

		if network == "udp" && c.id.IsUDP() ||
			network != "udp" && !c.id.IsUDP() {
			if weight == MaxWeight && !found {
				rw.Reset()
				found = true
			}

			if weight == MaxWeight || !found {
				rw.Add(c, int(weight))
			}
		}
	}

	return rw.Next()
}

func (t *Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	select {
	case <-t.close:
	default:
		for _, c := range t.connectors {
			c.Close()
		}
		close(t.close)
	}

	return nil
}

func (t *Tunnel) CloseOnIdle() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	select {
	case <-t.close:
	default:
		if len(t.connectors) == 0 {
			close(t.close)
			return true
		}
	}
	return false
}

func (t *Tunnel) clean() {
	ticker := time.NewTicker(t.ttl)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.mu.Lock()
			if len(t.connectors) == 0 {
				t.mu.Unlock()
				break
			}
			var connectors []*Connector
			for _, c := range t.connectors {
				if c.IsClosed() {
					logger.Default().Debugf("remove tunnel: %s, connector: %s", t.id, c.id)
					if t.sd != nil {
						t.sd.Deregister(context.Background(), &sd.Service{
							ID:   c.id.String(),
							Name: t.id.String(),
							Node: t.node,
						})
					}
					continue
				}

				connectors = append(connectors, c)
				if t.sd != nil {
					t.sd.Renew(context.Background(), &sd.Service{
						ID:   c.id.String(),
						Name: t.id.String(),
						Node: t.node,
					})
				}
			}
			if len(connectors) != len(t.connectors) {
				t.connectors = connectors
			}
			t.mu.Unlock()
		case <-t.close:
			return
		}
	}
}

type ConnectorPool struct {
	node    string
	sd      sd.SD
	tunnels map[string]*Tunnel
	mu      sync.RWMutex
}

func NewConnectorPool(node string, sd sd.SD) *ConnectorPool {
	p := &ConnectorPool{
		node:    node,
		sd:      sd,
		tunnels: make(map[string]*Tunnel),
	}
	go p.closeIdles()
	return p
}

func (p *ConnectorPool) Add(tid relay.TunnelID, c *Connector, ttl time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := tid.String()

	t := p.tunnels[s]
	if t == nil {
		t = NewTunnel(p.node, tid, ttl)
		t.WithSD(p.sd)

		p.tunnels[s] = t
	}
	t.AddConnector(c)
}

func (p *ConnectorPool) Get(network string, tid string) *Connector {
	if p == nil {
		return nil
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	t := p.tunnels[tid]
	if t == nil {
		return nil
	}

	return t.GetConnector(network)
}

func (p *ConnectorPool) Close() error {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for k, v := range p.tunnels {
		v.Close()
		delete(p.tunnels, k)
	}

	return nil
}

func (p *ConnectorPool) closeIdles() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		p.mu.Lock()
		for k, v := range p.tunnels {
			if v.CloseOnIdle() {
				delete(p.tunnels, k)
				logger.Default().Debugf("remove idle tunnel: %s", k)
			}
		}
		p.mu.Unlock()
	}
}

func parseTunnelID(s string) (tid relay.TunnelID) {
	if s == "" {
		return
	}
	private := false
	if s[0] == '$' {
		private = true
		s = s[1:]
	}
	uuid, _ := uuid.Parse(s)

	if private {
		return relay.NewPrivateTunnelID(uuid[:])
	}
	return relay.NewTunnelID(uuid[:])
}
