#!/usr/bin/env node

import { spawn } from "node:child_process";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const installerPath = join(packageRoot, "install.sh");

function usage(message) {
  if (message) {
    console.error(`Error: ${message}`);
  }
  console.error(
    "Usage: delegation-layer install-cli [--version VERSION] [--install-dir ABSOLUTE_DIR]",
  );
}

function parseArguments(argumentsList) {
  let version;
  let installDir;

  for (let index = 0; index < argumentsList.length; index += 1) {
    const argument = argumentsList[index];
    if (argument === "--version") {
      version = argumentsList[index + 1];
      index += 1;
      if (!version) {
        throw new Error("--version requires a value");
      }
      continue;
    }
    if (argument === "--install-dir") {
      installDir = argumentsList[index + 1];
      index += 1;
      if (!installDir) {
        throw new Error("--install-dir requires a value");
      }
      if (!isAbsolute(installDir)) {
        throw new Error("--install-dir must be an absolute path");
      }
      continue;
    }
    if (argument === "--help" || argument === "-h") {
      usage();
      return null;
    }
    throw new Error(`unknown argument ${argument}`);
  }

  if (process.platform === "win32") {
    throw new Error("the release installer supports Linux and macOS only");
  }

  return { installDir: installDir ? resolve(installDir) : undefined, version };
}

export function main(argumentsList) {
  const options = parseArguments(argumentsList);
  if (!options) {
    return Promise.resolve();
  }

  const environment = { ...process.env };
  if (options.version) {
    environment.DELEGATION_LAYER_VERSION = options.version;
  }
  if (options.installDir) {
    environment.DELEGATION_LAYER_INSTALL_DIR = options.installDir;
  }

  return new Promise((resolvePromise, reject) => {
    const child = spawn("sh", [installerPath], {
      env: environment,
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
