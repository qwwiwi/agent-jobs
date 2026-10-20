//go:build !darwin

package jobs

// sysctlProc and sysctlGroup: no sysctl process identity outside macOS (Linux reads /proc), so a
// caller refuses to signal; variables so tests can stand in for them.
var (
	sysctlProc  = func(int) (sysctlInfo, error) { return sysctlInfo{}, errNoSysctlIdentity }
	sysctlGroup = func(int) ([]int, error) { return nil, errNoSysctlIdentity }
)
