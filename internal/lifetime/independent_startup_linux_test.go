//go:build linux

package lifetime

func independentFixtureFDDirectory() string { return "/proc/self/fd" }
