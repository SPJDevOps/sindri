package config

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Redacted is what `config show` prints in place of a secret.
const Redacted = "<redacted>"

// RedactedCopy returns the server sections with every secret replaced, for
// printing. A secret is never shown, even to the operator who wrote it: the
// output of `config show` ends up pasted into issues and chat (ADR 15).
func (c *Config) RedactedCopy() Config {
	out := *c
	out.Desired = nil
	out.Database.URL = c.Database.Redacted()
	if out.Storage.SecretAccessKey != "" {
		out.Storage.SecretAccessKey = Redacted
	}
	if out.Storage.SessionToken != "" {
		out.Storage.SessionToken = Redacted
	}
	if out.Auth.StaticToken != "" {
		out.Auth.StaticToken = Redacted
	}
	if c.Auth.OIDC != nil {
		o := *c.Auth.OIDC
		if o.ClientSecret != "" {
			o.ClientSecret = Redacted
		}
		out.Auth.OIDC = &o
	}
	if len(c.Credentials) > 0 {
		out.Credentials = make(Credentials, len(c.Credentials))
		for name, cred := range c.Credentials {
			if cred.Username != "" {
				cred.Username = Redacted
			}
			cred.Password = Redacted
			out.Credentials[name] = cred
		}
	}
	return out
}

type docOut struct {
	Kind     string   `yaml:"kind"`
	Metadata *metaOut `yaml:"metadata,omitempty"`
	Spec     any      `yaml:"spec"`
}

type metaOut struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
}

type repoSpecOut struct {
	Type      string        `yaml:"type"`
	Protocol  string        `yaml:"protocol"`
	Upstream  *upstreamOut  `yaml:"upstream,omitempty"`
	Members   []MemberRef   `yaml:"members,omitempty"`
	Access    *accessOut    `yaml:"access,omitempty"`
	Lifecycle *lifecycleOut `yaml:"lifecycle,omitempty"`
}

type upstreamOut struct {
	URL         string   `yaml:"url"`
	TTL         Duration `yaml:"ttl"`
	NegativeTTL Duration `yaml:"negativeTtl"`
	Credentials string   `yaml:"credentials,omitempty"`
}

type accessOut struct {
	Read  []string `yaml:"read,omitempty"`
	Write []string `yaml:"write,omitempty"`
}

type lifecycleOut struct {
	Stages []string `yaml:"stages"`
}

type orgPolicyOut struct {
	Mode           string            `yaml:"mode"`
	MandatoryRepos []mandatoryOut    `yaml:"mandatoryRepos,omitempty"`
	Namespaces     []namespaceOutDoc `yaml:"namespaces,omitempty"`
}

type mandatoryOut struct {
	Name   string `yaml:"name"`
	Locked bool   `yaml:"locked,omitempty"`
}

type namespaceOutDoc struct {
	Name   string   `yaml:"name"`
	Admins []string `yaml:"admins,omitempty"`
}

type stagesOut struct {
	Stages []stageOut `yaml:"stages"`
}

type stageOut struct {
	Name  string   `yaml:"name"`
	Gates []string `yaml:"gates,omitempty"`
}

// MarshalManifests renders the long form as a multi-document YAML stream,
// which LoadManifests reads back to the same tree. Credentials appear only by
// reference name; secrets are not part of Manifests at all.
func MarshalManifests(m *Manifests) ([]byte, error) {
	var docs []docOut
	if m.OrgPolicy != nil {
		p := orgPolicyOut{Mode: string(m.OrgPolicy.Mode)}
		for _, r := range m.OrgPolicy.MandatoryRepos {
			p.MandatoryRepos = append(p.MandatoryRepos, mandatoryOut(r))
		}
		for _, n := range m.OrgPolicy.Namespaces {
			p.Namespaces = append(p.Namespaces, namespaceOutDoc(n))
		}
		docs = append(docs, docOut{Kind: KindOrgPolicy, Spec: p})
	}
	if m.Lifecycle != nil {
		s := stagesOut{Stages: []stageOut{}}
		for _, st := range m.Lifecycle.Stages {
			o := stageOut{Name: st.Name}
			for _, g := range st.Gates {
				o.Gates = append(o.Gates, string(g))
			}
			s.Stages = append(s.Stages, o)
		}
		docs = append(docs, docOut{Kind: KindLifecycle, Spec: s})
	}
	for _, r := range m.Repositories {
		spec := repoSpecOut{
			Type:     string(r.Spec.Type),
			Protocol: string(r.Spec.Protocol),
			Members:  r.Spec.Members,
		}
		if u := r.Spec.Upstream; u != nil {
			spec.Upstream = &upstreamOut{URL: u.URL, TTL: u.TTL, NegativeTTL: u.NegativeTTL, Credentials: u.Credentials}
		}
		if len(r.Spec.Access.Read) > 0 || len(r.Spec.Access.Write) > 0 {
			spec.Access = &accessOut{Read: r.Spec.Access.Read, Write: r.Spec.Access.Write}
		}
		if r.Spec.Lifecycle != nil {
			stages := r.Spec.Lifecycle.Stages
			if stages == nil {
				stages = []string{}
			}
			spec.Lifecycle = &lifecycleOut{Stages: stages}
		}
		docs = append(docs, docOut{
			Kind:     KindRepository,
			Metadata: &metaOut{Name: r.Name, Namespace: r.Namespace},
			Spec:     spec,
		})
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return nil, fmt.Errorf("rendering %s: %w", d.Kind, err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("rendering manifests: %w", err)
	}
	return buf.Bytes(), nil
}

// MarshalServer renders the server sections, secrets redacted.
func MarshalServer(c *Config) ([]byte, error) {
	red := c.RedactedCopy()
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(red); err != nil {
		return nil, fmt.Errorf("rendering configuration: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("rendering configuration: %w", err)
	}
	return buf.Bytes(), nil
}
