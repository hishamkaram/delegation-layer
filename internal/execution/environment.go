package execution

// InheritedCommandEnvironment returns the explicit environment for a command
// in directory that inherits captured: a copy of captured followed by the
// host's implicit working directory entries, as exec.Cmd adds them when it
// inherits the process environment. It never reads the process environment,
// never aliases captured, and never returns nil.
func InheritedCommandEnvironment(captured []string, directory string) []string {
	entries := implicitWorkingDirectoryEntries(directory)
	environment := make([]string, 0, len(captured)+len(entries))
	return append(append(environment, captured...), entries...)
}
