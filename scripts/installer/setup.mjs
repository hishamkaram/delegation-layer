import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { validateSkillSource } from "./filesystem.mjs";
import { getHarness } from "./catalog.mjs";
import { parseSetupArguments } from "./setup-arguments.mjs";
import { inspectSetupCli } from "./setup-cli.mjs";
import { DoctorRequestRejectedError, runSetupDoctor } from "./setup-doctor.mjs";
import { printFirstTask, shellQuote } from "./setup-recipe.mjs";
import { inspectSkill, installSkill } from "./skill-store.mjs";
import { targetFor } from "./selection.mjs";
import { applyRouting, prepareRouting, printRouting } from "./routing-store.mjs";

const usage = "Usage: delegation-layer setup [--scope project|global] [--harness NAME[,NAME...]] [--yes] [--force] [--routing default|skill-only] [--install-dir ABS] [--provider ID --cwd ABS] [--model MODEL] [--effort EFFORT] [--review-target TARGET]";

async function packageVersion(packageRoot) {
  const metadata = JSON.parse(await readFile(join(packageRoot, "package.json"), "utf8"));
  if (metadata.name !== "delegation-layer" || typeof metadata.version !== "string"
    || !/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$/.test(metadata.version)) {
    throw new Error("Invalid delegation-layer package identity/version; cannot select an exact CLI release");
  }
  return metadata.version;
}

function printDoctor(report, output) {
  output(`Doctor: root ${report.root.path} (${report.root.status}); installation ${report.installation.status}; compatibility not_checked.`);
  for (const [name, role] of Object.entries(report.installation.roles)) {
    // Only validated role facts are printed; additive provider/native output is not echoed.
    if (["delegate", "delegate_run", "pueue", "pueued"].includes(name)) output(`  ${name}: ${role.status} (${role.path})`);
  }
  output(`Supervisor: binding ${report.supervisor.binding}; liveness not_checked; active_work unknown.`);
  if (report.provider) output(`Provider ${report.provider.id}: ${report.provider.status}; authentication unknown.`);
  for (const issue of report.issues) output(`  ${issue.area}: ${issue.code}; ${issue.next_step}`);
}

// Setup owns only ephemeral progress. Existing CLI, skill and routing owners keep
// authority over writes; plans are refreshed after doctor before publication.
export async function runSetup(context) {
  const { argumentsList, packageRoot, sourceDirectory, cwd, environment, platform, input, output, errorOutput, installCliMain, chooseInstallOptions } = context;
  const report = (message) => output.write(`${message}\n`);
  const parsed = parseSetupArguments(argumentsList);
  if (parsed.kind === "help") { report(usage); return 0; }
  if (parsed.kind === "error") { errorOutput.write(`Error: ${parsed.message}\n${usage}\n`); return 2; }
  const completed = [];
  let step = "selection";
  try {
    if (!["linux", "darwin"].includes(platform)) throw new Error("setup supports Linux and macOS only");
    const home = context.home ?? context.resolveHome();
    const selected = await chooseInstallOptions({ ...parsed.options, cwd, home, environment, platform, input, output, report });
    if (!selected) { report("Setup cancelled."); return 0; }
    const targets = [...new Set(selected.harnessNames.map((name) => targetFor(getHarness(name), selected.scope, selected.cwd, selected.home, environment)))];
    const inspectTargets = async () => {
      for (const target of targets) await inspectSkill({ sourceDirectory, target, force: selected.force });
    };
    const routingPlans = () => selected.routing === "default"
      ? prepareRouting(selected, "enable", { environment, platform, sourceDirectory }) : Promise.resolve([]);
    step = "read-only skill and routing validation";
    await validateSkillSource(sourceDirectory);
    await inspectTargets();
    await routingPlans();
    const version = await packageVersion(packageRoot);
    report(`Package: delegation-layer ${version}`);
    step = "CLI inspection";
    let cli = await inspectSetupCli({ installDir: parsed.options.installDir, environment, home, cwd });
    if (cli.absent) {
      step = "exact-version CLI installation";
      // Refresh all four absent roles immediately before the existing writer runs.
      cli = await inspectSetupCli({ installDir: cli.directory, environment, home, cwd });
      if (cli.absent) {
        try {
          await installCliMain(["--version", version, "--install-dir", cli.directory], { packageRoot, environment, platform, errorOutput, noReplace: true });
        } catch (cause) {
          throw new Error(`Could not install matching CLI release ${version}; verify that this exact release is available. No latest fallback or CLI overwrite is permitted. The installer may have published some roles; inspect ${cli.directory} before retrying or use an empty --install-dir.`, { cause });
        }
        completed.push("CLI installation");
        cli = await inspectSetupCli({ installDir: cli.directory, environment, home, cwd });
      }
    }
    report(`Selected CLI: ${cli.executable}`);
    if (cli.pathAction) {
      const pathSelection = cli.pathSelection.kind === "unknown" ? "unknown (PATH inspection unavailable)" : cli.pathSelection.executable ?? "none";
      report(`Manual PATH step: put ${cli.directory} before other Delegate installations on PATH. Current PATH selection: ${pathSelection}. Until PATH is verified, use this absolute command for passive checks: ${shellQuote(cli.executable)} doctor --json`);
    }
    step = "doctor";
    const doctor = await runSetupDoctor({ executable: cli.executable, providerArguments: parsed.options.providerArguments, environment });
    printDoctor(doctor.report, report);
    if (doctor.code !== 0 || doctor.signal) throw new Error(`Doctor did not complete successfully (${doctor.signal ?? `exit ${doctor.code}`}); skill and routing writes were not started. Upgrade an unsupported CLI using its original installation method.`);
    if (doctor.report.installation.status !== "observed" || ["delegate", "delegate_run", "pueue", "pueued"].some((name) => doctor.report.installation.roles[name].status !== "available")
      || doctor.report.root.status === "observation_incomplete" || doctor.report.supervisor.binding === "observation_incomplete"
      || (doctor.report.provider && doctor.report.provider.status !== "static_ready")) {
      throw new Error("Doctor exit status conflicts with its required observations; no skill or routing writes were started.");
    }
    completed.push("doctor observations");
    step = "refreshed skill and routing validation";
    await inspectTargets();
    await routingPlans();
    step = "skill installation";
    for (const target of targets) {
      await installSkill({ sourceDirectory, target, force: selected.force, report });
      completed.push(`skill ${target}`);
    }
    step = "routing installation";
    const refreshedRouting = await routingPlans();
    await applyRouting(refreshedRouting);
    for (const plan of refreshedRouting) printRouting(plan, "enable", report);
    if (selected.routing === "skill-only") report("Skill-only installation; existing routing settings were not changed.");
    completed.push("routing selection applied");
    report("Setup completed. Skill activation remains unknown; restart or resume the harness and review native hook trust where applicable.");
    printFirstTask({ executable: cli.executable, providerArguments: parsed.options.providerArguments, report });
    return 0;
  } catch (error) {
    errorOutput.write(`Error: ${error instanceof Error ? error.message : String(error)}\n`);
    if (error instanceof DoctorRequestRejectedError) errorOutput.write(`${usage}\n`);
    report(`Setup stopped at: ${step}. Completed: ${completed.length ? completed.join("; ") : "none"}. Remaining steps were not completed; no global rollback was attempted.`);
    return error instanceof DoctorRequestRejectedError ? 2 : 1;
  }
}
