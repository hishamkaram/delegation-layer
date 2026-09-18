#!/usr/bin/env node

import { accessSync, constants } from "node:fs";
import { copyFile, lstat, mkdir, readFile } from "node:fs/promises";
import { createInterface } from "node:readline/promises";
import { delimiter, dirname, isAbsolute, join, resolve } from "node:path";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";

const packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const sourcePath = join(packageRoot, "skills", "agent-integration", "SKILL.md");
const harnesses = [
  {
    name: "universal",
    label: "Universal Agent Skills",
    projectPath: join(".agents", "skills"),
    globalPath: join(".agents", "skills"),
    projectMarkers: [".agents"],
    globalMarkers: [".agents"],
    commands: [],
  },
  {
    name: "codex",
    label: "Codex",
    projectPath: join(".agents", "skills"),
    globalPath: join(".agents", "skills"),
    projectMarkers: [".agents", ".codex"],
    globalMarkers: [".agents", ".codex"],
    commands: ["codex"],
  },
  {
    name: "claude",
    label: "Claude Code",
    projectPath: join(".claude", "skills"),
    globalPath: join(".claude", "skills"),
    projectMarkers: [".claude"],
    globalMarkers: [".claude"],
    commands: ["claude"],
  },
  {
    name: "cursor",
    label: "Cursor",
    projectPath: join(".cursor", "skills"),
    globalPath: join(".cursor", "skills"),
    projectMarkers: [".cursor"],
    globalMarkers: [".cursor"],
    commands: ["cursor"],
  },
  {
    name: "gemini",
    label: "Gemini CLI",
    projectPath: join(".gemini", "skills"),
    globalPath: join(".gemini", "skills"),
    projectMarkers: [".gemini"],
    globalMarkers: [".gemini"],
    commands: ["gemini"],
  },
  {
    name: "opencode",
    label: "OpenCode",
    projectPath: join(".opencode", "skills"),
    globalPath: join(".config", "opencode", "skills"),
    projectMarkers: [".opencode"],
    globalMarkers: [join(".config", "opencode")],
    commands: ["opencode"],
  },
  {
    name: "pi",
    label: "Pi",
    projectPath: join(".pi", "skills"),
    globalPath: join(".pi", "agent", "skills"),
    projectMarkers: [".pi"],
    globalMarkers: [join(".pi", "agent")],
    commands: ["pi"],
  },
  {
    name: "hermes",
    label: "Hermes",
    projectPath: join(".hermes", "skills"),
    globalPath: join(".hermes", "skills"),
    projectMarkers: [".hermes"],
    globalMarkers: [".hermes"],
    commands: ["hermes"],
  },
];
const harnessByName = new Map(harnesses.map((harness) => [harness.name, harness]));

function usage(message, exitCode = 2) {
  if (message) {
    console.error(`Error: ${message}`);
  }
  console.error(
    "Usage:\n  delegation-layer install [--scope project|global] [--harness NAME[,NAME...]] [--yes] [--force]\n  delegation-layer --target ABSOLUTE_SKILL_DIR [--force]\n  delegation-layer install-cli [--version VERSION] [--install-dir ABSOLUTE_DIR]",
  );
  process.exitCode = exitCode;
}

function help() {
  console.log(
    "Usage:\n  delegation-layer install [--scope project|global] [--harness NAME[,NAME...]] [--yes] [--force]\n  delegation-layer --target ABSOLUTE_SKILL_DIR [--force]\n  delegation-layer install-cli [--version VERSION] [--install-dir ABSOLUTE_DIR]\n\nThe install command detects supported agent harnesses and installs the skill for a project or user.\nUse --harness universal when no specific harness is detected.",
  );
}

function requiredValue(argumentsList, index, option) {
  const value = argumentsList[index + 1];
  if (!value || value.startsWith("--")) {
    usage(`${option} requires a value`);
    return null;
  }
  return value;
}

function parseLegacyArguments(argumentsList) {
  let target;
  let force = false;

  for (let index = 0; index < argumentsList.length; index += 1) {
    const argument = argumentsList[index];
    if (argument === "--force") {
      force = true;
      continue;
    }
    if (argument === "--target") {
      target = requiredValue(argumentsList, index, "--target");
      if (!target) {
        return null;
      }
      index += 1;
      continue;
    }
    if (argument === "--help" || argument === "-h") {
      help();
      process.exit(0);
    }
    usage(`unknown argument ${argument}`);
    return null;
  }

  if (!target) {
    usage("--target is required");
    return null;
  }
  if (!isAbsolute(target)) {
    usage("--target must be an absolute path");
    return null;
  }
  return { force, target: resolve(target) };
}

