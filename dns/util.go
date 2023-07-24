package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	D "github.com/miekg/dns"
	"github.com/phuslu/log"
	"github.com/samber/lo"

	"github.com/Dreamacro/clash/common/cache"
	"github.com/Dreamacro/clash/common/errors2"
	"github.com/Dreamacro/clash/common/picker"
	"github.com/Dreamacro/clash/component/dialer"
	"github.com/Dreamacro/clash/component/resolver"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/tunnel"
)

const (
	proxyKey     = contextKey("key-dns-client-proxy")
	proxyTimeout = 10 * time.Second
)

func putMsgToCache(c *cache.LruCache[string, *D.Msg], key string, msg *D.Msg) {
	putMsgToCacheWithExpire(c, key, msg, 0)
}

func putMsgToCacheWithExpire(c *cache.LruCache[string, *D.Msg], key string, msg *D.Msg, ttl uint32) {
	if ttl == 0 {
		if ttl = minTTL(msg.Answer); ttl == 0 {
			return
		}
	}

	c.SetWithExpire(key, msg.Copy(), time.Now().Add(time.Duration(ttl)*time.Second))
}

func setMsgTTL(msg *D.Msg, ttl uint32) {
	setMsgTTLWithForce(msg, ttl, true)
}

func setMsgMaxTTL(msg *D.Msg, ttl uint32) {
	setMsgTTLWithForce(msg, ttl, false)
}

func setMsgTTLWithForce(msg *D.Msg, ttl uint32, force bool) {
	setTTL(msg.Answer, ttl, force)
	setTTL(msg.Ns, ttl, force)
	setTTL(msg.Extra, ttl, force)
}

func setTTL(records []D.RR, ttl uint32, force bool) {
	if force {
		for i := range records {
			records[i].Header().Ttl = ttl
		}
		return
	}

	delta := minTTL(records) - ttl
	for i := range records {
		records[i].Header().Ttl = lo.Clamp(records[i].Header().Ttl-delta, 1, records[i].Header().Ttl)
	}
}

func minTTL(records []D.RR) uint32 {
	minObj := lo.MinBy(records, func(r1 D.RR, r2 D.RR) bool {
		return r1.Header().Ttl < r2.Header().Ttl
	})
	if minObj != nil {
		return minObj.Header().Ttl
	}
	return 0
}

func isIPRequest(q D.Question) bool {
	return q.Qclass == D.ClassINET && (q.Qtype == D.TypeA || q.Qtype == D.TypeAAAA)
}

func transform(servers []NameServer, r *Resolver) []dnsClient {
	var ret []dnsClient
	for _, s := range servers {
		switch s.Net {
		case "https":
			ret = append(ret, newDoHClient(s.Addr, s.Proxy, r))
			continue
		case "dhcp":
			ret = append(ret, newDHCPClient(s.Addr))
			continue
		}

		ret = append(ret, newClient(s.Net, s.Addr, s.Proxy, s.Interface, s.IsDHCP, r))
	}
	return ret
}

func handleMsgWithEmptyAnswer(r *D.Msg) *D.Msg {
	msg := &D.Msg{}
	msg.Answer = []D.RR{}

	msg.SetRcode(r, D.RcodeSuccess)
	msg.Authoritative = true
	msg.RecursionAvailable = true

	return msg
}

func msgToIP(msg *D.Msg) []netip.Addr {
	var ips []netip.Addr

	for _, answer := range msg.Answer {
		switch ans := answer.(type) {
		case *D.AAAA:
			ip, ok := netip.AddrFromSlice(ans.AAAA)
			if !ok {
				continue
			}
			ips = append(ips, ip)
		case *D.A:
			ip, ok := netip.AddrFromSlice(ans.A)
			if !ok {
				continue
			}
			ips = append(ips, ip)
		}
	}

	return ips
}

func msgToIPStr(msg *D.Msg) []string {
	var ips []string

	for _, answer := range msg.Answer {
		switch ans := answer.(type) {
		case *D.AAAA:
			if ans.AAAA == nil {
				continue
			}
			ips = append(ips, ans.AAAA.String())
		case *D.A:
			if ans.A == nil {
				continue
			}
			ips = append(ips, ans.A.String())
		}
	}

	return ips
}

type wrapPacketConn struct {
	net.PacketConn
	rAddr net.Addr
}

func (wpc *wrapPacketConn) Read(b []byte) (n int, err error) {
	n, _, err = wpc.PacketConn.ReadFrom(b)
	return n, err
}

func (wpc *wrapPacketConn) Write(b []byte) (n int, err error) {
	return wpc.PacketConn.WriteTo(b, wpc.rAddr)
}

