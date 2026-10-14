package gateway

import "time"

// SetResumeTimings shortens the resuming page's waits for tests.
func SetResumeTimings(delay, check time.Duration) (restore func()) {
	previousDelay, previousCheck := resumePageDelay, resumeCheckTimeout
	resumePageDelay, resumeCheckTimeout = delay, check
	return func() { resumePageDelay, resumeCheckTimeout = previousDelay, previousCheck }
}
