import { mkdir } from "node:fs/promises";
import { join } from "node:path";
import {
  assertNoSymlinkComponents,
  copySkillTree,
  existingPath,
  skillFiles,
  skillTreeHasExistingFiles,
  skillTreeMatches,
} from "./filesystem.mjs";

export async function inspectSkill({ sourceDirectory, target, force }) {
  await assertNoSymlinkComponents(target);
  const targetStat = await existingPath(target);
  if (targetStat?.isSymbolicLink()) {
    throw new Error(`target directory must not be a symlink: ${target}`);
  }
  if (targetStat && !targetStat.isDirectory()) {
    throw new Error(`target must be a directory: ${target}`);
  }

  const files = await skillFiles(sourceDirectory);
  for (const relative of files) {
    const destination = join(target, relative);
    await assertNoSymlinkComponents(destination);
    const stat = await existingPath(destination);
    if (stat && !stat.isFile()) throw new Error(`existing skill entry is not a regular file: ${destination}`);
  }
  if (!force && (await skillTreeMatches(sourceDirectory, target, files))) {
    return { files, matches: true };
  }
  if (!force && (await skillTreeHasExistingFiles(target, files))) {
    throw new Error(`skill files already exist; use --force to replace them: ${target}`);
  }
  return { files, matches: false };
}

export async function installSkill({ sourceDirectory, target, force, report }) {
  const { files, matches } = await inspectSkill({ sourceDirectory, target, force });
  if (matches) {
    report(`Skill already installed: ${join(target, "SKILL.md")}`);
    return;
  }
  await mkdir(target, { recursive: true });
  await copySkillTree(sourceDirectory, target, files, force);
  report(`Installed agent integration skill: ${join(target, "SKILL.md")}`);
}
