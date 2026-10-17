import { open, mkdir, readFile, rename, unlink } from "node:fs/promises";
import { dirname, join } from "node:path";
import { getHarness } from "./catalog.mjs";
import { assertNoSymlinkComponents, existingPath } from "./filesystem.mjs";
import { countRoutingEvents, createRoutingPlan, routingPath, validateHookConfig } from "./routing-rules.mjs";
import { targetFor } from "./selection.mjs";

export async function prepareRouting(selected, action, { environment, platform, sourceDirectory }) {
  const plans = [];
  for (const name of new Set(selected.harnessNames)) {
    const path = routingPath(name, {
      scope: selected.scope,
      cwd: selected.cwd,
      home: selected.home,
      claudeConfigDir: environment.CLAUDE_CONFIG_DIR,
      codexHome: environment.CODEX_HOME,
    });
    if (!path || platform === "win32") {
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
    const harness = getHarness(name);
    if (!harness) throw new Error(`cannot prepare routing for unknown harness: ${name}`);
    const target = targetFor(harness, selected.scope, selected.cwd, selected.home, environment);
    let assetsPresent = true;
    if (action === "status" && countRoutingEvents(config, target) > 0) {
      for (const relative of ["SKILL.md", "scripts/delegate-routing-context.sh", "references/routing-rule.json"]) {
        const asset = await existingPath(join(target, relative));
        if (!asset?.isFile() || asset.isSymbolicLink()) assetsPresent = false;
      }
    }
    plans.push(createRoutingPlan({
      name,
      path,
      target,
      before,
      config,
      mode: stat ? stat.mode & 0o777 : 0o600,
      action,
      assetsPresent,
    }));
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
    if (error && typeof error === "object" && "code" in error && error.code === "EEXIST") {
      throw new Error(`routing configuration is locked: ${lockPath}; verify no other installer is running before removing a stale lock`);
    }
    throw error;
  }
  const suffix = `${process.pid}-${Date.now()}-${Math.random().toString(16).slice(2)}`;
  const temporary = plan.path + `.delegation-layer-stage-${suffix}`;
  try {
    const stat = await existingPath(plan.path);
    if (stat && (!stat.isFile() || stat.isSymbolicLink())) {
      throw new Error(`routing configuration changed during installation: ${plan.path}`);
    }
    const current = stat ? await readFile(plan.path, "utf8") : null;
    if (current !== before) throw new Error(`routing configuration changed during installation; retry: ${plan.path}`);
    if (before !== null) {
      const backup = await open(plan.path + `.delegation-layer-backup-${suffix}`, "wx", 0o600);
      try {
        await backup.writeFile(before);
        await backup.sync();
      } finally {
        await backup.close();
      }
    }
    if (after === null) {
      await unlink(plan.path);
      return;
    }
    const staged = await open(temporary, "wx", plan.mode);
    try {
      await staged.writeFile(after);
      await staged.sync();
    } finally {
      await staged.close();
    }
    await rename(temporary, plan.path);
  } finally {
    try {
      try {
        await unlink(temporary);
      } catch (error) {
        if (!(error && typeof error === "object" && "code" in error && error.code === "ENOENT")) throw error;
      }
    } finally {
      try {
        await lock.close();
      } finally {
        await unlink(lockPath);
      }
    }
  }
}

export async function applyRouting(plans) {
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
      try {
        await replaceRoutingConfig(plan, plan.after, plan.before);
      } catch (rollbackError) {
        failures.push(rollbackError instanceof Error ? rollbackError.message : String(rollbackError));
      }
    }
    const reason = error instanceof Error ? error.message : String(error);
    throw new Error(`${reason}${failures.length ? `; rollback needs attention: ${failures.join("; ")}` : "; earlier routing changes rolled back"}`);
  }
}

export function printRouting(plan, action, report) {
  if (!plan.path) {
    report(`${plan.name}: unsupported-host for automatic routing; skill-only guidance remains available.`);
    return;
  }
  report(`${plan.name}: ${plan.status} (${plan.path}).`);
  if (plan.status === "configured") {
    report("Activation not verified: start or resume a session and check the native hook output. Installation is not proof of model compliance.");
    if (plan.name === "codex") report("Codex: review and trust the hooks using /hooks. New or changed definitions are skipped until trusted.");
  }
  if (action === "disable") report("Only this installer's matching hooks were removed; plugin hooks are managed by the host plugin manager.");
}
