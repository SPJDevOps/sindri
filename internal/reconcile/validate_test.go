package reconcile_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/reconcile"
)

func TestValidateRepository_RejectsLifecycleOnProxy(t *testing.T) {
	err := reconcile.ValidateRepository(reconcile.RepositorySpec{
		Name:            "pypi-public",
		Type:            model.RepoTypeProxy,
		Protocol:        model.ProtocolPyPI,
		UpstreamURL:     "https://pypi.org/simple",
		LifecycleStages: []string{"prod"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "I4") {
		t.Fatalf("error should name I4: %v", err)
	}
}

func TestValidateRepository_RejectsLifecycleOnVirtual(t *testing.T) {
	err := reconcile.ValidateRepository(reconcile.RepositorySpec{
		Name:            "python",
		Type:            model.RepoTypeVirtual,
		Protocol:        model.ProtocolPyPI,
		Members:         []string{"internal"},
		LifecycleStages: []string{"dev"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateRepository_AllowsLifecycleOnHosted(t *testing.T) {
	err := reconcile.ValidateRepository(reconcile.RepositorySpec{
		Name:            "internal",
		Type:            model.RepoTypeHosted,
		Protocol:        model.ProtocolPyPI,
		LifecycleStages: []string{"prod"},
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidateRepository_AllowsProxyWithoutLifecycle(t *testing.T) {
	err := reconcile.ValidateRepository(reconcile.RepositorySpec{
		Name:        "pypi-public",
		Type:        model.RepoTypeProxy,
		Protocol:    model.ProtocolPyPI,
		UpstreamURL: "https://pypi.org/simple",
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidateRepository_RejectsCredentialsRefUntilSecretsExist(t *testing.T) {
	cases := []struct {
		name string
		spec reconcile.RepositorySpec
	}{
		{
			name: "Proxy",
			spec: reconcile.RepositorySpec{
				Name: "docker/fancy", Type: model.RepoTypeProxy, Protocol: model.ProtocolOCI,
				UpstreamURL: "https://whatever.example", CredentialsRef: "fancysecret",
			},
		},
		{
			name: "Hosted",
			spec: reconcile.RepositorySpec{
				Name: "internal", Type: model.RepoTypeHosted, Protocol: model.ProtocolPyPI,
				CredentialsRef: "fancysecret",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := reconcile.ValidateRepository(tc.spec)
			if !errors.Is(err, reconcile.ErrCredentialsUnsupported) {
				t.Fatalf("want ErrCredentialsUnsupported, got %v", err)
			}
			if !strings.Contains(err.Error(), "ADR 15") {
				t.Fatalf("error should point at ADR 15: %v", err)
			}
		})
	}
}
