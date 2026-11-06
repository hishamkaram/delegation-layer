//go:build darwin

package lifetime

func independentFixtureFDDirectory() string { return "/dev/fd" }
