//go:build !windows

package perf

// DisablePowerThrottling is a no-op outside Windows.
func DisablePowerThrottling() error { return nil }
