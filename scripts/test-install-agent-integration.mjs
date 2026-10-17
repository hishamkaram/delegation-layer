import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { chmod, mkdir, mkdtemp, readdir, readFile, realpath, rm, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve, sep } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { main as installCliMain } from "./install-cli.mjs";
import { parseInstallArguments, parseLegacyArguments } from "./installer/arguments.mjs";
import { harnesses } from "./installer/catalog.mjs";
import { routingPath } from "./installer/routing-rules.mjs";
import { targetFor } from "./installer/selection.mjs";

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const installerPath = resolve(process.argv[2] ?? join(repositoryRoot, "scripts", "install-agent-integration.mjs"));
const sourceDirectory = join(repositoryRoot, "skills", "agent-integration");

if (process.platform !== "win32") {
  const sourcePath = await realpath(installerPath);
  for (const entrypoint of [sourcePath, join(dirname(sourcePath), "install-cli.mjs")]) {
    assert.equal((await stat(entrypoint)).mode & 0o111, 0o111, `Installer is not executable: ${entrypoint}`);
  }
}

function runInstaller(args, { cwd, home, path, input = "" }) {
  const result = spawnSync(process.execPath, [installerPath, ...args], {
    cwd,
    encoding: "utf8",
    env: {
      ...process.env,
      DELEGATION_LAYER_HOME: home,
      CLAUDE_CONFIG_DIR: "",
      CODEX_HOME: "",
      PATH: path,
    },
    input,
  });
  return result;
}

async function testEntryWithoutResolvableHome(root) {
  const preloadPath = join(root, "throw-homedir.mjs");
  await writeFile(preloadPath, [
    'import os from "node:os";',
    'import { syncBuiltinESMExports } from "node:module";',
    'os.homedir = () => { throw new Error("forced home lookup failure"); };',
    "syncBuiltinESMExports();",
    "",
  ].join("\n"));
  const environment = { ...process.env };
  delete environment.DELEGATION_LAYER_HOME;
  const runWithoutHome = (argumentsList) => spawnSync(process.execPath, [
    "--import",
    preloadPath,
    installerPath,
    ...argumentsList,
  ], { cwd: root, encoding: "utf8", env: environment });

  const target = join(root, "entry-legacy-target");
  const legacy = runWithoutHome(["--target", target]);
  assert.equal(legacy.status, 0, legacy.stderr);
  await assertInstalled(target);

  const cliHelp = runWithoutHome(["install-cli", "--help"]);
  assert.equal(cliHelp.status, 0, cliHelp.stderr);
  assert.equal(cliHelp.stdout, "");
  assert.equal(cliHelp.stderr, "Usage: delegation-layer install-cli [--version VERSION] [--install-dir ABSOLUTE_DIR]\n");

  const install = runWithoutHome([
    "install", "--scope", "global", "--harness", "claude", "--yes", "--routing", "skill-only",
  ]);
  assert.equal(install.status, 1);
  assert.equal(install.stdout, "");
  assert.equal(install.stderr, "Error: forced home lookup failure\n");
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

const root = await realpath(await mkdtemp(join(tmpdir(), "delegation-layer-installer-")));
try {
  await testEntryWithoutResolvableHome(root);

  for (const target of [root, root + sep, join(root, "target") + sep, join(root, "target", "..", "other")]) {
    assert.deepEqual(parseLegacyArguments(["--target", target]), {
      kind: "options", options: { force: false, target: resolve(target) },
    });
  }
  const claudeHarness = harnesses.find((harness) => harness.name === "claude");
  assert.ok(claudeHarness);
  for (const cwd of [join(root, "first project"), join(root, "second project")]) {
    assert.equal(targetFor(claudeHarness, "global", cwd, root, { CLAUDE_CONFIG_DIR: "relative claude" }),
      join(cwd, "relative claude", "skills", "agent-integration"));
    assert.equal(routingPath("claude", { scope: "global", cwd, home: root, claudeConfigDir: "relative claude" }),
      join(cwd, "relative claude", "settings.json"));
    assert.equal(routingPath("codex", { scope: "global", cwd, home: root, codexHome: "relative codex" }),
      join(cwd, "relative codex", "hooks.json"));
  }
  assert.deepEqual(
    parseInstallArguments(["--scope", "project", "--harness", "Claude,Codex", "-y", "--force", "--routing", "default"]),
    {
      kind: "options",
      options: {
        force: true,
        harnessNames: ["claude", "codex"],
        scope: "project",
        yes: true,
        routing: "default",
      },
    },
  );
  assert.deepEqual(parseLegacyArguments(["--target", join(root, "legacy flags"), "--force"]), {
    kind: "options",
    options: { force: true, target: join(root, "legacy flags") },
  });

  const cliCalls = [];
  const cliOutput = [];
  await installCliMain(["--version", "9.8.7", "--install-dir", join(root, "release install")], {
    packageRoot: repositoryRoot,
    environment: { PRESERVED_SETTING: "kept" },
    platform: "linux",
    errorOutput: { write: (value) => cliOutput.push(value) },
    spawnProcess: (command, argumentsList, options) => {
      cliCalls.push({ command, argumentsList, options });
      const child = new EventEmitter();
      queueMicrotask(() => child.emit("exit", 0, null));
      return child;
    },
  });
  assert.equal(cliCalls.length, 1);
  assert.equal(cliCalls[0].command, "sh");
  assert.deepEqual(cliCalls[0].argumentsList, [join(repositoryRoot, "install.sh")]);
  assert.equal(cliCalls[0].options.stdio, "inherit");
  assert.equal(cliCalls[0].options.env.PRESERVED_SETTING, "kept");
  assert.equal(cliCalls[0].options.env.DELEGATION_LAYER_VERSION, "9.8.7");
  assert.equal(cliCalls[0].options.env.DELEGATION_LAYER_INSTALL_DIR, join(root, "release install"));
  assert.deepEqual(cliOutput, []);

  const helpOutput = [];
  await installCliMain(["--help"], {
    packageRoot: repositoryRoot,
    errorOutput: { write: (value) => helpOutput.push(value) },
    spawnProcess: () => assert.fail("help must not launch the release installer"),
  });
  assert.match(helpOutput.join(""), /delegation-layer install-cli/);
  assert.equal(cliCalls.length, 1);

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
