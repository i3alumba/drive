package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Reject special-purpose networks as well as private/LAN destinations. Resolve
// inside DialContext and dial the checked literal: a second DNS lookup must not
// turn a public URL into a connection to an internal service.
var remoteDenied = func() []netip.Prefix {
	var out []netip.Prefix
	for _, raw := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3", "::/96", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(raw))
	}
	return out
}()

func publicRemoteIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range remoteDenied {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func validateRemoteURL(u *url.URL) error {
	if u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.Hostname() == "" || len(u.String()) > 8192 {
		return errors.New("use a public HTTP or HTTPS URL without credentials or a fragment")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") && !strings.Contains(host, ":") {
		return errors.New("local destinations are not allowed")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicRemoteIP(ip) {
		return errors.New("local destinations are not allowed")
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return errors.New("only HTTP ports 80 and 443 are allowed")
	}
	return nil
}

func remoteHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil, DisableCompression: true, DisableKeepAlives: true, MaxConnsPerHost: 2,
		MaxResponseHeaderBytes: 64 << 10,
		TLSHandshakeTimeout:    15 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errors.New("invalid remote host")
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil || len(ips) == 0 {
				return nil, errors.New("remote host unavailable")
			}
			for _, ip := range ips {
				if !publicRemoteIP(ip) {
					return nil, errors.New("local destinations are not allowed")
				}
			}
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
			}
			return nil, errors.New("remote connection failed")
		},
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("HTTPS downgrade is not allowed")
		}
		req.Header.Del("Referer")
		return validateRemoteURL(req.URL)
	}}
}
