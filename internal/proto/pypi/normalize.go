package pypi

import "github.com/spj/sindri/internal/registry"

// NormalizeName folds a package name per PEP 503.
func NormalizeName(name string) string {
	return registry.NormalizePackageName(name)
}
