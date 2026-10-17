#!/usr/bin/env node

import { dirname, resolve } from "node:path";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";
import { runInstaller } from "./installer/application.mjs";
import { main as installCliMain } from "./install-cli.mjs";

const packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
process.exitCode = await runInstaller({
  argumentsList: process.argv.slice(2),
  packageRoot,
  cwd: process.cwd(),
  home: resolve(process.env.DELEGATION_LAYER_HOME ?? homedir()),
  environment: process.env,
  platform: process.platform,
  input: process.stdin,
  output: process.stdout,
  errorOutput: process.stderr,
  installCliMain,
});
