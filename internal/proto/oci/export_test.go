package oci

// SetReferrersPageSize lowers the referrers page size for a test and returns
// the function that restores it.
func SetReferrersPageSize(n int32) (restore func()) {
	old := referrersPageSize
	referrersPageSize = n
	return func() { referrersPageSize = old }
}
