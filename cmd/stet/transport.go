package main

import (
	"net"
	"net/netip"

	"github.com/stetdb/stet/internal/config"
)

// loopbackHost reports whether host is this machine: "localhost" or a
// loopback IP address. An empty or unspecified host (0.0.0.0, ::) is not.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// transportWarnings returns what stet warns of at start about the two legs
// (spec-v0.2-tryout 3). It stops nothing. The upstream's password is never in
// them: only its host is named.
func transportWarnings(cfg *config.Config) []string {
	var warnings []string
	host, _, err := net.SplitHostPort(cfg.UpstreamAddr())
	if err != nil {
		host = cfg.UpstreamAddr()
	}
	if !loopbackHost(host) {
		switch cfg.UpstreamTransport() {
		case config.TransportPlain, config.TransportPreferred:
			warnings = append(warnings, "upstream "+host+` is not this machine and its sslmode allows plaintext: statements, results and "password"-method passwords can cross the network unencrypted; fix: add sslmode=verify-full to upstream (and sslrootcert=<CA file> when the server's CA is not a public one)`)
		case config.TransportUnverified:
			warnings = append(warnings, "upstream "+host+" is not this machine and its certificate is not checked (sslmode=require without sslrootcert): the connection is encrypted, but to whoever answers; fix: use sslmode=verify-full (and sslrootcert=<CA file> when the server's CA is not a public one)")
		}
	}
	listenHost, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		listenHost = cfg.Listen
	}
	if !loopbackHost(listenHost) {
		warnings = append(warnings, "listen "+cfg.Listen+" is not a loopback address: the connection between clients and stet is not encrypted; fix: listen on 127.0.0.1, or keep clients and stet on a network you trust")
	}
	return warnings
}
