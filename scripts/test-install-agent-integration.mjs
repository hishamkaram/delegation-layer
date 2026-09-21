import assert from "node:assert/strict";
import { chmod, mkdir, mkdtemp, readdir, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const installerPath = resolve(process.argv[2] ?? join(repositoryRoot, "scripts", "install-agent-integration.mjs"));
const sourceDirectory = join(repositoryRoot, "skills", "agent-integration");

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
  async function files(directory, relative = "") {
    const entries = await readdir(join(directory, relative), { withFileTypes: true });
    const result = [];
    for (const entry of entries) {
      const child = join(relative, entry.name);
      if (entry.isDirectory()) {
        result.push(...(await files(directory, child)));
      } else {
        result.push(child);
      }
    }
    return result.sort();
  }

  const sourceFiles = await files(sourceDirectory);
  const installedFiles = await files(target);
  assert.deepEqual(installedFiles, sourceFiles);
  for (const relative of sourceFiles) {
    const installed = await readFile(join(target, relative));
    const source = await readFile(join(sourceDirectory, relative));
    assert.deepEqual(installed, source, relative);
  }
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
  assert.match(detected.stdout, /using global scope/);
  await assertInstalled(join(home, ".claude", "skills", "agent-integration"));

  const projectInstall = runInstaller(["install", "--scope", "project", "--harness", "claude", "--yes"], {
    cwd: project,
    home,
    path: bin,
  });
  assert.equal(projectInstall.status, 0, projectInstall.stderr);
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
  await assertInstalled(join(fallbackHome, ".agents", "skills", "agent-integration"));

  const changedSkill = join(project, ".claude", "skills", "agent-integration", "SKILL.md");
  await writeFile(changedSkill, "local change\n");
  const refusedOverwrite = runInstaller(["install", "--scope", "project", "--harness", "claude"], {
    cwd: project,
    home,
    path: bin,
  });
  assert.equal(refusedOverwrite.status, 1);
  assert.match(refusedOverwrite.stderr, /use --force/);

  const forcedOverwrite = runInstaller(["install", "--scope", "project", "--harness", "claude", "--force"], {
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

  const nestedConflictTarget = join(root, "nested-conflict-target");
  await mkdir(join(nestedConflictTarget, "references"), { recursive: true });
  const nestedConflictFile = join(nestedConflictTarget, "references", "result-handling.md");
  await writeFile(nestedConflictFile, "local customization\n");
  const nestedConflict = runInstaller(["--target", nestedConflictTarget], { cwd: project, home, path: bin });
  assert.equal(nestedConflict.status, 1);
  assert.match(nestedConflict.stderr, /use --force/);
  assert.equal(await readFile(nestedConflictFile, "utf8"), "local customization\n");

  const symlinkTarget = join(root, "symlink-target");
  const outside = join(root, "outside");
  await mkdir(symlinkTarget, { recursive: true });
  await mkdir(outside, { recursive: true });
  await symlink(outside, join(symlinkTarget, "references"), "dir");
  const symlinkInstall = runInstaller(["--target", symlinkTarget], { cwd: project, home, path: bin });
  assert.equal(symlinkInstall.status, 1);
  assert.match(symlinkInstall.stderr, /must not contain symlink components/);

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
