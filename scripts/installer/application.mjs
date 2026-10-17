import { join } from "node:path";
import { harnesses } from "./catalog.mjs";
import { parseInstallArguments, parseLegacyArguments } from "./arguments.mjs";
import { detectHarnesses } from "./detection.mjs";
import { validateSkillSource } from "./filesystem.mjs";
import { installSkill } from "./skill-store.mjs";
import { defaultHarnessNames, formatDetected, resolveInstallSelection, targetFor } from "./selection.mjs";
import { pickOptions } from "./terminal.mjs";
import { applyRouting, prepareRouting, printRouting } from "./routing-store.mjs";

const usageText = "Usage:\n  delegation-layer install [--scope project|global] [--harness NAME[,NAME...]] [--yes] [--force] [--routing default|skill-only]\n  delegation-layer routing status|disable [--scope project|global] [--harness NAME[,NAME...]]\n  delegation-layer --target ABSOLUTE_SKILL_DIR [--force]\n  delegation-layer install-cli [--version VERSION] [--install-dir ABSOLUTE_DIR]";
const helpText = `${usageText}\n\nThe install command detects supported agent harnesses and installs the skill for a project or user.\nGlobal scope is the default; use --scope project for a project-local install.\nUse --harness universal when no specific harness is detected.`;

function isFailure(value) {
  return value.kind === "error";
}

function printUsage(errorOutput, message) {
  if (message) errorOutput.write(`Error: ${message}\n`);
  errorOutput.write(`${usageText}\n`);
}

function printDetections(detections, report) {
  report("Detected harnesses:");
  detections.forEach(({ harness, reasons }, index) => {
    report(`  ${index + 1}. ${harness.label}${formatDetected(reasons)}`);
  });
}

async function chooseInstallOptions({
  force,
  harnessNames,
  scope,
  yes,
  routing,
  cwd,
  home,
  environment,
  platform,
  input,
  output,
  report,
}) {
  const detections = await detectHarnesses({ cwd, home, path: environment.PATH ?? "", platform });
  const defaults = defaultHarnessNames(detections);
  const canPrompt = Boolean(input.isTTY && output.isTTY);
  const needsPrompt = !yes && !harnessNames && !scope && canPrompt;

  if (!harnessNames && !needsPrompt) printDetections(detections, report);
  if (!canPrompt && !yes && !harnessNames && !scope) {
    report("No interactive terminal detected; using global scope and detected harnesses.");
  }

  if (needsPrompt) {
    scope = await pickOptions({
      initialValues: ["global"],
      options: [
        { value: "global", label: "Global", description: `available to your agents from ${home}` },
        { value: "project", label: "Project", description: `available to this project from ${cwd}` },
      ],
      title: "Where should the agent skill be installed?",
      input,
      output,
    });
    if (!scope) return null;
    harnessNames = await pickOptions({
      initialValues: defaults,
      multiple: true,
      options: detections.map(({ harness, reasons }) => ({
        value: harness.name,
        label: `${harness.label}${formatDetected(reasons)}`,
        description: targetFor(harness, scope, cwd, home, environment),
      })),
      title: "Which harnesses should use the skill?",
      input,
      output,
    });
    if (!harnessNames) return null;
  }

  if (needsPrompt && !routing) {
    routing = await pickOptions({
      initialValues: ["default"],
      options: [
        {
          value: "default",
          label: "Prefer Delegate",
          description: "Install collaboration context hooks for Claude and Codex; native hook trust still applies",
        },
        {
          value: "skill-only",
          label: "Skill only",
          description: "Install guidance without adding hooks; keep existing routing settings",
        },
      ],
      title: "How should external collaboration be routed?",
      input,
      output,
    });
    if (!routing) return null;
  }

  return resolveInstallSelection({ force, harnessNames, scope, routing, cwd, home, detections });
}

