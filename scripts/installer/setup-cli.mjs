import { access, constants, stat } from "node:fs/promises";
import { delimiter, dirname, isAbsolute, join, resolve } from "node:path";
import { existingPath } from "./filesystem.mjs";
import { normalizeAbsolute } from "./paths.mjs";

const bundleRoles = Object.freeze(["delegate", "delegate-run", "pueue", "pueued"]);

async function delegateOnPath(path, cwd) {
  for (const entry of path.split(delimiter).filter(Boolean)) {
    const candidate = resolve(cwd, entry, "delegate");
    // An existing but broken PATH entry must not authorize another installation.
    try {
      if (await existingPath(candidate)) return candidate;
    } catch (error) {
      if (error && typeof error === "object" && "code" in error && ["ENOTDIR", "EACCES"].includes(error.code)) continue;
      throw error;
    }
  }
  return null;
}

export async function inspectSetupCli({ installDir, environment, home, cwd }) {
  const explicitDir = installDir ?? (environment.DELEGATION_LAYER_INSTALL_DIR || undefined);
  if (explicitDir !== undefined && (!explicitDir || !isAbsolute(explicitDir))) {
    throw new Error("DELEGATION_LAYER_INSTALL_DIR must be an absolute path");
  }
  let pathSelection;
  try {
    pathSelection = { kind: "observed", executable: await delegateOnPath(environment.PATH ?? "", cwd) };
  } catch (error) {
    if (!explicitDir) throw error;
    // The selected directory owns CLI selection. An unreadable advisory PATH
    // cannot invalidate it, and cannot establish which CLI the shell will use.
    pathSelection = { kind: "unknown" };
  }
  const pathDelegate = pathSelection.kind === "observed" ? pathSelection.executable : null;
  const directory = explicitDir ? normalizeAbsolute(explicitDir) : pathDelegate ? dirname(pathDelegate) : join(home, ".local", "bin");
  const executable = join(directory, "delegate");
  const existing = await existingPath(executable);
  if (existing) {
    try {
      if (!(await stat(executable)).isFile()) throw new Error("not a regular executable");
      await access(executable, constants.X_OK);
    } catch (cause) {
      throw new Error(`Selected Delegate is unusable: ${executable}; repair it using its original installation method or choose an empty --install-dir`, { cause });
    }
  } else {
    const present = [];
    for (const role of bundleRoles) {
      if (await existingPath(join(directory, role))) present.push(role);
    }
    if (present.length) throw new Error(`Partial CLI installation in ${directory}; existing roles: ${present.join(", ")}. Choose an empty --install-dir; setup never overwrites CLI files.`);
  }
  return { executable, directory, absent: !existing, pathSelection, pathAction: pathSelection.kind === "unknown" || pathDelegate !== executable };
}
