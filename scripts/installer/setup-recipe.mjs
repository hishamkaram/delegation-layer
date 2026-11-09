export function shellQuote(value) {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

export function printFirstTask({ executable, providerArguments, report }) {
  if (providerArguments.length === 0) {
    report("Next: ask your lead agent to choose a provider and workspace, prepare a finite read-only brief, then use Delegate dispatch/status/collect and inspect terminal evidence. Setup has not launched a task.");
    return;
  }
  const command = shellQuote(executable);
  const options = providerArguments.map(shellQuote).join(" ");
  report("First-task example (review it, then run explicitly; no task has been launched):");
  report("1. Run this shell block to prepare one brief and dispatch once:");
  report("```sh");
  report('if TASK_FILES="$(mktemp -d)"; then');
  report('printf "Task files: %s\\n" "$TASK_FILES"');
  report('BRIEF="$TASK_FILES/brief"');
  report('RESPONSE="$TASK_FILES/response.json"');
  report('if cat >"$BRIEF" <<\'DELEGATE_BRIEF\'');
  report("Inspect the workspace and summarize its structure. Do not edit files.");
  report("You are the assigned worker; perform this task without delegating it.");
  report("DELEGATE_BRIEF");
  report("then");
  report(`if ${command} dispatch ${options} --brief "$BRIEF" --permission read-only --budget 30m --json >"$RESPONSE"; then dispatch_exit=0; else dispatch_exit=$?; fi`);
  report('cat "$RESPONSE"');
  report('printf "\\nDispatch exit: %s; saved response: %s\\n" "$dispatch_exit" "$RESPONSE"');
  report("else");
  report('printf "Brief preparation failed; no task dispatched.\\n" >&2');
  report('if rm -r -- "$TASK_FILES"; then unset TASK_FILES BRIEF RESPONSE; else printf "Could not remove task files: %s\\n" "$TASK_FILES" >&2; fi');
  report("fi");
  report("else");
  report('printf "Could not create task files; no task dispatched.\\n" >&2');
  report("fi");
  report("```");
  report("2. Only if task_record is created: read its exact task_id, then enter it below. For not_created or unknown, follow the saved response; never redispatch to recover output.");
  report("```sh");
  report('TASK=; printf "Created task_id: "; IFS= read -r TASK');
  report("```");
  report("3. After entering that exact created task_id, run this separate observation block:");
  report("```sh");
  report(`${command} status "$TASK" --json`);
  report(`${command} collect "$TASK" --json`);
  report("```");
  report("4. To stop this same created task explicitly, run this separate command:");
  report("```sh");
  report(`${command} cancel "$TASK" --json`);
  report("```");
  report("Inspect terminal result and artifact evidence; collect output is not proof of provider success. Keep the saved response and brief until closure is confirmed, then remove the owned TASK_FILES directory.");
}
