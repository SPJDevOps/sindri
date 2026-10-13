package main

import "testing"

func TestDockerDesktopHost(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{"IPv4Loopback", "127.0.0.1:8080", "host.docker.internal:8080"},
		{"IPv6Loopback", "[::1]:9090", "host.docker.internal:9090"},
		{"Localhost", "localhost:8080", "host.docker.internal:8080"},
		// Bound to every interface, the daemon's own localhost is no worse a
		// route than any other, and the address says nothing either way.
		{"AllInterfaces", "[::]:8080", ""},
		{"AllInterfacesIPv4", "0.0.0.0:8080", ""},
		{"SpecificInterface", "192.168.1.20:8080", ""},
		{"Malformed", "nonsense", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dockerDesktopHost(tc.addr); got != tc.want {
				t.Fatalf("dockerDesktopHost(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}
