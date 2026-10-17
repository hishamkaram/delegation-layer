// Package policy checks collected source facts without reading files or running tools.
package policy

// Limits names the repository's maintained-source budgets.
type Limits struct {
	Code          int `json:"code"`
	Tests         int `json:"tests"`
	Documentation int `json:"documentation"`
	Function      int `json:"function"`
}

// Manifest is the sole owner of subsystem boundaries and source budgets.
type Manifest struct {
	Version     int         `json:"version"`
	Module      string      `json:"module"`
	Limits      Limits      `json:"limits"`
	ModuleFiles []string    `json:"module_files"`
	Migration   string      `json:"migration"`
	Generated   []Generated `json:"generated"`
	Modules     []Module    `json:"-"`
}

// Module describes one canonical import boundary, including explicit test roles.
type Module struct {
	Name    string   `json:"name"`
	Owner   string   `json:"owner"`
	Layer   string   `json:"layer"`
	Imports []string `json:"imports"`
}

type Generated struct {
	Path      string `json:"path"`
	Generator string `json:"generator"`
	Source    string `json:"source"`
}

type Dependency struct {
	Target string `json:"target"`
	Line   int    `json:"line"`
}

type Function struct {
	Name   string `json:"name"`
	Line   int    `json:"line"`
	Length int    `json:"length"`
}

// Call counts statically known positional values; dynamic starred arity is unknown.
type Call struct {
	Name                string `json:"name"`
	Line                int    `json:"line"`
	PositionalArguments int    `json:"positional_arguments"`
}

// Source contains observations only; collectors cannot grant policy exceptions.
type Source struct {
	Path      string       `json:"path"`
	Module    string       `json:"module"`
	Kind      string       `json:"kind"`
	SHA256    string       `json:"sha256"`
	Lines     int          `json:"lines"`
	Imports   []Dependency `json:"imports"`
	Functions []Function   `json:"functions"`
	Calls     []Call       `json:"calls"`
	Effects   []Effect     `json:"effects"`
}

type Effect struct {
	Name string `json:"name"`
	Line int    `json:"line"`
}

type Finding struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Kind   string `json:"kind"`
	Symbol string `json:"symbol,omitempty"`
	Detail string `json:"detail"`
	Actual int    `json:"actual,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// Migration permits only an unchanged, explicitly owned baseline finding.
type Migration struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Symbol string `json:"symbol,omitempty"`
	Detail string `json:"detail"`
	SHA256 string `json:"sha256"`
	Owner  string `json:"owner"`
	Stage  string `json:"stage"`
}
