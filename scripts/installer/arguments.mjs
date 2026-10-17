import { isAbsolute } from "node:path";
import { harnesses, isHarnessName } from "./catalog.mjs";
import { normalizeAbsolute } from "./paths.mjs";

function error(message) {
  return { kind: "error", message };
}

function requiredValue(argumentsList, index) {
  const value = argumentsList[index + 1];
  return value && !value.startsWith("--") ? value : null;
}

export function parseLegacyArguments(argumentsList) {
  let target;
  let force = false;

  for (let index = 0; index < argumentsList.length; index += 1) {
    const argument = argumentsList[index];
    if (argument === "--force") {
      force = true;
      continue;
    }
    if (argument === "--target") {
      target = requiredValue(argumentsList, index);
      if (!target) return error("--target requires a value");
      index += 1;
      continue;
    }
    if (argument === "--help" || argument === "-h") {
      return { kind: "help" };
    }
    return error(`unknown argument ${argument}`);
  }

  if (!target) return error("--target is required");
  if (!isAbsolute(target)) return error("--target must be an absolute path");
  return { kind: "options", options: { force, target: normalizeAbsolute(target) } };
}

export function parseInstallArguments(argumentsList) {
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
    if (["--routing", "--scope", "--harness"].includes(argument)) {
      const value = requiredValue(argumentsList, index);
      if (!value) return error(`${argument} requires a value`);
      if (argument === "--routing") routing = value;
      if (argument === "--scope") scope = value;
      if (argument === "--harness") {
        harnessNames = value.split(",").map((name) => name.trim().toLowerCase());
      }
      index += 1;
      continue;
    }
    if (argument === "--help" || argument === "-h") {
      return { kind: "help" };
    }
    return error(`unknown argument ${argument}`);
  }

  if (scope && scope !== "project" && scope !== "global") {
    return error("--scope must be project or global");
  }
  if (harnessNames?.length === 0 || harnessNames?.some((name) => !name)) {
    return error("--harness requires at least one harness name");
  }
  const unknownHarness = harnessNames?.find((name) => !isHarnessName(name));
  if (unknownHarness) {
    return error(`unknown harness ${unknownHarness}; choose from ${harnesses.map((harness) => harness.name).join(", ")}`);
  }
  if (routing && !["default", "skill-only"].includes(routing)) {
    return error("--routing must be default or skill-only");
  }
  return { kind: "options", options: { force, harnessNames, scope, yes, routing } };
}

export function parseInstallCliArguments(argumentsList, { platform }) {
  let version;
  let installDir;

  for (let index = 0; index < argumentsList.length; index += 1) {
    const argument = argumentsList[index];
    if (argument === "--version") {
      version = argumentsList[index + 1];
      index += 1;
      if (!version) return error("--version requires a value");
      continue;
    }
    if (argument === "--install-dir") {
      installDir = argumentsList[index + 1];
      index += 1;
      if (!installDir) return error("--install-dir requires a value");
      if (!isAbsolute(installDir)) return error("--install-dir must be an absolute path");
      continue;
    }
    if (argument === "--help" || argument === "-h") {
      return { kind: "help" };
    }
    return error(`unknown argument ${argument}`);
  }

  if (platform === "win32") {
    return error("the release installer supports Linux and macOS only");
  }
  return {
    kind: "options",
    options: { installDir: installDir ? normalizeAbsolute(installDir) : undefined, version },
  };
}
