#!/usr/bin/env node

import { accessSync, constants } from "node:fs";
import { copyFile, lstat, mkdir, open, readdir, readFile, rename, unlink } from "node:fs/promises";
import { emitKeypressEvents } from "node:readline";
import { delimiter, dirname, isAbsolute, join, resolve } from "node:path";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";

const packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const sourceDirectory = join(packageRoot, "skills", "agent-integration");
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
    "Usage:\n  delegation-layer install [--scope project|global] [--harness NAME[,NAME...]] [--yes] [--force] [--routing default|skill-only]\n  delegation-layer routing status|disable [--scope project|global] [--harness NAME[,NAME...]]\n  delegation-layer --target ABSOLUTE_SKILL_DIR [--force]\n  delegation-layer install-cli [--version VERSION] [--install-dir ABSOLUTE_DIR]",
  );
  process.exitCode = exitCode;
}

function help() {
  console.log(
    "Usage:\n  delegation-layer install [--scope project|global] [--harness NAME[,NAME...]] [--yes] [--force] [--routing default|skill-only]\n  delegation-layer routing status|disable [--scope project|global] [--harness NAME[,NAME...]]\n  delegation-layer --target ABSOLUTE_SKILL_DIR [--force]\n  delegation-layer install-cli [--version VERSION] [--install-dir ABSOLUTE_DIR]\n\nThe install command detects supported agent harnesses and installs the skill for a project or user.\nGlobal scope is the default; use --scope project for a project-local install.\nUse --harness universal when no specific harness is detected.",
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
  let routing;

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
    if (argument === "--routing") {
      routing = requiredValue(argumentsList, index, "--routing");
      if (!routing) return null;
      index += 1;
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
  if (routing && !["default", "skill-only"].includes(routing)) {
    usage("--routing must be default or skill-only");
    return null;
  }
  return { force, harnessNames, scope, yes, routing };
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

async function assertNoSymlinkComponents(path) {
  let current = resolve(path);
  while (true) {
    const stat = await existingPath(current);
    if (stat?.isSymbolicLink()) {
      throw new Error(`skill destination must not contain symlink components: ${current}`);
    }
    const parent = dirname(current);
    if (parent === current) {
      return;
    }
    current = parent;
  }
}

async function skillFiles(directory, relative = "") {
  const entries = await readdir(join(directory, relative), { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const child = join(relative, entry.name);
    const path = join(directory, child);
    const stat = await lstat(path);
    if (stat.isSymbolicLink()) {
      throw new Error(`skill source or destination must not contain symlinks: ${path}`);
    }
    if (stat.isDirectory()) {
      files.push(...(await skillFiles(directory, child)));
      continue;
    }
    if (!stat.isFile()) {
      throw new Error(`skill entry must be a regular file: ${path}`);
    }
    files.push(child);
  }
  return files.sort();
}

async function skillTreeMatches(target, files) {
  for (const relative of files) {
    const destination = join(target, relative);
    await assertNoSymlinkComponents(destination);
    const stat = await existingPath(destination);
    if (!stat || !stat.isFile() || stat.isSymbolicLink()) {
      return false;
    }
    const [source, existing] = await Promise.all([
      readFile(join(sourceDirectory, relative)),
      readFile(destination),
    ]);
    if (!source.equals(existing)) {
      return false;
    }
  }
  return true;
}

async function skillTreeHasExistingFiles(target, files) {
  for (const relative of files) {
    const destination = join(target, relative);
    await assertNoSymlinkComponents(destination);
    if (await existingPath(destination)) {
      return true;
    }
  }
  return false;
}

async function copySkillTree(target, files, force) {
  for (const relative of files) {
    const destination = join(target, relative);
    const parent = dirname(destination);
    await assertNoSymlinkComponents(destination);
    await mkdir(parent, { recursive: true });
    const stat = await existingPath(destination);
    if (stat?.isSymbolicLink()) {
      throw new Error(`existing skill file must not be a symlink: ${destination}`);
    }
    if (stat && !stat.isFile()) {
      throw new Error(`existing skill entry is not a regular file: ${destination}`);
    }
    if (stat && !force) {
      throw new Error(`skill files already exist; use --force to replace them: ${target}`);
    }
    await copyFile(join(sourceDirectory, relative), destination);
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

function renderPicker({ error, index, multiple, options, selected, title }) {
  const lines = [title, ""];
  options.forEach((option, optionIndex) => {
    const current = optionIndex === index;
    const marker = multiple ? (selected.has(option.value) ? "[x]" : "[ ]") : "";
    const cursor = current ? "❯" : " ";
    const color = current ? "\u001b[36m" : "";
    const reset = current ? "\u001b[0m" : "";
    lines.push(`${color}${cursor} ${marker ? `${marker} ` : ""}${option.label}${reset}`);
    if (option.description) {
      lines.push(`      ${option.description}`);
    }
  });
  lines.push("");
  lines.push(multiple ? "↑↓ move  Space select  Enter continue  a all  n none  q cancel" : "↑↓ move  Enter select  q cancel");
  if (error) {
    lines.push(`\u001b[31m${error}\u001b[0m`);
  }
  process.stdout.write(`\u001b[2J\u001b[H${lines.join("\n")}\n`);
}

function pickOptions({ initialValues = [], multiple = false, options, title }) {
  const input = process.stdin;
  const output = process.stdout;
  if (!input.isTTY || !output.isTTY || typeof input.setRawMode !== "function") {
    return null;
  }

  let index = 0;
  const selected = new Set(initialValues);

  return new Promise((resolve, reject) => {
    let settled = false;
    let error;

    const cleanup = () => {
      input.off("keypress", onKeypress);
      try {
        input.setRawMode(false);
      } catch {
        // The terminal may already be closed while handling cancellation.
      }
      input.pause();
      output.write("\u001b[?25h\u001b[0m");
    };

    const finish = (value) => {
      if (settled) {
        return;
      }
      settled = true;
      cleanup();
      resolve(value);
    };

    const onKeypress = (character, key = {}) => {
      if ((key.ctrl && key.name === "c") || key.name === "escape" || key.name === "q") {
        finish(null);
        return;
      }
      if (key.name === "up" || key.name === "k") {
        index = (index + options.length - 1) % options.length;
        error = undefined;
        renderPicker({ error, index, multiple, options, selected, title });
        return;
      }
      if (key.name === "down" || key.name === "j") {
        index = (index + 1) % options.length;
        error = undefined;
        renderPicker({ error, index, multiple, options, selected, title });
        return;
      }
      if (multiple && (key.name === "space" || character === " ")) {
        const value = options[index].value;
        if (selected.has(value)) {
          selected.delete(value);
        } else {
          selected.add(value);
        }
        error = undefined;
        renderPicker({ error, index, multiple, options, selected, title });
        return;
      }
      if (multiple && key.name === "a") {
        options.forEach((option) => selected.add(option.value));
        error = undefined;
        renderPicker({ error, index, multiple, options, selected, title });
        return;
      }
      if (multiple && key.name === "n") {
        selected.clear();
        error = undefined;
        renderPicker({ error, index, multiple, options, selected, title });
        return;
      }
      if (key.name === "return" || key.name === "enter") {
        if (multiple && selected.size === 0) {
          error = "Select at least one harness, or press q to cancel.";
          renderPicker({ error, index, multiple, options, selected, title });
          return;
        }
        finish(multiple ? options.filter((option) => selected.has(option.value)).map((option) => option.value) : options[index].value);
      }
    };

    try {
      emitKeypressEvents(input);
      input.setRawMode(true);
      input.resume();
      input.on("keypress", onKeypress);
      output.write("\u001b[?25l");
      renderPicker({ error, index, multiple, options, selected, title });
    } catch (pickerError) {
      cleanup();
      reject(pickerError);
    }
  });
}

async function chooseInstallOptions({ force, harnessNames, scope, yes, routing }) {
  const cwd = process.cwd();
  const home = resolve(process.env.DELEGATION_LAYER_HOME ?? homedir());
  const detections = await detectHarnesses({ cwd, home });
  const defaults = defaultHarnessNames(detections);
  const canPrompt = Boolean(process.stdin.isTTY && process.stdout.isTTY);
  const needsPrompt = !yes && !harnessNames && !scope && canPrompt;

  if (!harnessNames && !needsPrompt) {
    printDetections(detections);
  }
  if (!canPrompt && !yes && !harnessNames && !scope) {
    console.log("No interactive terminal detected; using global scope and detected harnesses.");
  }

  if (needsPrompt) {
    scope = await pickOptions({
      initialValues: ["global"],
      options: [
        {
          value: "global",
          label: "Global",
          description: `available to your agents from ${home}`,
        },
        {
          value: "project",
          label: "Project",
          description: `available to this project from ${cwd}`,
        },
      ],
      title: "Where should the agent skill be installed?",
    });
    if (!scope) {
      return null;
    }
    harnessNames = await pickOptions({
      initialValues: defaults,
      multiple: true,
      options: detections.map(({ harness, reasons }) => ({
        value: harness.name,
        label: `${harness.label}${formatDetected(reasons)}`,
        description: targetFor(harness, scope, cwd, home),
      })),
      title: "Which harnesses should use the skill?",
    });
    if (!harnessNames) {
      return null;
    }
  }

  if (needsPrompt && !routing) {
    routing = await pickOptions({
      initialValues: ["default"],
      options: [
        { value: "default", label: "Prefer Delegate", description: "Install collaboration context hooks for Claude and Codex; native hook trust still applies" },
        { value: "skill-only", label: "Skill only", description: "Install guidance without adding hooks; keep existing routing settings" },
      ],
      title: "How should external collaboration be routed?",
    });
    if (!routing) return null;
  }

  return {
    routing: routing ?? "skill-only",
    cwd,
    force,
    harnessNames: harnessNames ?? defaults,
    home,
    scope: scope ?? "global",
  };
}

function targetFor(harness, scope, cwd, home) {
  if (scope === "global" && harness.name === "claude" && process.env.CLAUDE_CONFIG_DIR) {
    return join(resolve(process.env.CLAUDE_CONFIG_DIR), "skills", "agent-integration");
  }
  const root = scope === "global" ? home : cwd;
  const relativePath = scope === "global" ? harness.globalPath : harness.projectPath;
  return join(resolve(root, relativePath), "agent-integration");
}

async function install({ force, target }) {
  await assertNoSymlinkComponents(target);
  const targetStat = await existingPath(target);
  if (targetStat?.isSymbolicLink()) {
    throw new Error(`target directory must not be a symlink: ${target}`);
  }
  if (targetStat && !targetStat.isDirectory()) {
    throw new Error(`target must be a directory: ${target}`);
  }

  const files = await skillFiles(sourceDirectory);
  await mkdir(target, { recursive: true });

  if (!force && (await skillTreeMatches(target, files))) {
    console.log(`Skill already installed: ${join(target, "SKILL.md")}`);
    return;
  }
  if (!force && (await skillTreeHasExistingFiles(target, files))) {
    throw new Error(`skill files already exist; use --force to replace them: ${target}`);
  }

  await copySkillTree(target, files, force);
  console.log(`Installed agent integration skill: ${join(target, "SKILL.md")}`);
}

function shellQuote(value) {
  return "'" + value.replaceAll("'", "'\\''") + "'";
}

function routingPath(name, { scope, cwd, home }) {
  if (name === "claude") {
    return scope === "global"
      ? join(resolve(process.env.CLAUDE_CONFIG_DIR || join(home, ".claude")), "settings.json")
      : join(cwd, ".claude", "settings.local.json");
  }
  if (name === "codex") {
    return join(scope === "global" ? resolve(process.env.CODEX_HOME || join(home, ".codex")) : join(cwd, ".codex"), "hooks.json");
  }
  return null;
}

function jsonObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function validateHookConfig(config, path) {
  if (!jsonObject(config) || (config.hooks !== undefined && !jsonObject(config.hooks))) {
    throw new Error(`expected a JSON object with an optional hooks object: ${path}`);
  }
  for (const groups of Object.values(config.hooks ?? {})) {
    if (!Array.isArray(groups) || groups.some((group) => !jsonObject(group)
      || !Array.isArray(group.hooks) || group.hooks.some((hook) => !jsonObject(hook) || typeof hook.type !== "string"))) {
      throw new Error(`invalid hook configuration; no routing changes written: ${path}`);
    }
  }
}

async function prepareRouting(selected, action) {
  const plans = [];
  for (const name of new Set(selected.harnessNames)) {
    const path = routingPath(name, selected);
    if (!path || process.platform === "win32") {
      plans.push({ name, status: "unsupported-host" });
      continue;
    }
    await assertNoSymlinkComponents(path);
    const stat = await existingPath(path);
    if (stat && !stat.isFile()) throw new Error(`routing configuration must be a regular file: ${path}`);
    const before = stat ? await readFile(path, "utf8") : null;
    let config;
    try {
      config = before === null ? {} : JSON.parse(before);
    } catch {
      throw new Error(`invalid JSON; routing configuration left unchanged: ${path}`);
    }
    validateHookConfig(config, path);
    const target = targetFor(harnessByName.get(name), selected.scope, selected.cwd, selected.home);
    const script = join(target, "scripts", "delegate-routing-context.sh");
    const hooks = config.hooks ?? {};
    let present = 0;
    let changed = false;
    for (const event of ["SessionStart", "SubagentStart"]) {
      const command = `/bin/sh ${shellQuote(script)} ${event}`;
      const groups = hooks[event] ?? [];
      const matches = groups.flatMap((group) => group.hooks).filter((hook) => hook.type === "command" && hook.command === command);
      if (matches.length) present += 1;
      if (action === "disable") {
        if (!matches.length) continue;
        hooks[event] = groups.flatMap((group) => {
          const remaining = group.hooks.filter((hook) => hook.type !== "command" || hook.command !== command);
          if (remaining.length === group.hooks.length) return [group];
          return remaining.length ? [{ ...group, hooks: remaining }] : [];
        });
        if (!hooks[event].length) delete hooks[event];
        changed = true;
      } else if (action === "enable" && !matches.length) {
        hooks[event] = [...groups, { hooks: [{ type: "command", command }] }];
        changed = true;
      }
    }
    if (changed) config.hooks = hooks;
    let assetsPresent = true;
    if (action === "status" && present) {
      for (const relative of ["SKILL.md", "scripts/delegate-routing-context.sh", "references/routing-rule.json"]) {
        const asset = await existingPath(join(target, relative));
        if (!asset?.isFile() || asset.isSymbolicLink()) assetsPresent = false;
      }
    }
    plans.push({ name, path, target, before, after: changed ? JSON.stringify(config, null, 2) + "\n" : before,
      mode: stat ? stat.mode & 0o777 : 0o600,
      status: action === "disable" ? "disabled" : action === "enable" || (present === 2 && assetsPresent) ? "configured" : present ? "incomplete" : "not-configured" });
  }
  return plans;
}

async function replaceRoutingConfig(plan, before, after) {
  await assertNoSymlinkComponents(plan.path);
  await mkdir(dirname(plan.path), { recursive: true });
  const lockPath = plan.path + ".delegation-layer.lock";
  let lock;
  try {
    lock = await open(lockPath, "wx", 0o600);
  } catch (error) {
    if (error.code === "EEXIST") throw new Error(`routing configuration is locked: ${lockPath}; verify no other installer is running before removing a stale lock`);
    throw error;
  }
  const suffix = `${process.pid}-${Date.now()}-${Math.random().toString(16).slice(2)}`;
  const temporary = plan.path + `.delegation-layer-stage-${suffix}`;
  try {
    const stat = await existingPath(plan.path);
    if (stat && (!stat.isFile() || stat.isSymbolicLink())) throw new Error(`routing configuration changed during installation: ${plan.path}`);
    const current = stat ? await readFile(plan.path, "utf8") : null;
    if (current !== before) throw new Error(`routing configuration changed during installation; retry: ${plan.path}`);
    if (before !== null) {
      const backup = await open(plan.path + `.delegation-layer-backup-${suffix}`, "wx", 0o600);
      try { await backup.writeFile(before); await backup.sync(); } finally { await backup.close(); }
    }
    if (after === null) {
      await unlink(plan.path);
      return;
    }
    const staged = await open(temporary, "wx", plan.mode);
    try { await staged.writeFile(after); await staged.sync(); } finally { await staged.close(); }
    await rename(temporary, plan.path);
  } finally {
    try {
      try { await unlink(temporary); } catch (error) { if (error.code !== "ENOENT") throw error; }
    } finally {
      try { await lock.close(); } finally { await unlink(lockPath); }
    }
  }
}

async function applyRouting(plans) {
  const applied = [];
  try {
    for (const plan of plans) {
      if (!plan.path || plan.before === plan.after) continue;
      await replaceRoutingConfig(plan, plan.before, plan.after);
      applied.push(plan);
    }
  } catch (error) {
    const failures = [];
    for (const plan of applied.reverse()) {
      try { await replaceRoutingConfig(plan, plan.after, plan.before); }
      catch (rollbackError) { failures.push(rollbackError.message); }
    }
    throw new Error(`${error.message}${failures.length ? `; rollback needs attention: ${failures.join("; ")}` : "; earlier routing changes rolled back"}`);
  }
}

function printRouting(plan, action) {
  if (!plan.path) {
    console.log(`${plan.name}: unsupported-host for automatic routing; skill-only guidance remains available.`);
    return;
  }
  console.log(`${plan.name}: ${plan.status} (${plan.path}).`);
  if (plan.status === "configured") {
    console.log("Activation not verified: start or resume a session and check the native hook output. Installation is not proof of model compliance.");
    if (plan.name === "codex") console.log("Codex: review and trust the hooks using /hooks. New or changed definitions are skipped until trusted.");
  }
  if (action === "disable") console.log("Only this installer's matching hooks were removed; plugin hooks are managed by the host plugin manager.");
}

async function validateSourceTree() {
  const files = await skillFiles(sourceDirectory);
  if (!files.includes("SKILL.md")) {
    throw new Error(`skill package is missing SKILL.md: ${sourceDirectory}`);
  }
  return files;
}

await validateSourceTree();

const argumentsList = process.argv.slice(2);
if (argumentsList[0] === "install-cli") {
  try {
    const { main } = await import("./install-cli.mjs");
    await main(argumentsList.slice(1));
  } catch (error) {
    console.error(`Error: ${error.message}`);
    process.exitCode = 1;
  }
} else if (argumentsList[0] === "routing") {
  const action = argumentsList[1];
  const options = parseInstallArguments(argumentsList.slice(2));
  if (!["status", "disable"].includes(action)) {
    usage("routing requires status or disable");
  } else if (options) {
    try {
      if (options.routing || options.force) throw new Error("routing status/disable do not accept --routing or --force");
      const selected = await chooseInstallOptions({ ...options, yes: true, harnessNames: options.harnessNames ?? ["claude", "codex"] });
      const plans = await prepareRouting(selected, action);
      if (action === "disable") await applyRouting(plans);
      for (const plan of plans) printRouting(plan, action);
    } catch (error) {
      console.error(`Error: ${error.message}`);
      process.exitCode = 1;
    }
  }
} else if (argumentsList[0] === "install") {
  const options = parseInstallArguments(argumentsList.slice(1));
  if (options) {
    try {
      const selected = await chooseInstallOptions(options);
      if (!selected) {
        console.log("Installation cancelled.");
      } else {
        const routingPlans = selected.routing === "default" ? await prepareRouting(selected, "enable") : [];
        const targets = selected.harnessNames.map((name) => targetFor(harnessByName.get(name), selected.scope, selected.cwd, selected.home));
        const uniqueTargets = [...new Set(targets)];
        console.log(`Installing for ${selected.scope}: ${uniqueTargets.join(", ")}`);
        for (const target of uniqueTargets) {
          await install({ force: selected.force, target });
        }
        await applyRouting(routingPlans);
        for (const plan of routingPlans) printRouting(plan, "enable");
        if (selected.routing === "skill-only") console.log("Skill-only installation; existing routing settings were not changed.");
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
