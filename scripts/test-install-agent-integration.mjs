import assert from "node:assert/strict";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const installerPath = resolve(process.argv[2] ?? join(repositoryRoot, "scripts", "install-agent-integration.mjs"));
const sourcePath = join(repositoryRoot, "skills", "agent-integration", "SKILL.md");

function runInstaller(args, { cwd, home, path, input = "" }) {
  const result = spawnSync(process.execPath, [installerPath, ...args], {
    cwd,
    encoding: "utf8",
    env: {
      ...process.env,
      DELEGATION_LAYER_HOME: home,
      PATH: path,
    },
    input,
  });
  return result;
}

async function assertInstalled(target) {
  const installed = await readFile(join(target, "SKILL.md"));
  const source = await readFile(sourcePath);
  assert.deepEqual(installed, source);
}

const root = await mkdtemp(join(tmpdir(), "delegation-layer-installer-"));
try {
  const project = join(root, "project");
  const home = join(root, "home");
  const bin = join(root, "bin");
  const fallbackProject = join(root, "fallback-project");
  const fallbackHome = join(root, "fallback-home");
  await mkdir(join(project, ".claude"), { recursive: true });
  await mkdir(bin, { recursive: true });
  await mkdir(home, { recursive: true });
  await mkdir(fallbackProject, { recursive: true });
  await mkdir(fallbackHome, { recursive: true });
  const claude = join(bin, "claude");
  await writeFile(claude, "#!/bin/sh\nexit 0\n");
  await chmod(claude, 0o755);

  const detected = runInstaller(["install"], { cwd: project, home, path: bin });
  assert.equal(detected.status, 0, detected.stderr);
  assert.match(detected.stdout, /detected: project \.claude/);
  await assertInstalled(join(project, ".claude", "skills", "agent-integration"));

  const idempotent = runInstaller(["install", "--scope", "project", "--harness", "claude"], {
    cwd: project,
    home,
    path: bin,
  });
  assert.equal(idempotent.status, 0, idempotent.stderr);
  assert.match(idempotent.stdout, /Skill already installed/);

  const globalInstall = runInstaller(["install", "--scope", "global", "--harness", "pi"], {
    cwd: project,
    home,
    path: bin,
  });
  assert.equal(globalInstall.status, 0, globalInstall.stderr);
  await assertInstalled(join(home, ".pi", "agent", "skills", "agent-integration"));

  const fallback = runInstaller(["install", "--yes"], {
    cwd: fallbackProject,
    home: fallbackHome,
    path: join(root, "empty-bin"),
  });
  assert.equal(fallback.status, 0, fallback.stderr);
  await assertInstalled(join(fallbackProject, ".agents", "skills", "agent-integration"));

  const changedSkill = join(project, ".claude", "skills", "agent-integration", "SKILL.md");
  await writeFile(changedSkill, "local change\n");
  const refusedOverwrite = runInstaller(["install", "--harness", "claude"], {
    cwd: project,
    home,
    path: bin,
  });
  assert.equal(refusedOverwrite.status, 1);
  assert.match(refusedOverwrite.stderr, /use --force/);

  const forcedOverwrite = runInstaller(["install", "--harness", "claude", "--force"], {
    cwd: project,
    home,
    path: bin,
  });
  assert.equal(forcedOverwrite.status, 0, forcedOverwrite.stderr);
  await assertInstalled(join(project, ".claude", "skills", "agent-integration"));

  const legacyTarget = join(root, "legacy-target");
  const legacy = runInstaller(["--target", legacyTarget], { cwd: project, home, path: bin });
  assert.equal(legacy.status, 0, legacy.stderr);
  await assertInstalled(legacyTarget);

  const invalidHarness = runInstaller(["install", "--harness", "unknown"], {
    cwd: project,
    home,
    path: bin,
  });
  assert.equal(invalidHarness.status, 2);
  assert.match(invalidHarness.stderr, /unknown harness unknown/);

  const emptyHarness = runInstaller(["install", "--harness", ","], {
    cwd: project,
    home,
    path: bin,
  });
  assert.equal(emptyHarness.status, 2);
  assert.match(emptyHarness.stderr, /at least one harness name/);

  console.log("Agent integration installer behavior checks passed.");
} finally {
  await rm(root, { force: true, recursive: true });
}
