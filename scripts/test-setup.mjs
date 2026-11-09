import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { spawnSync } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const entrypoint = await realpath(process.argv[2] ?? join(repositoryRoot, "scripts/install-agent-integration.mjs"));
const packageRoot = resolve(dirname(entrypoint), "..");
const { runInstaller } = await import(pathToFileURL(join(packageRoot, "scripts/installer/application.mjs")));
const { runSetupDoctor } = await import(pathToFileURL(join(packageRoot, "scripts/installer/setup-doctor.mjs")));
const { parseSetupArguments } = await import(pathToFileURL(join(packageRoot, "scripts/installer/setup-arguments.mjs")));
const { shellQuote } = await import(pathToFileURL(join(packageRoot, "scripts/installer/setup-recipe.mjs")));
const root = await mkdtemp(join(tmpdir(), "delegate-setup-test-"));

function doctorReport(directory, overrides = {}) {
  return {
    schema_version: 1,
    command: "doctor",
    root: { path: join(directory, "state"), status: "not_initialized" },
    installation: {
      scope: "installation_bundle", verification: "filesystem_metadata", compatibility: "not_checked", status: "observed",
      roles: Object.fromEntries([["delegate", "delegate"], ["delegate_run", "delegate-run"], ["pueue", "pueue"], ["pueued", "pueued"]].map(([key, role]) => [key, { path: join(directory, role), status: "available" }])),
    },
    supervisor: { binding: "not_initialized", liveness: "not_checked", active_work: "unknown" },
    issues: [],
    ...overrides,
  };
}

async function executable(path, source) {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, `#!${process.execPath}\n${source}\n`);
  await chmod(path, 0o755);
}

async function fixture(name) {
  const directory = join(root, name);
  const home = join(directory, "home");
  const cwd = join(directory, "workspace");
  const bin = join(directory, "bin");
  await Promise.all([home, cwd, bin].map((path) => mkdir(path, { recursive: true })));
  return { directory, home, cwd, bin };
}

async function installed(f, { payload = doctorReport(f.bin), code = 0, before = "" } = {}) {
  await executable(join(f.bin, "delegate"), `${before}\nprocess.stdout.write(${JSON.stringify(JSON.stringify(payload))}); process.exitCode = ${code};`);
}

async function run(f, args = [], options = {}) {
  let stdout = "";
  let stderr = "";
  const calls = [];
  const environment = {
    ...process.env, PATH: f.bin, DELEGATION_LAYER_HOME: f.home, HOME: f.home,
    CLAUDE_CONFIG_DIR: "", CODEX_HOME: "", DELEGATION_LAYER_INSTALL_DIR: "",
    ...options.environment,
  };
  if (!options.environment?.DELEGATION_LAYER_INSTALL_DIR) delete environment.DELEGATION_LAYER_INSTALL_DIR;
  const input = options.input ?? { isTTY: false };
  const code = await runInstaller({
    argumentsList: ["setup", ...(options.defaults ? [] : ["--yes", "--harness", "claude"]), ...args], packageRoot,
    cwd: f.cwd, home: f.home, environment, platform: process.platform,
    input, output: { isTTY: Boolean(input.isTTY), write: (chunk) => { stdout += chunk; } },
    errorOutput: { write: (chunk) => { stderr += chunk; } },
    installCliMain: async (argumentsList, context) => {
      calls.push({ argumentsList, context });
      if (options.install) return options.install(argumentsList, context);
      throw new Error("Unexpected CLI install");
    },
  });
  return { code, stdout, stderr, calls };
}

async function noSkill(f) {
  await assert.rejects(readFile(join(f.home, ".claude/skills/agent-integration/SKILL.md")), { code: "ENOENT" });
}

