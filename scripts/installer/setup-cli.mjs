import { access, constants, stat } from "node:fs/promises";
import { delimiter, dirname, isAbsolute, join, resolve } from "node:path";
import { existingPath } from "./filesystem.mjs";
import { normalizeAbsolute } from "./paths.mjs";

const bundleRoles = Object.freeze(["delegate", "delegate-run", "pueue", "pueued"]);

async function delegateOnPath(path, cwd) {
  for (const entry of path.split(delimiter)) {
    const candidate = resolve(cwd, entry, "delegate");
    // An existing but broken PATH entry must not authorize another installation.
    if (await existingPath(candidate)) return candidate;
  }
  return null;
}

export async function inspectSetupCli({ installDir, environment, home, cwd }) {
  const explicitDir = installDir ?? environment.DELEGATION_LAYER_INSTALL_DIR;
  if (explicitDir !== undefined && (!explicitDir || !isAbsolute(explicitDir))) {
    throw new Error("DELEGATION_LAYER_INSTALL_DIR must be an absolute path");
  }
  const pathDelegate = await delegateOnPath(environment.PATH ?? "", cwd);
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
  return { executable, directory, absent: !existing, pathDelegate, pathAction: pathDelegate !== executable };
}
