package main

// shellCall records one shellRunner invocation for assertions in the
// platform service tests.
type shellCall struct {
	name string
	args []string
}
