// Package reconcile validates repository specifications at write time.
//
// Invariant I4 lives here: lifecycle configuration on a proxy (or virtual)
// repository is rejected before any row is written. M7's reconciler will call
// the same ValidateRepository; the management API already does.
package reconcile

import (
	"errors"
	"fmt"

	"github.com/spj/sindri/internal/model"
)

// ErrCredentialsUnsupported means a repository names a credentials_ref, but
// this build has no source for secrets. M7 adds one (ADR 15) and replaces this
// rejection with a check that the reference exists.
var ErrCredentialsUnsupported = errors.New("upstream credentials are not supported yet")

// RepositorySpec is the subset of a repository declaration that validation
// needs. It is deliberately independent of YAML parsing so M5 can enforce I4
// before the config-as-code surface arrives in M7.
type RepositorySpec struct {
	Name            string
	Type            model.RepoType
	Protocol        model.Protocol
	UpstreamURL     string
	CredentialsRef  string
	Members         []string
	LifecycleStages []string
}

// ValidateRepository checks a repository specification. A proxy or virtual
// repository that carries lifecycle stages fails (invariant I4).
//
// Any credentials_ref fails too, for now. Nothing yet supplies secrets, so a
// reference would be accepted here and then break the first pull; and while
// reads are anonymous, a credentialed proxy would republish a private upstream
// to anyone who can reach Sindri. Both end in M7 (ADR 15).
func ValidateRepository(spec RepositorySpec) error {
	if spec.CredentialsRef != "" {
		return fmt.Errorf("%w: repository %q names credentials_ref %q, but secrets cannot be configured until M7 (ADR 15)",
			ErrCredentialsUnsupported, spec.Name, spec.CredentialsRef)
	}
	if len(spec.LifecycleStages) == 0 {
		return nil
	}
	switch spec.Type {
	case model.RepoTypeProxy, model.RepoTypeVirtual:
		return fmt.Errorf("lifecycle stages are not allowed on a %s repository %q (invariant I4)",
			spec.Type, spec.Name)
	case model.RepoTypeHosted, "":
		return nil
	default:
		return fmt.Errorf("unknown repository type %q", spec.Type)
	}
}
