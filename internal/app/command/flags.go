package command

import (
	"flag"
	"fmt"
	"strings"
)

// orderFlags permits documented ID --json placement without silently accepting
// unknown options as positional values. A flag's following value remains literal.
func orderFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var options, positional []string
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, _, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := fs.Lookup(name)
		if f == nil {
			return nil, fmt.Errorf("unknown flag %q", arg)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate flag --%s", name)
		}
		seen[name] = true
		options = append(options, arg)
		boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
		if assigned || (ok && boolean.IsBoolFlag()) {
			continue
		}
		i++
		if i == len(args) {
			return nil, fmt.Errorf("missing value for --%s", name)
		}
		options = append(options, args[i])
	}
	options = append(options, "--")
	return append(options, positional...), nil
}
