import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { chmod, cp, mkdir, mkdtemp, open, readFile, readdir, realpath, rm, stat, symlink, unlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { runInstaller as runInteractiveInstaller } from "./installer/application.mjs";
import { createPickerState, transitionPicker } from "./installer/picker-state.mjs";
import { countRoutingEvents, createRoutingPlan } from "./installer/routing-rules.mjs";
import { applyRouting } from "./installer/routing-store.mjs";
import { pickOptions } from "./installer/terminal.mjs";

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const installer = resolve(process.argv[2] ?? join(repositoryRoot, "scripts/install-agent-integration.mjs"));
const root = await realpath(await mkdtemp(join(tmpdir(), "delegate routing 'test-")));
const home = join(root, "home");
const project = join(root, "project");
const environment = { ...process.env, DELEGATION_LAYER_HOME: home, CLAUDE_CONFIG_DIR: "", CODEX_HOME: "" };
function run(args, extra = {}) {
  return spawnSync(process.execPath, [installer, ...args], { cwd: project, env: { ...environment, ...extra }, encoding: "utf8" });
}
function success(result) { assert.equal(result.status, 0, result.stderr); return result; }
const install = ["install", "--scope", "global", "--harness", "claude,codex", "--yes", "--routing", "default"];
const configPath = join(home, ".claude", "settings.json");
const codexPath = join(home, ".codex", "hooks.json");
const readJSON = async (path) => JSON.parse(await readFile(path, "utf8"));

class FakeInput extends EventEmitter {
  constructor(eventSequences = []) {
    super();
    this.isTTY = true;
    this.eventSequences = eventSequences;
    this.rawModes = [];
    this.resumeCount = 0;
    this.pauseCount = 0;
  }

  setRawMode(enabled) { this.rawModes.push(enabled); }
  resume() {
    const events = this.eventSequences[this.resumeCount] ?? [];
    this.resumeCount += 1;
    queueMicrotask(() => events.forEach(([character, key]) => this.emit("keypress", character, key)));
  }
  pause() { this.pauseCount += 1; }
}

class FakeOutput {
  constructor({ throwAtWrite } = {}) {
    this.isTTY = true;
    this.writes = [];
    this.throwAtWrite = throwAtWrite;
  }

  write(value) {
    this.writes.push(value);
    if (this.writes.length === this.throwAtWrite) throw new Error("render failed");
    return true;
  }
}

function testRoutingRules() {
  const target = join(root, "policy target ' quoted");
  const config = {
    unrelated: { keep: true },
    hooks: {
      SessionStart: [{ matcher: "startup", hooks: [{ type: "command", command: "echo user" }] }],
    },
  };
  const before = JSON.stringify(config);
  const enabled = createRoutingPlan({
    name: "claude",
    path: join(root, "settings.json"),
    target,
    before,
    config,
    mode: 0o640,
    action: "enable",
  });
  const enabledConfig = JSON.parse(enabled.after);
  assert.equal(enabled.status, "configured");
  assert.equal(countRoutingEvents(enabledConfig, target), 2);
  assert.deepEqual(enabledConfig.unrelated, config.unrelated);
  assert.deepEqual(enabledConfig.hooks.SessionStart[0], config.hooks.SessionStart[0]);
  assert.equal(JSON.stringify(config), before, "routing planning must not mutate its input");

  const repeated = createRoutingPlan({
    name: "claude",
    path: enabled.path,
    target,
    before: enabled.after,
    config: enabledConfig,
    mode: enabled.mode,
    action: "enable",
  });
  assert.equal(repeated.after, enabled.after, "enabling existing routes must preserve exact bytes");

  const incompleteConfig = {
    ...enabledConfig,
    hooks: { ...enabledConfig.hooks, SubagentStart: undefined },
  };
  delete incompleteConfig.hooks.SubagentStart;
  const incomplete = createRoutingPlan({
    name: "claude",
    path: enabled.path,
    target,
    before: JSON.stringify(incompleteConfig),
    config: incompleteConfig,
    mode: enabled.mode,
    action: "status",
    assetsPresent: false,
  });
  assert.equal(incomplete.status, "incomplete");

  const disabled = createRoutingPlan({
    name: "claude",
    path: enabled.path,
    target,
    before: enabled.after,
    config: enabledConfig,
    mode: enabled.mode,
    action: "disable",
  });
  const disabledConfig = JSON.parse(disabled.after);
  assert.equal(disabled.status, "disabled");
  assert.equal(countRoutingEvents(disabledConfig, target), 0);
  assert.deepEqual(disabledConfig.hooks.SessionStart, config.hooks.SessionStart);
}

function testPickerState() {
  let state = createPickerState({ initialValues: ["claude"], multiple: true });
  state = transitionPicker(state, "down", ["claude", "codex"]).state;
  state = transitionPicker(state, "toggle", ["claude", "codex"]).state;
  assert.deepEqual(state.selectedValues, ["claude", "codex"]);
  state = transitionPicker(state, "none", ["claude", "codex"]).state;
  const empty = transitionPicker(state, "enter", ["claude", "codex"]);
  assert.equal(empty.done, false);
  assert.match(empty.state.error, /Select at least one harness/);
  const selected = transitionPicker(empty.state, "all", ["claude", "codex"]);
  assert.deepEqual(transitionPicker(selected.state, "enter", ["claude", "codex"]).value, ["claude", "codex"]);

  const single = createPickerState({});
  const wrapped = transitionPicker(single, "up", ["global", "project"]);
  assert.equal(wrapped.state.index, 1);
  assert.equal(transitionPicker(wrapped.state, "enter", ["global", "project"]).value, "project");
}

async function testTerminalCleanup() {
  const input = new FakeInput([[
    ["", { name: "down" }],
    ["", { name: "enter" }],
  ]]);
  const output = new FakeOutput();
  const selected = await pickOptions({
    initialValues: ["global"],
    options: [{ value: "global", label: "Global" }, { value: "project", label: "Project" }],
    title: "Scope",
    input,
    output,
  });
  assert.equal(selected, "project");
  assert.deepEqual(input.rawModes, [true, false]);
  assert.equal(input.resumeCount, 1);
  assert.equal(input.pauseCount, 1);
  assert.equal(input.listenerCount("keypress"), 0);
  assert.ok(output.writes.some((write) => write.includes("\u001b[?25l")));
  assert.ok(output.writes.some((write) => write.includes("\u001b[?25h")));

  const failedInput = new FakeInput();
  const failedOutput = new FakeOutput({ throwAtWrite: 2 });
  await assert.rejects(
    pickOptions({
      options: [{ value: "one", label: "One" }],
      title: "Render failure",
      input: failedInput,
      output: failedOutput,
    }),
    /render failed/,
  );
  assert.deepEqual(failedInput.rawModes, [true, false]);
  assert.equal(failedInput.pauseCount, 1);
  assert.equal(failedInput.listenerCount("keypress"), 0);
  assert.ok(failedOutput.writes.at(-1).includes("\u001b[?25h"));
}

async function replacementPlan(name) {
  const path = join(root, name, "settings.json");
  await mkdir(dirname(path), { recursive: true });
  const before = '{"previous":true}';
  await writeFile(path, before);
  return { path, before, after: '{"updated":true}', mode: 0o600 };
}

function containsError(error, expected) {
  if (error === expected) return true;
  if (error instanceof AggregateError && error.errors.some((nested) => containsError(nested, expected))) return true;
  return error instanceof Error && error.cause !== undefined && containsError(error.cause, expected);
}

async function testPrimaryAndCleanupFailures() {
  const plan = await replacementPlan("primary-and-cleanup");
  const primary = new Error("staged write failed");
  const stageClose = new Error("staged close failed");
  const stageRemove = new Error("staged removal failed");
  const lockClose = new Error("lock close failed");
  const lockRemove = new Error("lock removal failed");
  const attempted = [];
  await assert.rejects(applyRouting([plan], {
    open: async (path, ...args) => {
      const file = await open(path, ...args);
      if (path.includes("-stage-")) return {
        writeFile: async () => { throw primary; },
        sync: () => file.sync(),
        close: async () => { attempted.push("stage close"); await file.close(); throw stageClose; },
      };
      if (path.endsWith(".lock")) return {
        close: async () => { attempted.push("lock close"); await file.close(); throw lockClose; },
      };
      return file;
    },
    unlink: async (path) => {
      await unlink(path);
      if (path.includes("-stage-")) { attempted.push("stage remove"); throw stageRemove; }
      attempted.push("lock remove");
      throw lockRemove;
    },
  }), (error) => {
    for (const failure of [primary, stageClose, stageRemove, lockClose, lockRemove]) {
      assert.ok(containsError(error, failure), `missing original failure: ${failure.message}`);
      assert.ok(error.message.includes(failure.message));
    }
    return true;
  });
  assert.deepEqual(attempted, ["stage close", "stage remove", "lock close", "lock remove"]);
  assert.equal(await readFile(plan.path, "utf8"), plan.before);
  assert.ok((await readdir(dirname(plan.path))).every((name) => !name.includes("-stage-") && !name.endsWith(".lock")));
}

async function testBackupFailuresPreserved() {
  const plan = await replacementPlan("backup-failures");
  const writeFailure = new Error("backup write failed");
  const closeFailure = new Error("backup close failed");
  await assert.rejects(applyRouting([plan], {
    open: async (path, ...args) => {
      const file = await open(path, ...args);
      if (!path.includes("-backup-")) return file;
      return {
        writeFile: async () => { throw writeFailure; },
        sync: () => file.sync(),
        close: async () => { await file.close(); throw closeFailure; },
      };
    },
  }), (error) => containsError(error, writeFailure) && containsError(error, closeFailure));
  assert.equal(await readFile(plan.path, "utf8"), plan.before);
  await assert.rejects(readFile(plan.path + ".delegation-layer.lock"), { code: "ENOENT" });
}

async function testCleanupFailureRollsBackCommittedChange() {
  const plan = await replacementPlan("committed-cleanup-failure");
  const cleanupFailure = new Error("committed lock close failed");
  let failClose = true;
  await assert.rejects(applyRouting([plan], {
    open: async (path, ...args) => {
      const file = await open(path, ...args);
      if (!path.endsWith(".lock")) return file;
      return {
        close: async () => {
          await file.close();
          if (failClose) { failClose = false; throw cleanupFailure; }
        },
      };
    },
  }), (error) => containsError(error, cleanupFailure) && /rolled back/.test(error.message));
  assert.equal(await readFile(plan.path, "utf8"), plan.before, "a committed change must roll back despite cleanup failure");
  assert.ok((await readdir(dirname(plan.path))).every((name) => !name.includes("-stage-") && !name.endsWith(".lock")));
}

async function testRollbackRetainsOriginalAndRollbackErrors() {
  const first = await replacementPlan("rollback-errors-first");
  const second = await replacementPlan("rollback-errors-second");
  const third = await replacementPlan("rollback-errors-third");
  const occupied = Object.assign(new Error("lock already occupied"), { code: "EEXIST" });
  const rollbackFailure = new Error("rollback lock failed");
  let secondLocks = 0;
  await assert.rejects(applyRouting([first, second, third], {
    open: async (path, ...args) => {
      if (path === third.path + ".delegation-layer.lock") throw occupied;
      if (path === second.path + ".delegation-layer.lock" && ++secondLocks === 2) throw rollbackFailure;
      return open(path, ...args);
    },
  }), (error) => {
    assert.ok(containsError(error, occupied), "original lock failure cause must remain available");
    assert.ok(containsError(error, rollbackFailure), "rollback failure object must remain available");
    assert.match(error.message, /rollback needs attention/);
    assert.match(error.message, /rollback lock failed/);
    return true;
  });
  assert.equal(await readFile(first.path, "utf8"), first.before, "rollback must continue after another rollback failed");
  assert.equal(await readFile(second.path, "utf8"), second.after);
  assert.equal(await readFile(third.path, "utf8"), third.before);
}

async function testLazyHomeResolution(cwd, root) {
  const homeCalls = [];
  const resolveHome = () => {
    homeCalls.push("called");
    throw new Error("home lookup failed");
  };
  const invoke = (argumentsList, { output = new FakeOutput(), errorOutput = new FakeOutput(), installCliMain } = {}) =>
    runInteractiveInstaller({
      argumentsList,
      packageRoot: repositoryRoot,
      cwd,
      resolveHome,
      environment: { PATH: "" },
      platform: "linux",
      input: new FakeInput(),
      output,
      errorOutput,
      installCliMain: installCliMain ?? (async () => {}),
    });

  const installErrors = new FakeOutput();
  assert.equal(await invoke(
    ["install", "--scope", "global", "--harness", "claude", "--yes", "--routing", "skill-only"],
    { errorOutput: installErrors },
  ), 1);
  assert.deepEqual(installErrors.writes, ["Error: home lookup failed\n"]);
  assert.equal(homeCalls.length, 1);

  const routingErrors = new FakeOutput();
  assert.equal(await invoke(["routing", "status", "--harness", "claude"], { errorOutput: routingErrors }), 1);
  assert.deepEqual(routingErrors.writes, ["Error: home lookup failed\n"]);
  assert.equal(homeCalls.length, 2);

  const usageErrors = new FakeOutput();
  assert.equal(await invoke(["install", "--routing", "invalid"], { errorOutput: usageErrors }), 2);
  assert.equal(homeCalls.length, 2, "invalid arguments must not resolve home");

  const helpOutput = new FakeOutput();
  assert.equal(await invoke(["install", "--help"], { output: helpOutput }), 0);
  assert.match(helpOutput.writes.join(""), /Usage:/);
  assert.equal(homeCalls.length, 2, "help must not resolve home");

  const target = join(root, "lazy-home-target");
  assert.equal(await invoke(["--target", target]), 0);
  await readFile(join(target, "SKILL.md"));
  assert.equal(homeCalls.length, 2, "legacy target installation must not resolve home");

  let installCliCalls = 0;
  assert.equal(await invoke(["install-cli", "--help"], {
    installCliMain: async () => { installCliCalls += 1; },
  }), 0);
  assert.equal(installCliCalls, 1);
  assert.equal(homeCalls.length, 2, "install-cli must not resolve home");
}

try {
  testRoutingRules();
  testPickerState();
  await testTerminalCleanup();
  await testPrimaryAndCleanupFailures();
  await testBackupFailuresPreserved();
  await testCleanupFailureRollsBackCommittedChange();
  await testRollbackRetainsOriginalAndRollbackErrors();
  await mkdir(join(home, ".claude"), { recursive: true });
  await mkdir(project);
  await testLazyHomeResolution(project, root);
  const interactiveProject = join(root, "interactive-project");
  const interactiveHome = join(root, "interactive-home");
  await mkdir(join(interactiveProject, ".claude"), { recursive: true });
  await mkdir(interactiveHome, { recursive: true });
  const interactiveInput = new FakeInput([
    [["", { name: "down" }], ["", { name: "enter" }]],
    [["", { name: "enter" }]],
    [["", { name: "enter" }]],
  ]);
  const interactiveOutput = new FakeOutput();
  const interactiveErrors = new FakeOutput();
  const interactiveExit = await runInteractiveInstaller({
    argumentsList: ["install"],
    packageRoot: repositoryRoot,
    cwd: interactiveProject,
    home: interactiveHome,
    environment: { PATH: "", DELEGATION_LAYER_HOME: interactiveHome },
    platform: "linux",
    input: interactiveInput,
    output: interactiveOutput,
    errorOutput: interactiveErrors,
    installCliMain: async () => {},
  });
  assert.equal(interactiveExit, 0, interactiveErrors.writes.join(""));
  await readFile(join(interactiveProject, ".claude/skills/agent-integration/SKILL.md"));
  const interactiveRouting = await readJSON(join(interactiveProject, ".claude/settings.local.json"));
  assert.equal(interactiveRouting.hooks.SessionStart.length, 1);
  assert.equal(interactiveRouting.hooks.SubagentStart.length, 1);
  assert.deepEqual(interactiveInput.rawModes, [true, false, true, false, true, false]);
  assert.equal(interactiveInput.pauseCount, 3);

  const existing = { unrelated: { keep: true }, hooks: { SessionStart: [{ matcher: "startup", hooks: [{ type: "command", command: "echo user-hook" }] }], Stop: [{ hooks: [{ type: "command", command: "echo stop" }] }] } };
  const original = JSON.stringify(existing);
  await writeFile(configPath, original);
  await chmod(configPath, 0o640);
  const result = success(run(install));
  assert.match(result.stdout, /Activation not verified/);
  assert.match(result.stdout, /trust.*\/hooks/);
  const config = await readJSON(configPath);
  assert.equal((await stat(configPath)).mode & 0o777, 0o640);
  assert.equal((await stat(codexPath)).mode & 0o777, 0o600);
  assert.deepEqual(config.unrelated, existing.unrelated);
  assert.deepEqual(config.hooks.Stop, existing.hooks.Stop);
  assert.deepEqual(config.hooks.SessionStart[0], existing.hooks.SessionStart[0]);
  assert.equal(config.hooks.SessionStart.length, 2);
  assert.equal(config.hooks.SubagentStart.length, 1);
  const configured = await readFile(configPath, "utf8");
  const helper = join(home, ".claude/skills/agent-integration/scripts/delegate-routing-context.sh");
  await rm(helper);
  assert.match(success(run(["routing", "status", "--harness", "claude"])).stdout, /incomplete/);
  success(run([...install, "--force"]));
  success(run(install));
  assert.equal(await readFile(configPath, "utf8"), configured, "reinstall must not duplicate or rewrite hooks");
  const backups = (await readdir(dirname(configPath))).filter((name) => name.includes("backup"));
  assert.equal(backups.length, 1);
  assert.equal(await readFile(join(dirname(configPath), backups[0]), "utf8"), original);
  for (const event of ["SessionStart", "SubagentStart"]) {
    const command = config.hooks[event].at(-1).hooks[0].command;
    const hook = spawnSync("/bin/sh", ["-c", command], { encoding: "utf8", env: { PATH: "/usr/bin:/bin" }, input: "{}" });
    success(hook);
    const output = JSON.parse(hook.stdout).hookSpecificOutput;
    assert.equal(output.hookEventName, event);
    assert.match(output.additionalContext, /external agent/);
    assert.ok(output.additionalContext.includes(join(dirname(dirname(helper)), "SKILL.md")));
  }
  const unusualSkill = join(root, "skill \"quoted\" \\ tab\tline\n雪");
  await cp(dirname(dirname(helper)), unusualSkill, { recursive: true });
  for (const event of ["SessionStart", "SubagentStart"]) {
    const hook = success(spawnSync("/bin/sh", [join(unusualSkill, "scripts/delegate-routing-context.sh"), event], {
      encoding: "utf8", env: { PATH: "/usr/bin:/bin" }, input: "{}",
    }));
    const context = JSON.parse(hook.stdout).hookSpecificOutput.additionalContext;
    assert.ok(context.includes(join(unusualSkill, "SKILL.md")), "hook must preserve the exact skill path");
    assert.match(context, /external agent/);
  }
  success(run(["install", "--scope", "global", "--harness", "claude", "--routing", "skill-only"]));
  assert.equal(await readFile(configPath, "utf8"), configured);
  assert.match(success(run(["routing", "status"])).stdout, /configured/);
  success(run(["routing", "disable"]));
  assert.deepEqual(await readJSON(configPath), existing);
  assert.deepEqual(await readJSON(codexPath), { hooks: {} });
  success(run(["routing", "disable"]));
  assert.match(success(run(["routing", "status"])).stdout, /not-configured/);
  success(run(install));
  await writeFile(codexPath, "{ invalid JSON");
  const before = await readFile(configPath, "utf8");
  assert.notEqual(run(["routing", "disable"]).status, 0);
  assert.equal(await readFile(configPath, "utf8"), before, "all configurations must validate before mutation");
  assert.notEqual(run([...install, "--force"]).status, 0, "force must not override malformed settings");
  await writeFile(codexPath, JSON.stringify({ hooks: { SessionStart: "invalid" } }));
  assert.notEqual(run(install).status, 0);
  await rm(codexPath);
  await symlink(configPath, codexPath);
  assert.notEqual(run(install).status, 0);
  await rm(codexPath);
  const otherHome = join(root, "rollback");
  const rollbackClaudeDirectory = join(otherHome, ".claude");
  await mkdir(join(otherHome, ".codex"), { recursive: true });
  await mkdir(rollbackClaudeDirectory, { recursive: true });
  const rollbackClaudePath = join(rollbackClaudeDirectory, "settings.json");
  const rollbackClaudeOriginal = JSON.stringify({
    unrelated: { preserve: true },
    hooks: { Stop: [{ hooks: [{ type: "command", command: "echo existing" }] }] },
  });
  await writeFile(rollbackClaudePath, rollbackClaudeOriginal);
  await chmod(rollbackClaudePath, 0o640);
  await writeFile(join(otherHome, ".codex/hooks.json.delegation-layer.lock"), "occupied");
  const rollback = run(install, { DELEGATION_LAYER_HOME: otherHome });
  assert.notEqual(rollback.status, 0);
  assert.match(rollback.stderr, /rolled back/);
  assert.equal(
    await readFile(rollbackClaudePath, "utf8"),
    rollbackClaudeOriginal,
    "rollback must restore the existing user config exactly",
  );
  assert.equal((await stat(rollbackClaudePath)).mode & 0o777, 0o640);
  const absentRollbackHome = join(root, "rollback-absent");
  const absentRollbackClaudePath = join(absentRollbackHome, ".claude", "settings.json");
  await mkdir(join(absentRollbackHome, ".codex"), { recursive: true });
  await writeFile(join(absentRollbackHome, ".codex/hooks.json.delegation-layer.lock"), "occupied");
  const absentRollback = run(install, { DELEGATION_LAYER_HOME: absentRollbackHome });
  assert.notEqual(absentRollback.status, 0);
  assert.match(absentRollback.stderr, /rolled back/);
  await assert.rejects(readFile(absentRollbackClaudePath), { code: "ENOENT" });
  const custom = { CLAUDE_CONFIG_DIR: join(root, "custom claude"), CODEX_HOME: join(root, "custom codex") };
  success(run(install, custom));
  await readFile(join(custom.CLAUDE_CONFIG_DIR, "skills/agent-integration/SKILL.md"));
  assert.equal((await readJSON(join(custom.CODEX_HOME, "hooks.json"))).hooks.SessionStart.length, 1);
  success(run(["routing", "disable"], custom));
  success(run(["install", "--scope", "project", "--harness", "claude,codex", "--routing", "default"]));
  await readFile(join(project, ".claude/settings.local.json"));
  await readFile(join(project, ".codex/hooks.json"));
  success(run(["routing", "disable", "--scope", "project"]));
  for (const host of ["universal", "cursor", "gemini", "opencode", "pi", "hermes"]) {
    assert.match(success(run(["install", "--scope", "project", "--harness", host, "--routing", "default"])).stdout, /unsupported-host/);
  }
  assert.notEqual(run(["install", "--routing", "typo"]).status, 0);
  assert.notEqual(run(["routing", "invalid"]).status, 0);
  console.log("Routing installer checks passed (merge, capture, disable, rollback, paths, package hooks).");
} finally {
  await rm(root, { recursive: true, force: true });
}
