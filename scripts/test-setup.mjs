import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { spawnSync } from "node:child_process";
import { chmod, lstat, mkdir, mkdtemp, readFile, realpath, rm, symlink, writeFile } from "node:fs/promises";
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
const root = await realpath(await mkdtemp(join(tmpdir(), "delegate-setup-test-")));

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
  assert.equal(blocks.length, 4);
  for (const [, block] of blocks) {
    const syntax = spawnSync("sh", ["-n"], { input: block, encoding: "utf8" });
    assert.ifError(syntax.error);
    assert.equal(syntax.status, 0, syntax.stderr);
  }
  assert.match(blocks[1][1], /read -r TASK$/);
  assert.doesNotMatch(blocks[1][1], / status | collect /);
  assert.deepEqual(JSON.parse(await readFile(observed, "utf8")), ["doctor", "--json", ...providerArgs]);

  const preparation = await fixture("recipe-preparation");
  const prepShims = join(preparation.directory, "shims");
  await mkdir(prepShims);
  const mktempShim = join(prepShims, "mktemp");
  await executable(mktempShim, "process.exitCode = 1;");
  const prepareBlock = blocks[0][1];
  const prepareEnvironment = { ...process.env, PATH: `${prepShims}:${process.env.PATH ?? ""}` };
  const noTemp = spawnSync("sh", ["-s"], { input: `${prepareBlock}\nprintf 'shell-survived'\n`, encoding: "utf8", env: prepareEnvironment });
  assert.ifError(noTemp.error);
  assert.equal(noTemp.status, 0, noTemp.stderr);
  assert.match(noTemp.stdout, /shell-survived/);
  assert.match(noTemp.stderr, /Could not create task files/);
  assert.deepEqual(JSON.parse(await readFile(observed, "utf8")), ["doctor", "--json", ...providerArgs], "preparation failure must not dispatch");

  const failedBriefDirectory = join(preparation.directory, "failed-brief");
  await mkdir(failedBriefDirectory);
  await executable(mktempShim, `process.stdout.write(${JSON.stringify(failedBriefDirectory)});`);
  await executable(join(prepShims, "cat"), "process.exitCode = 1;");
  const noBrief = spawnSync("sh", ["-s"], { input: `${prepareBlock}\nprintf 'shell-survived'\n`, encoding: "utf8", env: prepareEnvironment });
  assert.ifError(noBrief.error);
  assert.equal(noBrief.status, 0, noBrief.stderr);
  assert.match(noBrief.stdout, /shell-survived/);
  assert.match(noBrief.stderr, /Brief preparation failed/);
  await assert.rejects(readFile(failedBriefDirectory), { code: "ENOENT" });
  assert.deepEqual(JSON.parse(await readFile(observed, "utf8")), ["doctor", "--json", ...providerArgs], "brief failure must not dispatch");

  const unknownDirectory = join(preparation.directory, "unknown-response");
  await mkdir(unknownDirectory);
  await executable(mktempShim, `process.stdout.write(${JSON.stringify(unknownDirectory)});`);
  await rm(join(prepShims, "cat"));
  const unknownDispatch = spawnSync("sh", ["-s"], { input: prepareBlock, encoding: "utf8", env: prepareEnvironment });
  assert.ifError(unknownDispatch.error);
  assert.equal(unknownDispatch.status, 0, unknownDispatch.stderr);
  assert.match(await readFile(join(unknownDirectory, "brief"), "utf8"), /Do not edit files/);
  assert.equal(await readFile(join(unknownDirectory, "response.json"), "utf8"), JSON.stringify(payload), "retain evidence when the fixture gives no created task record");

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

  const pathEntries = await fixture("path-entries");
  await installed(pathEntries);
  await executable(join(pathEntries.cwd, "delegate"), "throw new Error('empty PATH must not select cwd');");
  const notDirectory = join(pathEntries.directory, "not-a-directory");
  await writeFile(notDirectory, "regular PATH entry");
  const inaccessible = join(pathEntries.directory, "inaccessible");
  await mkdir(inaccessible);
  await executable(join(inaccessible, "delegate"), "throw new Error('unavailable PATH entry');");
  await chmod(inaccessible, 0);
  try {
    // Elevated users can traverse mode-000 directories; exercise EACCES on
    // ordinary contributor/CI accounts, and still cover ENOTDIR on every host.
    const deniedEntry = process.getuid?.() === 0 ? "" : inaccessible;
    const unusualPath = `:${notDirectory}:${deniedEntry}::${pathEntries.bin}:`;
    const pathSelected = await run(pathEntries, [], { environment: { PATH: unusualPath } });
    assert.equal(pathSelected.code, 0, pathSelected.stderr);
    assert.ok(pathSelected.stdout.includes(`Selected CLI: ${join(pathEntries.bin, "delegate")}`));
    const explicitDespitePath = await run(pathEntries, ["--install-dir", pathEntries.bin], { environment: { PATH: `${notDirectory}:${deniedEntry}` } });
    assert.equal(explicitDespitePath.code, 0, explicitDespitePath.stderr);
  } finally {
    await chmod(inaccessible, 0o700);
  }
  await executable(join(defaultSelection.cwd, "delegate"), "throw new Error('unset PATH must not select cwd');");
  for (const path of [undefined, ""]) {
    const withoutPath = await run(defaultSelection, [], { environment: { PATH: path, DELEGATION_LAYER_INSTALL_DIR: "" } });
    assert.equal(withoutPath.code, 0, withoutPath.stderr);
    assert.ok(withoutPath.stdout.includes(`Selected CLI: ${join(defaultBin, "delegate")}`));
  }

  const advisoryPath = await fixture("advisory-path");
  await installed(advisoryPath);
  const loopEntry = join(advisoryPath.directory, "loop");
  await symlink("loop", loopEntry);
  const longEntry = join(advisoryPath.directory, "x".repeat(512));
  for (const [name, entry, code] of [["loop", loopEntry, "ELOOP"], ["long", longEntry, "ENAMETOOLONG"]]) {
    await assert.rejects(lstat(join(entry, "delegate")), { code }, "the fixture must produce the requested native PATH observation failure");
    for (const useEnvironment of [false, true]) {
      const result = await run(advisoryPath, useEnvironment ? [] : ["--install-dir", advisoryPath.bin], {
        environment: { PATH: `${entry}:${advisoryPath.bin}`, ...(useEnvironment ? { DELEGATION_LAYER_INSTALL_DIR: advisoryPath.bin } : {}) },
      });
      assert.equal(result.code, 0, result.stderr);
      assert.ok(result.stdout.includes(`Selected CLI: ${join(advisoryPath.bin, "delegate")}`));
      assert.match(result.stdout, /Current PATH selection: unknown \(PATH inspection unavailable\)/);
      assert.ok(result.stdout.includes(`${shellQuote(join(advisoryPath.bin, "delegate"))} doctor --json`));
      assert.doesNotMatch(result.stderr + result.stdout, /ELOOP|ENAMETOOLONG/);
      assert.equal(result.calls.length, 0);
    }
    const automatic = await fixture(`automatic-path-${name}`);
    await installed(automatic);
    const failed = await run(automatic, [], { environment: { PATH: `${entry}:${automatic.bin}` } });
    assert.equal(failed.code, 1, "automatic selection must not infer a fallback from unobservable PATH");
    assert.equal(failed.calls.length, 0);
    await noSkill(automatic);
  }

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

  for (const initiallyInstalled of [true, false]) {
    const rejected = await fixture(`rejected-request-${initiallyInstalled}`);
    const rejectedSource = "process.stderr.write('private native diagnostic must not be echoed'); process.exitCode = 2;";
    if (initiallyInstalled) await executable(join(rejected.bin, "delegate"), rejectedSource);
    const result = await run(rejected, ["--provider", "bad:profile", "--cwd", rejected.cwd, "--routing", "default", "--install-dir", rejected.bin], {
      install: async () => executable(join(rejected.bin, "delegate"), rejectedSource),
    });
    assert.equal(result.code, 2, result.stderr);
    assert.match(result.stderr, /Doctor command\/request rejected/);
    assert.match(result.stderr, /check the provider options/);
    assert.match(result.stderr, /if doctor is unsupported/);
    assert.match(result.stderr, /Usage: delegation-layer setup/);
    assert.doesNotMatch(result.stderr + result.stdout, /private native diagnostic/);
    assert.ok(result.stdout.includes(`Completed: ${initiallyInstalled ? "none" : "CLI installation"}`));
    await noSkill(rejected);
    await assert.rejects(readFile(join(rejected.home, ".claude/settings.json")), { code: "ENOENT" });
  }

  const contradictory = [
    ["installation-blocked", (report) => { report.installation.status = "blocked"; }],
    ["installation-incomplete", (report) => { report.installation.status = "observation_incomplete"; }],
    ["missing-role", (report) => { report.installation.roles.delegate_run.status = "missing"; }],
    ["root-incomplete", (report) => { report.root.status = "observation_incomplete"; }],
    ["binding-incomplete", (report) => { report.supervisor.binding = "observation_incomplete"; }],
    ["provider-blocked", (report) => { report.provider.status = "blocked"; }],
    ["provider-incomplete", (report) => { report.provider.status = "observation_incomplete"; }],
    ["provider-id-mismatch", (report) => { report.provider.id = "another:profile"; }],
  ];
  for (const [name, change] of contradictory) {
    const f = await fixture(name);
    const providerReport = { id: "claude:print", status: "static_ready", executable: "/installed/provider", authentication: "unknown", verification: "static_preflight" };
    const payload = doctorReport(f.bin, { provider: providerReport });
    change(payload);
    await installed(f, { payload, code: 0 });
    const result = await run(f, ["--provider", "claude:print", "--cwd", f.cwd, "--routing", "default"]);
    assert.equal(result.code, 1, name);
    assert.match(result.stderr, name === "provider-id-mismatch" ? /malformed doctor report/ : /exit status conflicts/);
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
