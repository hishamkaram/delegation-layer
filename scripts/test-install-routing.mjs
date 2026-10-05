import assert from "node:assert/strict";
import { cp, mkdir, mkdtemp, readFile, readdir, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

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
try {
  await mkdir(join(home, ".claude"), { recursive: true });
  await mkdir(project);
  const existing = { unrelated: { keep: true }, hooks: { SessionStart: [{ matcher: "startup", hooks: [{ type: "command", command: "echo user-hook" }] }], Stop: [{ hooks: [{ type: "command", command: "echo stop" }] }] } };
  const original = JSON.stringify(existing);
  await writeFile(configPath, original);
  const result = success(run(install));
  assert.match(result.stdout, /Activation not verified/);
  assert.match(result.stdout, /trust.*\/hooks/);
  const config = await readJSON(configPath);
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
  await mkdir(join(otherHome, ".codex"), { recursive: true });
  await writeFile(join(otherHome, ".codex/hooks.json.delegation-layer.lock"), "occupied");
  const rollback = run(install, { DELEGATION_LAYER_HOME: otherHome });
  assert.notEqual(rollback.status, 0);
  assert.match(rollback.stderr, /rolled back/);
  await assert.rejects(readFile(join(otherHome, ".claude/settings.json")), { code: "ENOENT" });
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
