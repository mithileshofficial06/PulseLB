// Package perf asks the OS not to throttle this process.
package perf

import (
	"syscall"
	"unsafe"
)

// Windows 11 applies "EcoQoS" power throttling to processes that are not in
// the foreground: on hybrid CPUs it parks them on efficiency cores and
// coalesces their timers. For a load balancer that shows up as multi-second
// latency stalls while the CPU is mostly idle (we measured p99 jumping from
// ~20ms to over 1s at 5k req/s as soon as another app had focus). Opting out
// is what Windows recommends for latency-sensitive work.
const (
	processPowerThrottling          = 4   // PROCESS_INFORMATION_CLASS
	throttlingCurrentVersion        = 1   // PROCESS_POWER_THROTTLING_CURRENT_VERSION
	throttlingExecutionSpeed        = 0x1 // PROCESS_POWER_THROTTLING_EXECUTION_SPEED
	throttlingIgnoreTimerResolution = 0x4 // PROCESS_POWER_THROTTLING_IGNORE_TIMER_RESOLUTION
)

type powerThrottlingState struct {
	Version     uint32
	ControlMask uint32
	StateMask   uint32
}

var setProcessInformation = syscall.NewLazyDLL("kernel32.dll").NewProc("SetProcessInformation")

// DisablePowerThrottling opts the current process out of EcoQoS execution
// speed throttling and timer-resolution throttling.
func DisablePowerThrottling() error {
	if err := setProcessInformation.Find(); err != nil {
		return err // older Windows: nothing to disable
	}
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return err
	}
	st := powerThrottlingState{
		Version:     throttlingCurrentVersion,
		ControlMask: throttlingExecutionSpeed | throttlingIgnoreTimerResolution,
		StateMask:   0, // controlled, and turned off
	}
	r, _, e := setProcessInformation.Call(uintptr(h), processPowerThrottling, uintptr(unsafe.Pointer(&st)), unsafe.Sizeof(st))
	if r == 0 {
		return e
	}
	return nil
}
