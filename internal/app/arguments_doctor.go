package app

import (
	"errors"
	"flag"
	"io"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

func parseDoctorArguments(a Arguments, rest []string) (Arguments, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.Root, "root", "", "absolute state root")
	fs.BoolVar(&a.JSON, "json", false, "emit versioned JSON")
	fs.StringVar(&a.Provider, "provider", "", "provider profile")
	fs.StringVar(&a.Cwd, "cwd", "", "absolute working directory")
	fs.StringVar(&a.Config.Model, "model", "", "requested model")
	fs.StringVar(&a.Config.Effort, "effort", "", "requested effort")
	fs.StringVar(&a.ReviewTarget, "review-target", "", "Codex review target")
	ordered, err := orderFlags(fs, rest)
	if err != nil {
		return Arguments{}, err
	}
	if err = fs.Parse(ordered); err != nil {
		return Arguments{}, err
	}
	if err = validateNoPositional("doctor", fs.Args()); err != nil {
		return Arguments{}, err
	}
	if err = validateDoctorArguments(&a, fs); err != nil {
		return Arguments{}, err
	}
	return a, nil
}

func validateDoctorArguments(a *Arguments, fs *flag.FlagSet) error {
	if a.Root != "" && !filepath.IsAbs(a.Root) {
		return errors.New("doctor --root must be absolute")
	}
	if (a.Provider == "") != (a.Cwd == "") {
		return errors.New("doctor requires --provider and --cwd together")
	}
	dependent := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "model" || f.Name == "effort" || f.Name == "review-target" {
			dependent = true
		}
	})
	if a.Provider == "" && dependent {
		return errors.New("doctor model, effort and review-target require --provider and --cwd")
	}
	if a.Provider != "" {
		if err := config.ValidateProvider(a.Provider); err != nil {
			return err
		}
		if !filepath.IsAbs(a.Cwd) {
			return errors.New("doctor --cwd must be absolute")
		}
		if err := a.applyReviewTarget(); err != nil {
			return err
		}
	}
	a.Config.Permission = config.ModeReadOnly
	return nil
}
