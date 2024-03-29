// package chinadns implements a ChinaDNSing proxy. It caches an upstream net.Conn for some time, so if the same
// client returns the upstream's Conn will be precached. Depending on how you benchmark this looks to be
// 50% faster than just opening a new connection for every client. It works with UDP and TCP and uses
// inband healthchecking.
package chinadns

import (
	"context"
	"crypto/tls"
	"errors"
	maxminddb "github.com/oschwald/maxminddb-golang"
	"net"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/debug"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"

	"github.com/miekg/dns"
	ot "github.com/opentracing/opentracing-go"
)

var log = clog.NewWithPlugin("chinadns")

// ChinaDNS represents a plugin instance that can proxy requests to another (DNS) server. It has a list
// of proxies each representing one upstream proxy.
type ChinaDNS struct {
	proxies    []*Proxy
	p          Policy
	hcInterval time.Duration

	from    string
	ignored []string

	tlsConfig     *tls.Config
	tlsServerName string
	maxfails      uint32
	expire        time.Duration
	Geoip         *maxminddb.Reader
	opts          options // also here for testing

	Next plugin.Handler
}

// New returns a new ChinaDNS.
func New() *ChinaDNS {
	f := &ChinaDNS{maxfails: 2, tlsConfig: new(tls.Config), expire: defaultExpire, p: new(random), from: ".", hcInterval: hcInterval}
	return f
}



// SetProxy appends p to the proxy list and starts healthchecking.
func (f *ChinaDNS) SetProxy(p *Proxy) {
	f.proxies = append(f.proxies, p)
	p.start(f.hcInterval)
}

// Len returns the number of configured proxies.
func (f *ChinaDNS) Len() int { return len(f.proxies) }

// Name implements plugin.Handler.
func (f *ChinaDNS) Name() string { return "chinadns" }


func (f *ChinaDNS) isInsideChina(res *dns.Msg) bool {
	if len(res.Answer) == 0 {
		return false
	}

	for _, ans := range res.Answer {
		var ip net.IP

		if ans.Header().Rrtype == dns.TypeA {
			ip = ans.(*dns.A).A
		} else {
			continue
		}

		var record struct {
			Country struct {
				ISOCode string `maxminddb:"iso_code"`
			} `maxminddb:"country"`
		}
		err := f.Geoip.Lookup(ip, &record)
		if err != nil {
			return false
		}
		if record.Country.ISOCode != "CN" {
			return false
		}
	}

	return true
}

// ServeDNS implements plugin.Handler.
func (f *ChinaDNS) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {

	state := request.Request{W: w, Req: r}
	if !f.match(state) {
		return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
	}

	fails := 0
	var span, child ot.Span
	span = ot.SpanFromContext(ctx)
	i := 0
	list := f.List()
	deadline := time.Now().Add(defaultTimeout)
	start := time.Now()
	for time.Now().Before(deadline) {
		if i >= len(list) {
			// reached the end of list, reset to begin
			i = 0
			fails = 0
		}

		proxy := list[i]
		i++
		if proxy.Down(f.maxfails) {
			fails++
			if fails < len(f.proxies) {
				continue
			}
			// All upstream proxies are dead, assume healthcheck is completely broken and randomly
			// select an upstream to connect to.
			r := new(random)
			proxy = r.List(f.proxies)[0]

			HealthcheckBrokenCount.Add(1)
		}

		if span != nil {
			child = span.Tracer().StartSpan("connect", ot.ChildOf(span.Context()))
			ctx = ot.ContextWithSpan(ctx, child)
		}

		var (
			ret *dns.Msg
			err error
		)
		opts := f.opts
		for {
			ret, err = proxy.Connect(ctx, state, opts)
			if err == ErrCachedClosed { // Remote side closed conn, can only happen with TCP.
				continue
			}
			// Retry with TCP if truncated and prefer_udp configured.
			if ret != nil && ret.Truncated && !opts.forceTCP && opts.preferUDP {
				opts.forceTCP = true
				continue
			}
			break
		}

		if child != nil {
			child.Finish()
		}
		taperr := toDnstap(ctx, proxy.addr, f, state, ret, start)

		if err != nil {
			// Kick off health check to see if *our* upstream is broken.
			if f.maxfails != 0 {
				proxy.Healthcheck()
			}

			if fails < len(f.proxies) {
				continue
			}
			break
		}


		// Check if the reply is correct; if not return FormErr.
		if !state.Match(ret) {
			debug.Hexdumpf(ret, "Wrong reply for id: %d, %s %d", ret.Id, state.QName(), state.QType())

			formerr := new(dns.Msg)
			formerr.SetRcode(state.Req, dns.RcodeFormatError)
			_ = w.WriteMsg(formerr)
			return 0, taperr
		}
		if f.isInsideChina(ret){
			_ = w.WriteMsg(ret)
			return 0,nil
		}
		return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
	}
	return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)

}

func (f *ChinaDNS) match(state request.Request) bool {
	if !plugin.Name(f.from).Matches(state.Name()) || !f.isAllowedDomain(state.Name()) {
		return false
	}

	return true
}

func (f *ChinaDNS) isAllowedDomain(name string) bool {
	if dns.Name(name) == dns.Name(f.from) {
		return true
	}

	for _, ignore := range f.ignored {
		if plugin.Name(ignore).Matches(name) {
			return false
		}
	}
	return true
}

// ForceTCP returns if TCP is forced to be used even when the request comes in over UDP.
func (f *ChinaDNS) ForceTCP() bool { return f.opts.forceTCP }

// PreferUDP returns if UDP is preferred to be used even when the request comes in over TCP.
func (f *ChinaDNS) PreferUDP() bool { return f.opts.preferUDP }

// List returns a set of proxies to be used for this client depending on the policy in f.
func (f *ChinaDNS) List() []*Proxy { return f.p.List(f.proxies) }

var (
	// ErrCachedClosed means cached connection was closed by peer.
	ErrCachedClosed = errors.New("cached connection was closed by peer")
)

// options holds various options that can be set.
type options struct {
	forceTCP  bool
	preferUDP bool
}

const defaultTimeout = 5 * time.Second
