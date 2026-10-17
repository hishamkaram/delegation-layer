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

export async function installSkill({ sourceDirectory, target, force, report }) {
  await assertNoSymlinkComponents(target);
  const targetStat = await existingPath(target);
  if (targetStat?.isSymbolicLink()) {
    throw new Error(`target directory must not be a symlink: ${target}`);
  }
  if (targetStat && !targetStat.isDirectory()) {
    throw new Error(`target must be a directory: ${target}`);
  }

  const files = await skillFiles(sourceDirectory);
  await mkdir(target, { recursive: true });
  if (!force && (await skillTreeMatches(sourceDirectory, target, files))) {
    report(`Skill already installed: ${join(target, "SKILL.md")}`);
    return;
  }
  if (!force && (await skillTreeHasExistingFiles(target, files))) {
    throw new Error(`skill files already exist; use --force to replace them: ${target}`);
  }
  await copySkillTree(sourceDirectory, target, files, force);
  report(`Installed agent integration skill: ${join(target, "SKILL.md")}`);
}
