import { join } from "node:path";
import { resolveFrom } from "./paths.mjs";

const ROUTING_EVENTS = ["SessionStart", "SubagentStart"];

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function shellQuote(value) {
  return "'" + value.replaceAll("'", "'\\''") + "'";
}

function hookCommand(target, event) {
  return `/bin/sh ${shellQuote(join(target, "scripts", "delegate-routing-context.sh"))} ${event}`;
}

export function routingPath(name, { scope, cwd, home, claudeConfigDir, codexHome }) {
  if (name === "claude") {
    return scope === "global"
      ? join(resolveFrom(cwd, claudeConfigDir || join(home, ".claude")), "settings.json")
      : join(cwd, ".claude", "settings.local.json");
  }
  if (name === "codex") {
    return join(scope === "global" ? resolveFrom(cwd, codexHome || join(home, ".codex")) : join(cwd, ".codex"), "hooks.json");
  }
  return null;
}

export function validateHookConfig(config, path) {
  if (!isObject(config) || (config.hooks !== undefined && !isObject(config.hooks))) {
    throw new Error(`expected a JSON object with an optional hooks object: ${path}`);
  }
  for (const groups of Object.values(config.hooks ?? {})) {
    if (!Array.isArray(groups) || groups.some((group) => !isObject(group)
      || !Array.isArray(group.hooks) || group.hooks.some((hook) => !isObject(hook) || typeof hook.type !== "string"))) {
      throw new Error(`invalid hook configuration; no routing changes written: ${path}`);
    }
  }
}

export function countRoutingEvents(config, target) {
  const hooks = config.hooks ?? {};
  return ROUTING_EVENTS.filter((event) => {
    const expected = hookCommand(target, event);
    return (hooks[event] ?? []).some((group) => group.hooks.some(
      (hook) => hook.type === "command" && hook.command === expected,
    ));
  }).length;
}

export function createRoutingPlan({ name, path, target, before, config, mode, action, assetsPresent = true }) {
  validateHookConfig(config, path);
  const nextConfig = { ...config };
  const hooks = { ...(config.hooks ?? {}) };
  let changed = false;
  let present = 0;

  for (const event of ROUTING_EVENTS) {
    const command = hookCommand(target, event);
    const groups = hooks[event] ?? [];
    const matches = groups.flatMap((group) => group.hooks)
      .filter((hook) => hook.type === "command" && hook.command === command);
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

  if (!["enable", "disable", "status"].includes(action)) {
    throw new Error(`unsupported routing action: ${action}`);
  }
  if (changed) nextConfig.hooks = hooks;
  return {
    name,
    path,
    target,
    before,
    after: changed ? `${JSON.stringify(nextConfig, null, 2)}\n` : before,
    mode,
    status: action === "disable"
      ? "disabled"
      : action === "enable" || (present === 2 && assetsPresent)
        ? "configured"
        : present
          ? "incomplete"
          : "not-configured",
  };
}
