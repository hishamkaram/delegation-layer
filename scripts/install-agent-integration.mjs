#!/usr/bin/env node

import { copyFile, lstat, mkdir, readFile } from "node:fs/promises";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const sourcePath = join(packageRoot, "skills", "agent-integration", "SKILL.md");

function usage(message) {
  if (message) {
    console.error(`Error: ${message}`);
  }
  console.error(
    "Usage:\n  delegation-layer --target ABSOLUTE_SKILL_DIR [--force]\n  delegation-layer install-cli [--version VERSION] [--install-dir ABSOLUTE_DIR]",
  );
  process.exitCode = 2;
}

function parseArguments(argumentsList) {
  let target;
  let force = false;

  for (let index = 0; index < argumentsList.length; index += 1) {
    const argument = argumentsList[index];
    if (argument === "--force") {
      force = true;
      continue;
    }
    if (argument === "--target") {
      target = argumentsList[index + 1];
      index += 1;
      continue;
    }
    if (argument === "--help" || argument === "-h") {
      console.log(
        "Usage:\n  delegation-layer --target ABSOLUTE_SKILL_DIR [--force]\n  delegation-layer install-cli [--version VERSION] [--install-dir ABSOLUTE_DIR]",
      );
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

async function install({ force, target }) {
  const targetStat = await existingPath(target);
  if (targetStat?.isSymbolicLink()) {
    throw new Error(`target directory must not be a symlink: ${target}`);
  }
  if (targetStat && !targetStat.isDirectory()) {
    throw new Error(`target must be a directory: ${target}`);
  }

  await mkdir(target, { recursive: true });

  const destination = join(target, "SKILL.md");
  const destinationStat = await existingPath(destination);
  if (destinationStat?.isSymbolicLink()) {
    throw new Error(`existing SKILL.md must not be a symlink: ${destination}`);
  }
  if (destinationStat && !destinationStat.isFile()) {
    throw new Error(`existing SKILL.md is not a regular file: ${destination}`);
  }

  if (destinationStat && !force) {
    const [source, existing] = await Promise.all([
      readFile(sourcePath),
      readFile(destination),
    ]);
    if (source.equals(existing)) {
      console.log(`Skill already installed: ${destination}`);
      return;
    }
    throw new Error(`SKILL.md already exists; use --force to replace it: ${destination}`);
  }

  await copyFile(sourcePath, destination);
  console.log(`Installed agent integration skill: ${destination}`);
}

const argumentsList = process.argv.slice(2);
if (argumentsList[0] === "install-cli") {
  try {
    const { main } = await import("./install-cli.mjs");
    await main(argumentsList.slice(1));
  } catch (error) {
    console.error(`Error: ${error.message}`);
    process.exitCode = 1;
  }
} else {
  const options = parseArguments(argumentsList);
  if (options) {
    try {
      await install(options);
    } catch (error) {
      console.error(`Error: ${error.message}`);
      process.exitCode = 1;
    }
  }
}
