package config

import (
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultDocsURL is where the UI's footer and sign-in help point when `ui.docsUrl`
// is not set.
const DefaultDocsURL = "https://spjdevops.github.io/sindri/"

// UI configures the read-only web interface. It is a server-level section with
// no manifest form: it is an operator concern, not a repository one.
type UI struct {
	// Where the "Docs" link in the web interface footer, and the sign-in help,
	// point. Set it to an internal copy of the documentation in an air-gapped
	// install. An absolute `http` or `https` URL without credentials.
	DocsURL string `yaml:"docsUrl"`
}

func defaultUI() UI { return UI{DocsURL: DefaultDocsURL} }

// UnmarshalYAML accepts only the mapping.
func (u *UI) UnmarshalYAML(node *yaml.Node) error {
	type raw UI
	return strictDecode(node, (*raw)(u))
}

func (u *UI) applyDefaults() {
	if u.DocsURL == "" {
		u.DocsURL = defaultUI().DocsURL
	}
}

func (u *UI) validate() error {
	parsed, err := url.Parse(u.DocsURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("ui.docsUrl: must be an absolute http or https URL")
	}
	if parsed.User != nil {
		return fmt.Errorf("ui.docsUrl: must not contain credentials")
	}
	if strings.ContainsAny(u.DocsURL, " \t\r\n") {
		return fmt.Errorf("ui.docsUrl: must not contain whitespace")
	}
	return nil
}
