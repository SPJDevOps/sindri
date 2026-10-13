package main

import (
	"net"
	"net/netip"
)

// dockerDesktopHost returns the registry host a Docker Desktop daemon must
// push to when the server listens on loopback only, or "" when it does not.
//
// Docker Desktop runs its daemon in a VM, so `docker push localhost:8080/...`
// reaches the VM, not this machine. The daemon reaches this machine as
// host.docker.internal instead. A server bound to every interface needs no
// hint: the address says nothing about where the daemon is.
func dockerDesktopHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if host != "localhost" {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.IsLoopback() {
			return ""
		}
	}
	return net.JoinHostPort("host.docker.internal", port)
}
