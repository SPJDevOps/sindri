package config

import (
	"strings"
	"testing"
)

func TestParse_UIDocsURL(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    string
		wantErr string
	}{
		{name: "AbsentGetsTheDefault", doc: "", want: DefaultDocsURL},
		{name: "EmptyMappingGetsTheDefault", doc: "ui: {}\n", want: DefaultDocsURL},
		{name: "HTTPSIsKept", doc: "ui: {docsUrl: 'https://docs.example.com/sindri/'}\n", want: "https://docs.example.com/sindri/"},
		{name: "PlainHTTPIsAllowedForAnInternalCopy", doc: "ui: {docsUrl: 'http://wiki.internal/sindri'}\n", want: "http://wiki.internal/sindri"},
		{name: "RelativeIsRefused", doc: "ui: {docsUrl: /docs}\n", wantErr: "ui.docsUrl: must be an absolute http or https URL"},
		{name: "OtherSchemeIsRefused", doc: "ui: {docsUrl: 'javascript://x/%0aalert(1)'}\n", wantErr: "ui.docsUrl: must be an absolute http or https URL"},
		{name: "CredentialsAreRefused", doc: "ui: {docsUrl: 'https://u:p@docs.example.com/'}\n", wantErr: "ui.docsUrl: must not contain credentials"},
		{name: "UnknownKeyIsRefused", doc: "ui: {docs: x}\n", wantErr: "docs"},
		{name: "ScalarIsRefused", doc: "ui: https://docs.example.com/\n", wantErr: "ui"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.doc), "ui.yaml")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if cfg.UI.DocsURL != tc.want {
				t.Errorf("UI.DocsURL = %q, want %q", cfg.UI.DocsURL, tc.want)
			}
		})
	}
}
