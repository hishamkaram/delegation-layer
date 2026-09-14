package pueue

// Config is the supported pueue 4.0.4 base configuration. Profiles are validated
// during parsing but never selected: delegate supplies no profile option.
type Config struct {
	Client ClientConfig
	Daemon DaemonConfig
	Shared SharedConfig
}

type ClientConfig struct {
	RestartInPlace            bool
	ReadLocalLogs             bool
	ShowConfirmationQuestions bool
	EditMode                  string
	ShowExpandedAliases       bool
	DarkMode                  bool
	MaxStatusLines            *uint64
	StatusTimeFormat          string
	StatusDatetimeFormat      string
}

type DaemonConfig struct {
	PauseGroupOnFailure bool
	PauseAllOnFailure   bool
	CompressStateFile   bool
	Callback            *string
	EnvVars             map[string]string
	CallbackLogLines    uint64
	ShellCommand        *[]string
}

type SharedConfig struct {
	PueueDirectory        *string
	RuntimeDirectory      *string
	AliasFile             *string
	UseUnixSocket         bool
	UnixSocketPath        *string
	UnixSocketPermissions *uint32
	Host                  string
	Port                  string
	PIDPath               *string
	DaemonCert            *string
	DaemonKey             *string
	SharedSecretPath      *string
}

type rawSettings struct {
	Client   *rawClient           `yaml:"client"`
	Daemon   *rawDaemon           `yaml:"daemon"`
	Shared   *rawShared           `yaml:"shared"`
	Profiles map[string]rawNested `yaml:"profiles"`
}

type rawNested struct {
	Client *rawClient `yaml:"client"`
	Daemon *rawDaemon `yaml:"daemon"`
	Shared *rawShared `yaml:"shared"`
}

type rawClient struct {
	RestartInPlace            *bool   `yaml:"restart_in_place"`
	ReadLocalLogs             *bool   `yaml:"read_local_logs"`
	ShowConfirmationQuestions *bool   `yaml:"show_confirmation_questions"`
	EditMode                  *string `yaml:"edit_mode"`
	ShowExpandedAliases       *bool   `yaml:"show_expanded_aliases"`
	DarkMode                  *bool   `yaml:"dark_mode"`
	MaxStatusLines            *uint64 `yaml:"max_status_lines" nullable:"true"`
	StatusTimeFormat          *string `yaml:"status_time_format"`
	StatusDatetimeFormat      *string `yaml:"status_datetime_format"`
}

type rawDaemon struct {
	PauseGroupOnFailure *bool             `yaml:"pause_group_on_failure"`
	PauseAllOnFailure   *bool             `yaml:"pause_all_on_failure"`
	CompressStateFile   *bool             `yaml:"compress_state_file"`
	Callback            *string           `yaml:"callback" nullable:"true"`
	EnvVars             map[string]string `yaml:"env_vars"`
	CallbackLogLines    *uint64           `yaml:"callback_log_lines"`
	ShellCommand        *[]string         `yaml:"shell_command" nullable:"true"`
}

type rawShared struct {
	PueueDirectory        *string `yaml:"pueue_directory" nullable:"true"`
	RuntimeDirectory      *string `yaml:"runtime_directory" nullable:"true"`
	AliasFile             *string `yaml:"alias_file" nullable:"true"`
	UseUnixSocket         *bool   `yaml:"use_unix_socket"`
	UnixSocketPath        *string `yaml:"unix_socket_path" nullable:"true"`
	UnixSocketPermissions *uint32 `yaml:"unix_socket_permissions" nullable:"true"`
	Host                  *string `yaml:"host"`
	Port                  *string `yaml:"port"`
	PIDPath               *string `yaml:"pid_path" nullable:"true"`
	DaemonCert            *string `yaml:"daemon_cert" nullable:"true"`
	DaemonKey             *string `yaml:"daemon_key" nullable:"true"`
	SharedSecretPath      *string `yaml:"shared_secret_path" nullable:"true"`
}

func valueOr[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

func clientDefaults(r *rawClient) ClientConfig {
	if r == nil {
		r = &rawClient{}
	}
	return ClientConfig{
		RestartInPlace:            valueOr(r.RestartInPlace, false),
		ReadLocalLogs:             valueOr(r.ReadLocalLogs, true),
		ShowConfirmationQuestions: valueOr(r.ShowConfirmationQuestions, false),
		EditMode:                  valueOr(r.EditMode, "toml"),
		ShowExpandedAliases:       valueOr(r.ShowExpandedAliases, false),
		DarkMode:                  valueOr(r.DarkMode, false),
		MaxStatusLines:            r.MaxStatusLines,
		StatusTimeFormat:          valueOr(r.StatusTimeFormat, "%H:%M:%S"),
		StatusDatetimeFormat:      valueOr(r.StatusDatetimeFormat, "%Y-%m-%d\n%H:%M:%S"),
	}
}

func daemonDefaults(r *rawDaemon) DaemonConfig {
	if r == nil {
		r = &rawDaemon{}
	}
	envs := r.EnvVars
	if envs == nil {
		envs = map[string]string{}
	}
	return DaemonConfig{
		PauseGroupOnFailure: valueOr(r.PauseGroupOnFailure, false),
		PauseAllOnFailure:   valueOr(r.PauseAllOnFailure, false),
		CompressStateFile:   valueOr(r.CompressStateFile, false),
		Callback:            r.Callback,
		EnvVars:             envs,
		CallbackLogLines:    valueOr(r.CallbackLogLines, uint64(10)),
		ShellCommand:        r.ShellCommand,
	}
}

func sharedDefaults(r *rawShared) SharedConfig {
	if r == nil {
		permissions := uint32(0o700)
		r = &rawShared{UnixSocketPermissions: &permissions}
	}
	return SharedConfig{
		PueueDirectory: r.PueueDirectory, RuntimeDirectory: r.RuntimeDirectory,
		AliasFile: r.AliasFile, UseUnixSocket: valueOr(r.UseUnixSocket, true),
		UnixSocketPath: r.UnixSocketPath, UnixSocketPermissions: r.UnixSocketPermissions,
		Host: valueOr(r.Host, "127.0.0.1"), Port: valueOr(r.Port, "6924"),
		PIDPath: r.PIDPath, DaemonCert: r.DaemonCert, DaemonKey: r.DaemonKey,
		SharedSecretPath: r.SharedSecretPath,
	}
}
