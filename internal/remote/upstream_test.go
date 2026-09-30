package remote

import (
	"testing"

	"github.com/spj/sindri/internal/registry"
)

func TestUpstreamImagePath(t *testing.T) {
	cases := []struct {
		name     string
		upstream string
		repo     string
		image    string
		want     string
	}{
		{name: "DockerHubOfficialImageGetsLibrary", upstream: "https://registry-1.docker.io", repo: "dockerhub", image: "alpine", want: "library/alpine"},
		{name: "DockerHubAliasHost", upstream: "https://index.docker.io/", repo: "dockerhub", image: "alpine", want: "library/alpine"},
		{name: "DockerHubHostCaseInsensitive", upstream: "https://Registry-1.Docker.IO", repo: "dockerhub", image: "alpine", want: "library/alpine"},
		{name: "DockerHubAlreadyQualified", upstream: "https://registry-1.docker.io", repo: "dockerhub", image: "library/alpine", want: "library/alpine"},
		{name: "DockerHubUserImageUntouched", upstream: "https://registry-1.docker.io", repo: "dockerhub", image: "bitnami/redis", want: "bitnami/redis"},
		{name: "OtherRegistrySingleSegmentUntouched", upstream: "https://ghcr.io", repo: "ghcr", image: "alpine", want: "alpine"},
		{name: "EmptyImageUsesRepoName", upstream: "https://ghcr.io", repo: "org/tool", image: "", want: "org/tool"},
		{name: "EmptyImageOnDockerHubGetsLibrary", upstream: "https://registry-1.docker.io", repo: "alpine", image: "", want: "library/alpine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := upstreamImagePath(registry.Remote{UpstreamURL: tc.upstream}, tc.repo, tc.image)
			if got != tc.want {
				t.Fatalf("upstreamImagePath(%q, %q, %q) = %q, want %q", tc.upstream, tc.repo, tc.image, got, tc.want)
			}
		})
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "SameHostDifferentPath", a: "https://mirror.example/files/x.whl", b: "https://mirror.example/simple", want: true},
		{name: "HostCaseInsensitive", a: "https://Mirror.Example/x", b: "https://mirror.example", want: true},
		{name: "DifferentHost", a: "https://files.example/x", b: "https://mirror.example/simple", want: false},
		{name: "DifferentScheme", a: "http://mirror.example/x", b: "https://mirror.example", want: false},
		{name: "DifferentPort", a: "https://mirror.example:8443/x", b: "https://mirror.example", want: false},
		{name: "LookalikeSuffixHost", a: "https://mirror.example.evil/x", b: "https://mirror.example", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameOrigin(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameOrigin(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
