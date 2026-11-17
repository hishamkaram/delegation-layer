import { dirname, join } from "node:path";
import { validateSkillSource } from "./filesystem.mjs";
import { inspectSkill, installSkill } from "./skill-store.mjs";

// The installer owns this finite distribution inventory; routing still belongs
// to agent-integration, and --target continues to install that skill alone.
const bundledSkills = Object.freeze(["agent-integration", "delegate-upgrade"]);

export function skillBundle(packageRoot, integrationTargets) {
  return [...new Set(integrationTargets)].flatMap((integrationTarget) => bundledSkills.map((name) => ({
    sourceDirectory: join(packageRoot, "skills", name),
    target: name === "agent-integration" ? integrationTarget : join(dirname(integrationTarget), name),
  })));
}

export async function inspectSkillBundle(plans, force) {
  for (const plan of plans) {
    await validateSkillSource(plan.sourceDirectory);
    await inspectSkill({ ...plan, force });
  }
}

export async function installSkillBundle(plans, force, report) {
  await inspectSkillBundle(plans, force);
  const completed = [];
  for (const plan of plans) {
    try {
      await installSkill({ ...plan, force, report });
      completed.push(plan.target);
    } catch (cause) {
      const detail = cause instanceof Error ? cause.message : "unknown filesystem failure";
      throw new Error(`Skill installation failed at ${plan.target}: ${detail}; completed: ${completed.join(", ") || "none"}. No rollback was attempted.`, { cause });
    }
  }
}