function parseInstallArguments(argumentsList) {
  let force = false;
  let harnessNames;
  let scope;
  let yes = false;

  for (let index = 0; index < argumentsList.length; index += 1) {
    const argument = argumentsList[index];
    if (argument === "--force") {
      force = true;
      continue;
    }
    if (argument === "--yes" || argument === "-y") {
      yes = true;
      continue;
    }
    if (argument === "--scope") {
      scope = requiredValue(argumentsList, index, "--scope");
      if (!scope) {
        return null;
      }
      index += 1;
      continue;
    }
    if (argument === "--harness") {
      const value = requiredValue(argumentsList, index, "--harness");
      if (!value) {
        return null;
      }
      harnessNames = value.split(",").map((name) => name.trim().toLowerCase());
      index += 1;
      continue;
    }
    if (argument === "--help" || argument === "-h") {
      help();
      process.exit(0);
    }
    usage(`unknown argument ${argument}`);
    return null;
  }

  if (scope && scope !== "project" && scope !== "global") {
    usage("--scope must be project or global");
    return null;
  }
  if (harnessNames?.length === 0 || harnessNames?.some((name) => !name)) {
    usage("--harness requires at least one harness name");
    return null;
  }
  if (harnessNames?.some((name) => !harnessByName.has(name))) {
    const unknown = harnessNames.find((name) => !harnessByName.has(name));
    usage(`unknown harness ${unknown}; choose from ${harnesses.map((harness) => harness.name).join(", ")}`);
    return null;
  }
  return { force, harnessNames, scope, yes };
}

async function existingPath(path) {
  try {
    return await lstat(path);
  } catch (error) {
    if (error.code === "ENOENT") {
      return null;
    }
    throw error;
  }
}

function executableOnPath(command) {
  const pathEntries = (process.env.PATH ?? "").split(delimiter).filter(Boolean);
  const candidates = process.platform === "win32" ? [command, `${command}.exe`, `${command}.cmd`] : [command];
  return pathEntries.some((pathEntry) =>
    candidates.some((candidate) => {
      try {
        accessSync(join(pathEntry, candidate), constants.X_OK);
        return true;
      } catch {
        return false;
      }
    }),
  );
}

async function detectHarnesses({ cwd, home }) {
  return Promise.all(
    harnesses.map(async (harness) => {
      const reasons = [];
      for (const marker of harness.projectMarkers) {
        const markerPath = resolve(cwd, marker);
        if ((await existingPath(markerPath))?.isDirectory()) {
          reasons.push(`project ${marker}`);
          break;
        }
      }
      for (const marker of harness.globalMarkers) {
        const markerPath = resolve(home, marker);
        if ((await existingPath(markerPath))?.isDirectory()) {
          reasons.push(`global ${marker}`);
          break;
        }
      }
      for (const command of harness.commands) {
        if (executableOnPath(command)) {
          reasons.push(`${command} on PATH`);
          break;
        }
      }
      return { harness, reasons };
    }),
  );
}

function defaultHarnessNames(detections) {
  const detected = detections
    .filter(({ harness, reasons }) => harness.name !== "universal" && reasons.length > 0)
    .map(({ harness }) => harness.name);
  if (detected.length > 0) {
    return detected;
  }
  return ["universal"];
}

function formatDetected(reasons) {
  return reasons.length > 0 ? ` (detected: ${reasons.join(", ")})` : "";
}

function printDetections(detections) {
  console.log("Detected harnesses:");
  detections.forEach(({ harness, reasons }, index) => {
    console.log(`  ${index + 1}. ${harness.label}${formatDetected(reasons)}`);
  });
}

function parseHarnessSelection(value, detections) {
  const tokens = value
    .split(",")
    .map((token) => token.trim().toLowerCase())
    .filter(Boolean);
  const names = tokens.map((token) => {
    if (/^\d+$/.test(token)) {
      const detection = detections[Number(token) - 1];
      if (!detection) {
        throw new Error(`harness choice ${token} is out of range`);
      }
      return detection.harness.name;
    }
    if (!harnessByName.has(token)) {
      throw new Error(`unknown harness ${token}`);
    }
    return token;
  });
  return [...new Set(names)];
}

