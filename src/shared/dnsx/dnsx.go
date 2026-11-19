// Package dnsx resolves hostnames via plaintext DNS or DoH, with ordered
// resolver fallback and optional system-DNS fallback.
package dnsx

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Resolver tries resolvers in order (DoH first if set, then plain, then
// system if allowed).
type Resolver struct {
	Plain       []string
	DoH         []string
	Timeout     time.Duration
	AllowSystem bool
	HTTPClient  *http.Client
}

// Lookup returns A (v4) and AAAA (v6) addresses for host.
func (r *Resolver) Lookup(ctx context.Context, host string) (v4, v6 []net.IP, err error) {
	// With no custom resolver, system DNS is the primary resolver. AllowSystem
	// only controls fallback after explicitly configured resolvers fail.
	if len(r.DoH) == 0 && len(r.Plain) == 0 {
		return r.lookupSystem(ctx, host)
	}
	if len(r.DoH) > 0 {
		for _, url := range r.DoH {
			v4, v6, err = r.lookupDoH(ctx, url, host)
			if err == nil && (len(v4)+len(v6) > 0) {
				return v4, v6, nil
			}
		}
	}
	if len(r.Plain) > 0 {
		for _, s := range r.Plain {
			v4, v6, err = r.lookupPlain(ctx, s, host)
			if err == nil && (len(v4)+len(v6) > 0) {
				return v4, v6, nil
			}
		}
	}
	if r.AllowSystem {
		return r.lookupSystem(ctx, host)
	}
	return nil, nil, fmt.Errorf("dnsx: all resolvers failed for %s", host)
}

func (r *Resolver) timeoutCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, r.Timeout)
}

func (r *Resolver) lookupPlain(ctx context.Context, server, host string) ([]net.IP, []net.IP, error) {
	cctx, cancel := r.timeoutCtx(ctx)
	defer cancel()
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "53")
	}
	res := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: r.Timeout}
			return d.DialContext(ctx, "udp", server)
		},
	}
	addrs, err := res.LookupNetIP(cctx, "ip", host)
	if err != nil {
		return nil, nil, err
	}
	return split(addrs)
}

func (r *Resolver) lookupSystem(ctx context.Context, host string) ([]net.IP, []net.IP, error) {
	cctx, cancel := r.timeoutCtx(ctx)
	defer cancel()
	res := &net.Resolver{PreferGo: false}
	addrs, err := res.LookupNetIP(cctx, "ip", host)
	if err != nil {
		return nil, nil, err
	}
	return split(addrs)
}

func split(addrs []netip.Addr) (v4, v6 []net.IP, err error) {
	for _, a := range addrs {
		if a.Is4() {
			v4 = append(v4, net.IP(a.AsSlice()))
		} else {
			v6 = append(v6, net.IP(a.AsSlice()))
		}
	}
	if len(v4)+len(v6) == 0 {
		return nil, nil, fmt.Errorf("dnsx: no addresses")
	}
	return v4, v6, nil
}

func (r *Resolver) lookupDoH(ctx context.Context, url, host string) ([]net.IP, []net.IP, error) {
	cctx, cancel := r.timeoutCtx(ctx)
	defer cancel()

	var v4, v6 []net.IP
	for _, qtype := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		msg := dnsmessage.Message{
			Header: dnsmessage.Header{
				ID:               uint16(rand.Uint32()),
				RecursionDesired: true,
			},
			Questions: []dnsmessage.Question{{
				Name:  dnsmessage.MustNewName(host + "."),
				Type:  qtype,
				Class: dnsmessage.ClassINET,
			}},
		}
		packed, err := msg.Pack()
		if err != nil {
			continue
		}
		b64 := base64.RawURLEncoding.EncodeToString(packed)
		req, err := http.NewRequestWithContext(cctx, http.MethodGet,
			fmt.Sprintf("%s?dns=%s", url, b64), nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/dns-message")
		client := r.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: r.Timeout}
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		var ans dnsmessage.Message
		if err := ans.Unpack(body); err != nil {
			continue
		}
		for _, a := range ans.Answers {
			switch r := a.Body.(type) {
			case *dnsmessage.AResource:
				v4 = append(v4, net.IP(r.A[:]))
			case *dnsmessage.AAAAResource:
				v6 = append(v6, net.IP(r.AAAA[:]))
			}
		}
	}
	if len(v4)+len(v6) == 0 {
		return nil, nil, fmt.Errorf("dnsx: doh no answers for %s", host)
	}
	return v4, v6, nil
}
