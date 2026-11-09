import { isAbsolute } from "node:path";
import { parseInstallArguments } from "./arguments.mjs";
import { normalizeAbsolute } from "./paths.mjs";

export function parseSetupArguments(argumentsList) {
  const installArguments = [];
  const providerArguments = [];
  let installDir;
  const supplied = new Map();
  const valueFlags = ["--install-dir", "--provider", "--cwd", "--model", "--effort", "--review-target"];
  for (let index = 0; index < argumentsList.length; index += 1) {
    const argument = argumentsList[index];
    if (!valueFlags.includes(argument)) {
      installArguments.push(argument);
      continue;
    }
    const value = argumentsList[index + 1];
    if (!value || value.startsWith("--")) return { kind: "error", message: `${argument} requires a value` };
    index += 1;
    if (supplied.has(argument)) return { kind: "error", message: `${argument} must be supplied once` };
    supplied.set(argument, value);
    if (["--install-dir", "--cwd"].includes(argument) && !isAbsolute(value)) {
      return { kind: "error", message: `${argument} must be an absolute path` };
    }
    if (argument === "--install-dir") installDir = normalizeAbsolute(value);
    else providerArguments.push(argument, value);
  }
  const parsed = parseInstallArguments(installArguments);
  if (parsed.kind !== "options") return parsed;
  const provider = supplied.has("--provider");
  const cwd = supplied.has("--cwd");
  if (provider !== cwd || (!provider && providerArguments.length > 0)) {
    return { kind: "error", message: "--provider and --cwd are required together; --model, --effort and --review-target require that pair" };
  }
  return { kind: "options", options: { ...parsed.options, installDir, providerArguments } };
}
