package config

import (
	"sort"
	"time"

	"github.com/spj/sindri/internal/model"
)

// Preset is a named upstream pinned in the binary, so that nobody hand-types
// the pypi.org simple index and an upstream URL change is a release rather
// than a surprise (ARCHITECTURE.md section 8.2). `sindri config show
// --expanded` prints what a preset became.
type Preset struct {
	URL         string
	Protocol    model.Protocol
	TTL         time.Duration
	NegativeTTL time.Duration
}

// Protocols this build does not serve. Their presets exist so that the names
// are reserved and the error is precise, rather than "unknown preset".
const (
	protocolNPM   model.Protocol = "npm"
	protocolMaven model.Protocol = "maven"
)

var presets = map[string]Preset{
	"pypi": {
		URL:         "https://pypi.org/simple",
		Protocol:    model.ProtocolPyPI,
		TTL:         time.Hour,
		NegativeTTL: 5 * time.Minute,
	},
	"dockerhub": {
		URL:         "https://registry-1.docker.io",
		Protocol:    model.ProtocolOCI,
		TTL:         time.Hour,
		NegativeTTL: 5 * time.Minute,
	},
	"ghcr": {
		URL:         "https://ghcr.io",
		Protocol:    model.ProtocolOCI,
		TTL:         time.Hour,
		NegativeTTL: 5 * time.Minute,
	},
	"npm": {
		URL:         "https://registry.npmjs.org",
		Protocol:    protocolNPM,
		TTL:         time.Hour,
		NegativeTTL: 5 * time.Minute,
	},
	"maven-central": {
		URL:         "https://repo.maven.apache.org/maven2",
		Protocol:    protocolMaven,
		TTL:         time.Hour,
		NegativeTTL: 5 * time.Minute,
	},
}

// LookupPreset returns a named upstream preset.
func LookupPreset(name string) (Preset, bool) {
	p, ok := presets[name]
	return p, ok
}

// PresetNames lists the presets in a stable order, for error messages.
func PresetNames() []string {
	names := make([]string, 0, len(presets))
	for n := range presets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// servedProtocol reports whether this build has an adapter for p.
func servedProtocol(p model.Protocol) bool {
	return p == model.ProtocolOCI || p == model.ProtocolPyPI
}