async function chooseInstallOptions({ force, harnessNames, scope, yes }) {
  const cwd = process.cwd();
  const home = resolve(process.env.DELEGATION_LAYER_HOME ?? homedir());
  const detections = await detectHarnesses({ cwd, home });
  const defaults = defaultHarnessNames(detections);
  const canPrompt = Boolean(process.stdin.isTTY && process.stdout.isTTY);
  const needsPrompt = !yes && !harnessNames && !scope && canPrompt;

  if (!harnessNames) {
    printDetections(detections);
  }
  if (!canPrompt && !yes && !harnessNames && !scope) {
    console.log("No interactive terminal detected; using project scope and detected harnesses.");
  }

  if (needsPrompt) {
    const readline = createInterface({ input: process.stdin, output: process.stdout });
    try {
      const selectedScope = (await readline.question("Install for [project/global] (project): ")).trim().toLowerCase();
      scope = selectedScope || "project";
      if (scope !== "project" && scope !== "global") {
        throw new Error("scope must be project or global");
      }
      const defaultSelection = defaults
        .map((name) => detections.findIndex(({ harness }) => harness.name === name) + 1)
        .join(",");
      const selection = await readline.question(`Choose harnesses [${defaultSelection}]: `);
      harnessNames = selection.trim() ? parseHarnessSelection(selection, detections) : defaults;
    } finally {
      readline.close();
    }
  }

  return {
    cwd,
    force,
    harnessNames: harnessNames ?? defaults,
    home,
    scope: scope ?? "project",
  };
}

function targetFor(harness, scope, cwd, home) {
  const root = scope === "global" ? home : cwd;
  const relativePath = scope === "global" ? harness.globalPath : harness.projectPath;
  return join(resolve(root, relativePath), "agent-integration");
}

async function install({ force, target }) {
  const targetStat = await existingPath(target);
  if (targetStat?.isSymbolicLink()) {
    throw new Error(`target directory must not be a symlink: ${target}`);
  }
  if (targetStat && !targetStat.isDirectory()) {
    throw new Error(`target must be a directory: ${target}`);
  }

  await mkdir(target, { recursive: true });

  const destination = join(target, "SKILL.md");
  const destinationStat = await existingPath(destination);
  if (destinationStat?.isSymbolicLink()) {
    throw new Error(`existing SKILL.md must not be a symlink: ${destination}`);
  }
  if (destinationStat && !destinationStat.isFile()) {
    throw new Error(`existing SKILL.md is not a regular file: ${destination}`);
  }

  if (destinationStat && !force) {
    const [source, existing] = await Promise.all([
      readFile(sourcePath),
      readFile(destination),
    ]);
    if (source.equals(existing)) {
      console.log(`Skill already installed: ${destination}`);
      return;
    }
    throw new Error(`SKILL.md already exists; use --force to replace it: ${destination}`);
  }

  await copyFile(sourcePath, destination);
  console.log(`Installed agent integration skill: ${destination}`);
}

const argumentsList = process.argv.slice(2);
if (argumentsList[0] === "install-cli") {
  try {
    const { main } = await import("./install-cli.mjs");
    await main(argumentsList.slice(1));
  } catch (error) {
    console.error(`Error: ${error.message}`);
    process.exitCode = 1;
  }
} else if (argumentsList[0] === "install") {
  const options = parseInstallArguments(argumentsList.slice(1));
  if (options) {
    try {
      const selected = await chooseInstallOptions(options);
      const targets = selected.harnessNames.map((name) => targetFor(harnessByName.get(name), selected.scope, selected.cwd, selected.home));
      const uniqueTargets = [...new Set(targets)];
      console.log(`Installing for ${selected.scope}: ${uniqueTargets.join(", ")}`);
      for (const target of uniqueTargets) {
        await install({ force: selected.force, target });
      }
    } catch (error) {
      console.error(`Error: ${error.message}`);
      process.exitCode = 1;
    }
  }
} else {
  const options = parseLegacyArguments(argumentsList);
  if (options) {
    try {
      await install(options);
    } catch (error) {
      console.error(`Error: ${error.message}`);
      process.exitCode = 1;
    }
  }
}
