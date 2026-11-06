#!/usr/bin/env bash
# Controlled diagnostics and exit status for the gate harness regression tests.
if grep -q "func Good" fixture.go 2>/dev/null || grep -q "CheckState" fixture.go 2>/dev/null; then
    exit 0
fi

if grep -q "Signal" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: p.Signal: Direct process signals are prohibited"
elif grep -q "Process.Kill" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: cmd.Process.Kill: Direct process signals are prohibited"
elif grep -q "syscall.Kill" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: syscall.Kill: Direct syscall signals and setsid are prohibited"
elif grep -q "unix.Kill" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: unix.Kill: Direct process signals are prohibited"
elif grep -q "syscall.Setsid" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: syscall.Setsid: Direct syscall signals and setsid are prohibited"
elif grep -q "CommandContext" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: exec.CommandContext: defaults to Process.Kill"
elif grep -q "WaitDelay" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: cmd.WaitDelay: Nonzero Cmd.WaitDelay can implicitly kill"
elif grep -q "os.Remove" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: Error return value of \`os.Remove\` is not checked"
elif grep -q "f.Sync" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: Error return value of \`f.Sync\` is not checked"
elif grep -q "switch s" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: missing cases in switch of type State: Unknown"
elif grep -Fq 'response, err := http.Get(' fixture.go 2>/dev/null; then
    echo "fixture.go:4:30: response body must be closed"
elif grep -Fq 'rows, err := db.Query(' fixture.go 2>/dev/null; then
    if grep -Fq 'defer rows.Close()' fixture.go 2>/dev/null; then
        echo "fixture.go:4:26: rows.Err must be checked"
    else
        echo "fixture.go:4:26: Rows/Stmt/NamedStmt was not closed"
    fi
elif grep -Fq 'io.EOF = nil' fixture.go 2>/dev/null; then
    echo "fixture.go:4:5: reassigning variable EOF in other package io"
elif grep -Fq '//go:generete ' fixture.go 2>/dev/null; then
    echo "fixture.go:2:1: compiler directive unrecognized: //go:generete"
elif grep -Fq 'slog.Info("fixture", "key")' fixture.go 2>/dev/null; then
    echo "fixture.go:4:26: odd number of arguments passed as key-value pairs for logging"
elif grep -Fq 'return value * time.Second' fixture.go 2>/dev/null; then
    echo "fixture.go:4:12: Multiplication of durations"
fi
exit "${STUB_LINT_EXIT:-1}"
