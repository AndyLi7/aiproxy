package ownedartifact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var errArtifactDNS = errors.New("artifact DNS resolution unavailable")

// Local TUN proxies may return Fake-IP answers that our public-address guard
// correctly rejects. This opt-in uses authenticated HTTPS DNS for IPv4 answers;
// it never changes the system resolver or bypasses the download address guard.
func lookupArtifactIPs(ctx context.Context, host string) ([]net.IPAddr, error) {
	switch os.Getenv("NATIVE_ARTIFACT_DNS_RESOLVER") {
	case "", "system":
		return net.DefaultResolver.LookupIPAddr(ctx, host)
	case "cloudflare":
		transport := artifactTransport()
		transport.MaxResponseHeaderBytes = 16 << 10
		client := &http.Client{
			Transport: transport, Timeout: 5 * time.Second, CheckRedirect: noRedirect,
		}
		defer client.CloseIdleConnections()
		return cloudflareArtifactIPs(ctx, host, client)
	default:
		return nil, errArtifactDNS
	}
}

func cloudflareArtifactIPs(ctx context.Context, host string, client *http.Client) ([]net.IPAddr, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IPAddr{{IP: ip}}, nil
	}
	if len(host) == 0 || len(host) > 253 || strings.ContainsAny(host, "/:@?#%\\ \t\r\n") {
		return nil, errArtifactDNS
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := "https://cloudflare-dns.com/dns-query?" + url.Values{
		"name": {host}, "type": {"A"},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errArtifactDNS
	}
	req.Header.Set("Accept", "application/dns-json")
	response, err := client.Do(req)
	if err != nil {
		return nil, errArtifactDNS
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errArtifactDNS
	}
	const maxResponse = 64 << 10
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse {
		return nil, errArtifactDNS
	}
	var answer struct {
		Status int
		Answer []struct {
			Type int
			Data string
		}
	}
	if json.Unmarshal(body, &answer) != nil || answer.Status != 0 {
		return nil, errArtifactDNS
	}
	ips := []net.IPAddr{}
	for _, record := range answer.Answer {
		if record.Type != 1 {
			continue
		}
		ip := net.ParseIP(record.Data)
		if ip == nil || ip.To4() == nil || !publicIP(ip) {
			return nil, errArtifactDNS
		}
		ips = append(ips, net.IPAddr{IP: ip})
	}
	if len(ips) == 0 {
		return nil, errArtifactDNS
	}
	return ips, nil
}
