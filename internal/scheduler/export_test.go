package scheduler

// NewForTest exposes the sleep-injectable constructor to black-box tests
// so cold-start backoff can be exercised without wall-clock delays. The
// `_test.go` suffix keeps it out of production builds.
var NewForTest = newScheduler
