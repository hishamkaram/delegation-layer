#!/usr/bin/env node

import { spawn } from "node:child_process";
import { join } from "node:path";
import { parseInstallCliArguments } from "./installer/arguments.mjs";

const usageText = "Usage: delegation-layer install-cli [--version VERSION] [--install-dir ABSOLUTE_DIR]";

export async function main(argumentsList, {
  packageRoot,
  environment = process.env,
  platform = process.platform,
  spawnProcess = spawn,
  errorOutput = process.stderr,
} = {}) {
  const parsed = parseInstallCliArguments(argumentsList, { platform });
  if (parsed.kind === "error") throw new Error(parsed.message);
  if (parsed.kind === "help") {
    errorOutput.write(`${usageText}\n`);
    return;
  }
  if (!packageRoot) throw new Error("install-cli requires a package root");

  const childEnvironment = { ...environment };
  if (parsed.options.version) {
    childEnvironment.DELEGATION_LAYER_VERSION = parsed.options.version;
  }
  if (parsed.options.installDir) {
    childEnvironment.DELEGATION_LAYER_INSTALL_DIR = parsed.options.installDir;
  }

  await new Promise((resolvePromise, reject) => {
    const child = spawnProcess("sh", [join(packageRoot, "install.sh")], {
      env: childEnvironment,
      stdio: "inherit",
    });
    child.on("error", reject);
    child.on("exit", (code, signal) => {
      if (code === 0) {
        resolvePromise();
        return;
      }
      reject(new Error(`CLI installer exited with ${signal ?? `status ${code}`}`));
    });
  });
}