function parseResult(parsed, errorOutput, output) {
  if (parsed.kind === "help") {
    output.write(`${helpText}\n`);
    return 0;
  }
  if (isFailure(parsed)) {
    printUsage(errorOutput, parsed.message);
    return 2;
  }
  return null;
}

export async function runInstaller({
  argumentsList,
  packageRoot,
  cwd,
  home,
  environment,
  platform,
  input,
  output,
  errorOutput,
  installCliMain,
}) {
  const report = (message) => output.write(`${message}\n`);
  const reportError = (error) => {
    const message = error instanceof Error ? error.message : String(error);
    errorOutput.write(`Error: ${message}\n`);
  };
  const sourceDirectory = join(packageRoot, "skills", "agent-integration");

  try {
    await validateSkillSource(sourceDirectory);
  } catch (error) {
    reportError(error);
    return 1;
  }

  if (argumentsList[0] === "install-cli") {
    try {
      await installCliMain(argumentsList.slice(1), { packageRoot, environment, platform });
      return 0;
    } catch (error) {
      reportError(error);
      return 1;
    }
  }

  if (argumentsList[0] === "routing") {
    const action = argumentsList[1];
    const parsed = parseInstallArguments(argumentsList.slice(2));
    const parseExit = parseResult(parsed, errorOutput, output);
    if (parseExit !== null) return parseExit;
    if (!["status", "disable"].includes(action)) {
      printUsage(errorOutput, "routing requires status or disable");
      return 2;
    }
    const options = parsed.options;
    try {
      if (options.routing || options.force) {
        throw new Error("routing status/disable do not accept --routing or --force");
      }
      const selected = await chooseInstallOptions({
        ...options,
        yes: true,
        harnessNames: options.harnessNames ?? ["claude", "codex"],
        cwd,
        home,
        environment,
        platform,
        input,
        output,
        report,
      });
      const plans = await prepareRouting(selected, action, { environment, platform, sourceDirectory });
      if (action === "disable") await applyRouting(plans);
      for (const plan of plans) printRouting(plan, action, report);
      return 0;
    } catch (error) {
      reportError(error);
      return 1;
    }
  }

  if (argumentsList[0] === "install") {
    const parsed = parseInstallArguments(argumentsList.slice(1));
    const parseExit = parseResult(parsed, errorOutput, output);
    if (parseExit !== null) return parseExit;
    try {
      const selected = await chooseInstallOptions({
        ...parsed.options,
        cwd,
        home,
        environment,
        platform,
        input,
        output,
        report,
      });
      if (!selected) {
        report("Installation cancelled.");
        return 0;
      }
      const routingPlans = selected.routing === "default"
        ? await prepareRouting(selected, "enable", { environment, platform, sourceDirectory })
        : [];
      const targets = selected.harnessNames.map((name) => {
        const harness = harnesses.find((candidate) => candidate.name === name);
        if (!harness) throw new Error(`cannot install for unknown harness: ${name}`);
        return targetFor(harness, selected.scope, selected.cwd, selected.home, environment);
      });
      const uniqueTargets = [...new Set(targets)];
      report(`Installing for ${selected.scope}: ${uniqueTargets.join(", ")}`);
      for (const target of uniqueTargets) {
        await installSkill({ force: selected.force, sourceDirectory, target, report });
      }
      await applyRouting(routingPlans);
      for (const plan of routingPlans) printRouting(plan, "enable", report);
      if (selected.routing === "skill-only") {
        report("Skill-only installation; existing routing settings were not changed.");
      }
      return 0;
    } catch (error) {
      reportError(error);
      return 1;
    }
  }

  const parsed = parseLegacyArguments(argumentsList);
  const parseExit = parseResult(parsed, errorOutput, output);
  if (parseExit !== null) return parseExit;
  try {
    await installSkill({
      force: parsed.options.force,
      sourceDirectory,
      target: parsed.options.target,
      report,
    });
    return 0;
  } catch (error) {
    reportError(error);
    return 1;
  }
}
