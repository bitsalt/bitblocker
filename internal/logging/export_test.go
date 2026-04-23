package logging

// NewForTest exposes the writer-parameterized constructor to black-box
// tests. The file suffix `_test.go` keeps it out of production builds.
var NewForTest = newWithWriter
