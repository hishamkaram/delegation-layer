import { lstat, mkdir, readdir, readFile, copyFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";

export async function existingPath(path) {
  try {
    return await lstat(path);
  } catch (error) {
    if (error && typeof error === "object" && "code" in error && error.code === "ENOENT") {
      return null;
    }
    throw error;
  }
}

export async function assertNoSymlinkComponents(path) {
  let current = resolve(path);
  while (true) {
    const stat = await existingPath(current);
    if (stat?.isSymbolicLink()) {
      throw new Error(`skill destination must not contain symlink components: ${current}`);
    }
    const parent = dirname(current);
    if (parent === current) return;
    current = parent;
  }
}

export async function skillFiles(directory, relative = "") {
  const entries = await readdir(join(directory, relative), { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const child = join(relative, entry.name);
    const path = join(directory, child);
    const stat = await lstat(path);
    if (stat.isSymbolicLink()) {
      throw new Error(`skill source or destination must not contain symlinks: ${path}`);
    }
    if (stat.isDirectory()) {
      files.push(...(await skillFiles(directory, child)));
      continue;
    }
    if (!stat.isFile()) throw new Error(`skill entry must be a regular file: ${path}`);
    files.push(child);
  }
  return files.sort();
}

export async function validateSkillSource(sourceDirectory) {
  const files = await skillFiles(sourceDirectory);
  if (!files.includes("SKILL.md")) {
    throw new Error(`skill package is missing SKILL.md: ${sourceDirectory}`);
  }
  return files;
}

export async function skillTreeMatches(sourceDirectory, target, files) {
  for (const relative of files) {
    const destination = join(target, relative);
    await assertNoSymlinkComponents(destination);
    const stat = await existingPath(destination);
    if (!stat?.isFile() || stat.isSymbolicLink()) return false;
    const [source, existing] = await Promise.all([
      readFile(join(sourceDirectory, relative)),
      readFile(destination),
    ]);
    if (!source.equals(existing)) return false;
  }
  return true;
}

export async function skillTreeHasExistingFiles(target, files) {
  for (const relative of files) {
    const destination = join(target, relative);
    await assertNoSymlinkComponents(destination);
    if (await existingPath(destination)) return true;
  }
  return false;
}

export async function copySkillTree(sourceDirectory, target, files, force) {
  for (const relative of files) {
    const destination = join(target, relative);
    await assertNoSymlinkComponents(destination);
    await mkdir(dirname(destination), { recursive: true });
    const stat = await existingPath(destination);
    if (stat?.isSymbolicLink()) {
      throw new Error(`existing skill file must not be a symlink: ${destination}`);
    }
    if (stat && !stat.isFile()) {
      throw new Error(`existing skill entry is not a regular file: ${destination}`);
    }
    if (stat && !force) {
      throw new Error(`skill files already exist; use --force to replace them: ${target}`);
    }
    await copyFile(join(sourceDirectory, relative), destination);
  }
}
