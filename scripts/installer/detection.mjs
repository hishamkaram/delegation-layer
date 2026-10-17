import { accessSync, constants } from "node:fs";
import { delimiter, join } from "node:path";
import { harnesses } from "./catalog.mjs";
import { existingPath } from "./filesystem.mjs";

function executableOnPath(command, path, platform) {
  const candidates = platform === "win32" ? [command, `${command}.exe`, `${command}.cmd`] : [command];
  return path.split(delimiter).filter(Boolean).some((pathEntry) => candidates.some((candidate) => {
    try {
      accessSync(join(pathEntry, candidate), constants.X_OK);
      return true;
    } catch {
      return false;
    }
  }));
}

export async function detectHarnesses({ cwd, home, path = "", platform = process.platform }) {
  return Promise.all(harnesses.map(async (harness) => {
    const reasons = [];
    for (const marker of harness.projectMarkers) {
      if ((await existingPath(join(cwd, ...marker)))?.isDirectory()) {
        reasons.push(`project ${join(...marker)}`);
        break;
      }
    }
    for (const marker of harness.globalMarkers) {
      if ((await existingPath(join(home, ...marker)))?.isDirectory()) {
        reasons.push(`global ${join(...marker)}`);
        break;
      }
    }
    for (const command of harness.commands) {
      if (executableOnPath(command, path, platform)) {
        reasons.push(`${command} on PATH`);
        break;
      }
    }
    return { harness, reasons };
  }));
}
