package utils

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type OutboundPolicy string

const (
	OutboundPolicyAllowPrivate OutboundPolicy = "allow_private"
	OutboundPolicyPublicOnly   OutboundPolicy = "public_only"
)

type lookupNetIPFunc func(context.Context, string, string) ([]netip.Addr, error)

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func OutboundPolicyFromConfigs(configs map[string]any) OutboundPolicy {
	if len(configs) == 0 {
		return OutboundPolicyAllowPrivate
	}

	value, ok := configs["outbound_policy"]
	if !ok || value == nil {
		return OutboundPolicyAllowPrivate
	}

	policy, ok := value.(string)
	if !ok {
		return OutboundPolicy("invalid")
	}

	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == "" {
		return OutboundPolicyAllowPrivate
	}

	return OutboundPolicy(policy)
}

var nonPublicOutboundPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("::ffff:0:0:0/80"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

var globalIPv6UnicastPrefix = netip.MustParsePrefix("2000::/3")

func isPublicOutboundAddress(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	if address.Is4In6() {
		return false
	}

	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return false
	}
	if address.Is6() && !globalIPv6UnicastPrefix.Contains(address) {
		return false
	}

	for _, prefix := range nonPublicOutboundPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}

	return true
}

func publicOnlyDialContext(
	lookup lookupNetIPFunc,
	dial dialContextFunc,
) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split outbound address: %w", err)
		}

		host = strings.Trim(host, "[]")
		addresses := make([]netip.Addr, 0, 1)
		if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
			addresses = append(addresses, literal)
		} else {
			addresses, err = lookup(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("resolve public-only outbound host %q: %w", host, err)
			}
		}

		if len(addresses) == 0 {
			return nil, fmt.Errorf("resolve public-only outbound host %q: no addresses", host)
		}

		for _, resolved := range addresses {
			if !isPublicOutboundAddress(resolved) {
				return nil, fmt.Errorf(
					"public-only outbound host %q resolved to non-public address %s",
					host,
					resolved,
				)
			}
		}

		ipv4Addresses := make([]netip.Addr, 0, len(addresses))
		for _, resolved := range addresses {
			if resolved.Is4() {
				ipv4Addresses = append(ipv4Addresses, resolved)
			}
		}
		if len(ipv4Addresses) == 0 {
			return nil, fmt.Errorf(
				"public-only outbound host %q has no public IPv4 address",
				host,
			)
		}

		var lastErr error
		if network == "tcp6" {
			return nil, errors.New("public-only outbound policy does not dial IPv6")
		}
		for _, resolved := range ipv4Addresses {

			conn, dialErr := dial(ctx, network, net.JoinHostPort(resolved.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}

		if lastErr == nil {
			lastErr = errors.New("no resolved address matches the requested network")
		}

		return nil, lastErr
	}
}

type publicOnlyRoundTripper struct {
	transport http.RoundTripper
}

func (roundTripper publicOnlyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := validatePublicOnlyURL(req); err != nil {
		return nil, err
	}
	return roundTripper.transport.RoundTrip(req)
}

func (roundTripper publicOnlyRoundTripper) CloseIdleConnections() {
	if closer, ok := roundTripper.transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func validatePublicOnlyURL(req *http.Request) error {
	if req == nil || req.URL == nil || req.URL.Hostname() == "" {
		return errors.New("public-only outbound target is invalid")
	}
	if !strings.EqualFold(req.URL.Scheme, "https") {
		return errors.New("public-only outbound policy requires HTTPS")
	}
	if req.URL.User != nil {
		return errors.New("public-only outbound target must not include credentials")
	}
	return nil
}

func publicOnlyRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if err := validatePublicOnlyURL(req); err != nil {
		return err
	}

	return nil
}