try {
  for (const args of [["--provider", "claude:print"], ["--cwd", root], ["--model", "x"], ["--version", "1.2.3"], ["--install-dir", "relative"]]) {
    assert.equal(parseSetupArguments(args).kind, "error", JSON.stringify(args));
  }
  assert.equal(shellQuote("a'b"), "'a'\\''b'");
  const shellValue = "spaces 'quotes' \"double\" ; $(false) `false` $HOME\nnext";
  const quotedShell = spawnSync("sh", ["-c", `printf '%s' ${shellQuote(shellValue)}`], { encoding: "utf8" });
  assert.ifError(quotedShell.error);
  assert.equal(quotedShell.status, 0);
  assert.equal(quotedShell.stdout, shellValue);

  class TerminalInput extends EventEmitter {
    constructor(keys) { super(); this.isTTY = true; this.keys = keys; }
    setRawMode() {}
    pause() {}
    resume() { queueMicrotask(() => this.emit("keypress", "", { name: this.keys.shift() })); }
  }
  const cancellation = await fixture("cancellation");
  const cancelled = await run(cancellation, [], { defaults: true, input: new TerminalInput(["q"]) });
  assert.equal(cancelled.code, 0, cancelled.stderr);
  assert.match(cancelled.stdout, /Setup cancelled/);
  assert.doesNotMatch(cancelled.stdout, /Setup completed/);
  assert.equal(cancelled.calls.length, 0);
  await noSkill(cancellation);

  const interactive = await fixture("interactive");
  await mkdir(join(interactive.home, ".claude"));
  await installed(interactive);
  const interactiveDefaults = await run(interactive, [], { defaults: true, input: new TerminalInput(["return", "return", "return"]) });
  assert.equal(interactiveDefaults.code, 0, interactiveDefaults.stderr);
  const hookConfig = JSON.parse(await readFile(join(interactive.home, ".claude/settings.json"), "utf8"));
  assert.ok(hookConfig.hooks.SessionStart.length > 0);


  const matching = await fixture("matching");
  const additive = doctorReport(matching.bin);
  additive.future_field = { allowed: true };
  additive.installation.roles.future_role = { arbitrary: "additive" };
  await installed(matching, { payload: additive });
  const success = await run(matching);
  assert.equal(success.code, 0, success.stderr);
  assert.match(success.stdout, /Setup completed/);
  assert.match(success.stdout, /Skill-only installation/);
  assert.match(success.stdout, /choose a provider and workspace/);
  assert.doesNotMatch(success.stdout, / dispatch /);
  assert.equal(success.calls.length, 0);
  assert.match((await run(matching)).stdout, /Skill already installed/);

  const provider = await fixture("provider 'quote");
  const observed = join(provider.directory, "args.json");
  const payload = doctorReport(provider.bin, { provider: { id: "claude:print", status: "static_ready", executable: "/installed/provider", authentication: "unknown", verification: "static_preflight" } });
  await installed(provider, { payload, before: `require('node:fs').writeFileSync(${JSON.stringify(observed)}, JSON.stringify(process.argv.slice(2)));` });
  const providerArgs = ["--provider", "claude:print", "--cwd", provider.cwd, "--model", "chosen", "--effort", "high"];
  const recipe = await run(provider, providerArgs);
  assert.equal(recipe.code, 0, recipe.stderr);
  const blocks = [...recipe.stdout.matchAll(/```sh\n([\s\S]*?)\n```/g)];
  assert.equal(blocks.length, 3);
  for (const [, block] of blocks) {
    const syntax = spawnSync("sh", ["-n"], { input: block, encoding: "utf8" });
    assert.ifError(syntax.error);
    assert.equal(syntax.status, 0, syntax.stderr);
  }
  assert.deepEqual(JSON.parse(await readFile(observed, "utf8")), ["doctor", "--json", ...providerArgs]);
  assert.match(recipe.stdout, /TASK_FILES="\$\(mktemp -d\)"/);
  assert.ok(recipe.stdout.includes(shellQuote(join(provider.bin, "delegate"))));
  for (const text of ["dispatch", "status", "collect", "cancel", "task_record", "evidence"]) assert.ok(recipe.stdout.includes(text), text);

  const absent = await fixture("absent");
  const requestedVersion = JSON.parse(await readFile(join(packageRoot, "package.json"), "utf8")).version;
  const exact = await run(absent, ["--install-dir", absent.bin], {
    environment: { DELEGATION_LAYER_VERSION: "9.9.9" },
    install: async (args) => {
      assert.deepEqual(args, ["--version", requestedVersion, "--install-dir", absent.bin]);
      await installed(absent);
    },
  });
  assert.equal(exact.code, 0, exact.stderr);
  assert.equal(exact.calls.length, 1);
  assert.equal(exact.calls[0].context.noReplace, true, "setup must ask the CLI owner for atomic no-replace publication");
  assert.doesNotMatch(exact.stdout, /Manual PATH step/, "refresh PATH after installing into an existing PATH directory");

  const failedInstall = await fixture("partial-publication");
  const failed = await run(failedInstall, ["--install-dir", failedInstall.bin], {
    install: async () => {
      await writeFile(join(failedInstall.bin, "pueue"), "published role");
      throw new Error("fixture publication failed");
    },
  });
  assert.equal(failed.code, 1);
  assert.match(failed.stderr, /matching CLI release/);
  await noSkill(failedInstall);
  const partialRerun = await run(failedInstall, ["--install-dir", failedInstall.bin, "--force"]);
  assert.equal(partialRerun.code, 1);
  assert.equal(partialRerun.calls.length, 0, "force must not authorize replacing partially published CLI roles");

  const environmentSelection = await fixture("environment-selection");
  const environmentBin = join(environmentSelection.directory, "environment-bin");
  await executable(join(environmentBin, "delegate"), `process.stdout.write(${JSON.stringify(JSON.stringify(doctorReport(root)))});`);
  await executable(join(environmentSelection.bin, "delegate"), "throw new Error('PATH must not win');");
  const envSelected = await run(environmentSelection, [], { environment: { DELEGATION_LAYER_INSTALL_DIR: environmentBin } });
  assert.equal(envSelected.code, 0, envSelected.stderr);
  assert.ok(envSelected.stdout.includes(`Selected CLI: ${join(environmentBin, "delegate")}`));
  await installed(environmentSelection);
  const explicitSelected = await run(environmentSelection, ["--install-dir", environmentSelection.bin], { environment: { DELEGATION_LAYER_INSTALL_DIR: environmentBin } });
  assert.equal(explicitSelected.code, 0, explicitSelected.stderr);
  assert.ok(explicitSelected.stdout.includes(`Selected CLI: ${join(environmentSelection.bin, "delegate")}`));

  const defaultSelection = await fixture("default-selection");
  const defaultBin = join(defaultSelection.home, ".local/bin");
  await executable(join(defaultBin, "delegate"), `process.stdout.write(${JSON.stringify(JSON.stringify(doctorReport(defaultBin)))});`);
  const defaultSelected = await run(defaultSelection);
  assert.equal(defaultSelected.code, 0, defaultSelected.stderr);
  assert.ok(defaultSelected.stdout.includes(`Selected CLI: ${join(defaultBin, "delegate")}`));

  for (const role of ["delegate-run", "pueue", "pueued"]) {
    const partial = await fixture(`partial-${role}`);
    await symlink(join(partial.directory, "missing"), join(partial.bin, role));
    const result = await run(partial, ["--install-dir", partial.bin]);
    assert.equal(result.code, 1);
    assert.match(result.stderr, new RegExp(`existing roles: ${role}`));
    assert.equal(result.calls.length, 0);
    await noSkill(partial);
  }
  const broken = await fixture("broken-path");
  await symlink(join(broken.directory, "missing"), join(broken.bin, "delegate"));
  const blockedPath = await run(broken);
  assert.equal(blockedPath.code, 1);
  assert.match(blockedPath.stderr, /Selected Delegate is unusable/);
  assert.equal(blockedPath.calls.length, 0);

  const shadowed = await fixture("shadowed");
  const selected = join(shadowed.directory, "selected");
  await executable(join(selected, "delegate"), `process.stdout.write(${JSON.stringify(JSON.stringify(doctorReport(selected)))});`);
  await executable(join(shadowed.bin, "delegate"), "throw new Error('wrong CLI selected');");
  const shadow = await run(shadowed, ["--install-dir", selected]);
  assert.equal(shadow.code, 0, shadow.stderr);
  assert.match(shadow.stdout, /Manual PATH step/);
  assert.ok(shadow.stdout.includes(`Selected CLI: ${join(selected, "delegate")}`));

  for (const [name, payload, code] of [
    ["unsupported", { schema_version: 2 }, 0],
    ["malformed-fields", { ...doctorReport(root), supervisor: {} }, 0],
    ["nonzero", doctorReport(root), 1],
    ["blocked", doctorReport(root), 2],
  ]) {
    const f = await fixture(name);
    await installed(f, { payload, code });
    const result = await run(f, ["--routing", "default"]);
    assert.equal(result.code, 1, name);
    await noSkill(f);
    await assert.rejects(readFile(join(f.home, ".claude/settings.json")), { code: "ENOENT" });
  }

  const conflict = await fixture("conflict");
  const skillPath = join(conflict.home, ".claude/skills/agent-integration/SKILL.md");
  await mkdir(dirname(skillPath), { recursive: true });
  await writeFile(skillPath, "user content");
  const conflictResult = await run(conflict, ["--install-dir", conflict.bin]);
  assert.equal(conflictResult.code, 1);
  assert.equal(conflictResult.calls.length, 0, "skill conflict must precede CLI installation");
  await installed(conflict);
  assert.equal((await run(conflict, ["--force"])).code, 0);

  const malformedRouting = await fixture("malformed-routing");
  await mkdir(join(malformedRouting.home, ".claude"));
  await writeFile(join(malformedRouting.home, ".claude/settings.json"), "{");
  const invalidRouting = await run(malformedRouting, ["--install-dir", malformedRouting.bin, "--routing", "default"]);
  assert.equal(invalidRouting.code, 1);
  assert.equal(invalidRouting.calls.length, 0);

  const race = await fixture("refresh");
  const raceSkill = join(race.home, ".claude/skills/agent-integration/SKILL.md");
  await installed(race, { before: `require('node:fs').mkdirSync(${JSON.stringify(dirname(raceSkill))}, {recursive:true}); require('node:fs').writeFileSync(${JSON.stringify(raceSkill)}, 'concurrent content');` });
  assert.equal((await run(race)).code, 1);
  assert.equal(await readFile(raceSkill, "utf8"), "concurrent content");

  const overflow = await fixture("overflow");
  const marker = join(overflow.directory, "natural-exit");
  await executable(join(overflow.bin, "delegate"), `const fs = require('node:fs'); const chunk = Buffer.alloc(65536, 120); for (let i=0;i<24;i++) { fs.writeSync(1, chunk); fs.writeSync(2, chunk); } fs.writeFileSync(${JSON.stringify(marker)}, 'closed');`);
  await assert.rejects(runSetupDoctor({ executable: join(overflow.bin, "delegate"), providerArguments: [], environment: process.env }), /exceeded the 1 MiB limit/);
  assert.equal(await readFile(marker, "utf8"), "closed", "overflow must drain both streams to natural child completion");
  console.log("Guided setup behavior checks passed.");
} finally {
  await rm(root, { recursive: true, force: true });
}
