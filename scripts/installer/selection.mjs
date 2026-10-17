import { join } from "node:path";
import { resolveFrom } from "./paths.mjs";

export function defaultHarnessNames(detections) {
  const detected = detections
    .filter(({ harness, reasons }) => harness.name !== "universal" && reasons.length > 0)
    .map(({ harness }) => harness.name);
  return detected.length > 0 ? detected : ["universal"];
}

export function formatDetected(reasons) {
  return reasons.length > 0 ? ` (detected: ${reasons.join(", ")})` : "";
}

export function resolveInstallSelection({ force, harnessNames, scope, routing, cwd, home, detections }) {
  return {
    routing: routing ?? "skill-only",
    cwd,
    force,
    harnessNames: harnessNames ?? defaultHarnessNames(detections),
    home,
    scope: scope ?? "global",
  };
}

export function targetFor(harness, scope, cwd, home, environment = {}) {
  if (!harness) throw new Error("cannot resolve an install target for an unknown harness");
  if (scope === "global" && harness.name === "claude" && environment.CLAUDE_CONFIG_DIR) {
    return join(resolveFrom(cwd, environment.CLAUDE_CONFIG_DIR), "skills", "agent-integration");
  }
  const root = scope === "global" ? home : cwd;
  const relativePath = scope === "global" ? harness.globalPath : harness.projectPath;
  return join(root, ...relativePath, "agent-integration");
}