func (wpc *wrapPacketConn) RemoteAddr() net.Addr {
	return wpc.rAddr
}

func dialContextByProxyOrInterface(
	ctx context.Context,
	network string,
	dstIP netip.Addr,
	port string,
	proxyOrInterface string,
	opts ...dialer.Option,
) (net.Conn, error) {
	proxy, ok := tunnel.FindProxyByName(proxyOrInterface)
	if !ok {
		opts = []dialer.Option{dialer.WithInterface(proxyOrInterface), dialer.WithRoutingMark(0)}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(dstIP.String(), port), opts...)
		if err == nil {
			return conn, nil
		}
		return nil, fmt.Errorf("proxy %s not found, %w", proxyOrInterface, err)
	}

	networkType := C.TCP
	if network == "udp" {
		networkType = C.UDP
	}

	metadata := &C.Metadata{
		NetWork: networkType,
		Host:    "",
		DstIP:   dstIP,
		DstPort: port,
	}

	if networkType == C.UDP {
		if !proxy.SupportUDP() {
			if tunnel.UDPFallbackMatch.Load() {
				return nil, fmt.Errorf("proxy %s UDP is not supported", proxy.Name())
			} else {
				log.Debug().
					Str("proxy", proxy.Name()).
					Msg("[DNS] proxy UDP is not supported, fallback to TCP")

				metadata.NetWork = C.TCP
				goto tcp
			}
		}

		packetConn, err := proxy.ListenPacketContext(ctx, metadata, opts...)
		if err != nil {
			return nil, err
		}

		return &wrapPacketConn{
			PacketConn: packetConn,
			rAddr:      metadata.UDPAddr(),
		}, nil
	}

tcp:
	return proxy.DialContext(ctx, metadata, opts...)
}

func batchExchange(ctx context.Context, clients []dnsClient, m *D.Msg) (msg *D.Msg, err error) {
	fast, ctx := picker.WithContext[*D.Msg](ctx)
	for _, clientM := range clients {
		r := clientM
		fast.Go(func() (*D.Msg, error) {
			mm, fErr := r.ExchangeContext(ctx, m)
			if fErr != nil {
				return nil, fErr
			} else if mm.Rcode == D.RcodeServerFailure || mm.Rcode == D.RcodeRefused {
				return nil, errors.New("server failure")
			}
			return mm, nil
		})
	}

	elm := fast.Wait()
	if elm == nil {
		err = errors.New("all DNS requests failed")
		if fErr := fast.Error(); fErr != nil {
			err = errors.Join(err, fErr)
		}
		return nil, errors2.Cause(err)
	}

	return elm, nil
}

func genMsgCacheKey(ctx context.Context, q D.Question) string {
	if proxy, ok := resolver.GetProxy(ctx); ok && proxy != "" {
		return fmt.Sprintf("%s:%s:%d:%d", proxy, q.Name, q.Qtype, q.Qclass)
	}
	return fmt.Sprintf("%s:%d:%d", q.Name, q.Qtype, q.Qclass)
}

func getTCPConn(ctx context.Context, addr string) (conn net.Conn, err error) {
	if proxy, ok := ctx.Value(proxyKey).(string); ok {
		host, port, _ := net.SplitHostPort(addr)
		ip, err1 := netip.ParseAddr(host)
		if err1 != nil {
			return nil, err1
		}
		conn, err = dialContextByProxyOrInterface(ctx, "tcp", ip, port, proxy)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}

	if err == nil {
		if c, ok := conn.(*net.TCPConn); ok {
			_ = c.SetKeepAlive(true)
		}
	}
	return
}

func logDnsResponse(q D.Question, msg *D.Msg, err error, network, source, proxyAdapter string) {
	if q.Qtype != D.TypeA && q.Qtype != D.TypeAAAA {
		return
	}

	var pr string
	if network != "" {
		network = network + "://"
	}
	if proxyAdapter != "" {
		pr = "(" + proxyAdapter + ")"
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		log.Debug().
			Err(err).
			Str("source", fmt.Sprintf("%s%s%s", network, source, pr)).
			Str("qType", D.Type(q.Qtype).String()).
			Str("name", q.Name).
			Msg("[DNS] dns response failed")
	} else if msg != nil {
		log.Debug().
			Str("source", fmt.Sprintf("%s%s%s", network, source, pr)).
			Str("qType", D.Type(q.Qtype).String()).
			Str("name", q.Name).
			Strs("answer", msgToIPStr(msg)).
			Msg("[DNS] dns response")
	}
}
