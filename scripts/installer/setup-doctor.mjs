import { spawn } from "node:child_process";

const outputLimit = 1024 * 1024;
const upgradeGuidance = "Upgrade Delegate using its original installation method to a release supporting doctor schema 1; setup never overwrites an existing CLI.";

function object(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function string(value) {
  return typeof value === "string";
}

function oneOf(value, choices) {
  return choices.includes(value);
}

function validateDoctorReport(report, providerArguments) {
  const validRoot = object(report?.root) && string(report.root.path)
    && oneOf(report.root.status, ["not_initialized", "present", "observation_incomplete"]);
  const installation = report?.installation;
  const validInstallation = object(installation) && installation.scope === "installation_bundle"
    && installation.verification === "filesystem_metadata" && installation.compatibility === "not_checked"
    && oneOf(installation.status, ["observed", "blocked", "observation_incomplete"])
    && object(installation.roles) && ["delegate", "delegate_run", "pueue", "pueued"].every((name) => {
      const role = installation.roles[name];
      return object(role) && string(role.path) && oneOf(role.status, ["available", "missing", "unusable", "observation_incomplete"]);
    });
  const supervisor = report?.supervisor;
  const validSupervisor = object(supervisor) && supervisor.liveness === "not_checked" && supervisor.active_work === "unknown"
    && oneOf(supervisor.binding, ["not_initialized", "binding_absent", "present", "observation_incomplete"]);
  const provider = report?.provider;
  const providerIndex = providerArguments.indexOf("--provider");
  const validProvider = providerIndex < 0 ? provider === undefined : object(provider)
    && provider.id === providerArguments[providerIndex + 1] && string(provider.executable)
    && provider.authentication === "unknown" && provider.verification === "static_preflight"
    && oneOf(provider.status, ["static_ready", "blocked", "observation_incomplete"]);
  const validIssues = Array.isArray(report?.issues) && report.issues.length <= 100
    && report.issues.every((issue) => object(issue) && string(issue.code) && string(issue.area) && string(issue.next_step));
  if (!object(report) || report.schema_version !== 1 || report.command !== "doctor"
    || !validRoot || !validInstallation || !validSupervisor || !validProvider || !validIssues) {
    throw new Error(`Unsupported or malformed doctor report. ${upgradeGuidance}`);
  }
  return report;
}

// The foreground child owns its lifetime. Both pipes are drained to natural closure,
// including after overflow; no timeout or signal can leave an unobserved child.
export async function runSetupDoctor({ executable, providerArguments, environment, spawnProcess = spawn }) {
  const result = await new Promise((resolvePromise, reject) => {
    const child = spawnProcess(executable, ["doctor", "--json", ...providerArguments], {
      env: environment,
      stdio: ["ignore", "pipe", "pipe"],
    });
    const chunks = [];
    let size = 0;
    let overflow = false;
    let childError;
    const consume = (capture) => (chunk) => {
      const bytes = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      const remaining = Math.max(0, outputLimit - size);
      if (capture && remaining) chunks.push(bytes.subarray(0, remaining));
      size += bytes.length;
      if (size > outputLimit) overflow = true;
    };
    child.stdout.on("data", consume(true));
    child.stderr.on("data", consume(false));
    child.stdout.on("error", (error) => { childError ??= error; });
    child.stderr.on("error", (error) => { childError ??= error; });
    child.on("error", (error) => { childError ??= error; });
    child.on("close", (code, signal) => {
      if (childError) {
        reject(new Error(`Could not run selected Delegate doctor. ${upgradeGuidance}`, { cause: childError }));
        return;
      }
      if (overflow) {
        reject(new Error("Doctor output exceeded the 1 MiB limit; child exited and output was discarded."));
        return;
      }
      resolvePromise({ code, signal, text: Buffer.concat(chunks).toString("utf8") });
    });
  });
  let report;
  try {
    report = JSON.parse(result.text);
  } catch {
    throw new Error(`Selected Delegate did not return doctor JSON. ${upgradeGuidance}`);
  }
  validateDoctorReport(report, providerArguments);
  return { ...result, report };
}
