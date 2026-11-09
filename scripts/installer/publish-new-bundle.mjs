import { link } from "node:fs/promises";
import { isAbsolute, join } from "node:path";

const [stageDirectory, installDirectory, ...extra] = process.argv.slice(2);
if (extra.length || !stageDirectory || !installDirectory || !isAbsolute(stageDirectory) || !isAbsolute(installDirectory)) {
  process.stderr.write("Fresh CLI publication requires absolute staging and installation directories.\n");
  process.exitCode = 1;
} else {
  // install.sh owns the verified stage on the destination filesystem. link()
  // publishes each exact destination atomically and fails for any existing entry.
  const published = [];
  for (const role of ["delegate-run", "pueue", "pueued", "delegate"]) {
    const destination = join(installDirectory, role);
    try {
      await link(join(stageDirectory, role), destination);
      published.push(role);
    } catch (error) {
      const exists = error && typeof error === "object" && "code" in error && error.code === "EEXIST";
      const reason = exists ? "destination already exists and was preserved" : "atomic no-replace publication failed";
      process.stderr.write(`Could not publish ${role} at ${destination}: ${reason}. Published roles: ${published.length ? published.join(", ") : "none"}. No rollback was attempted; inspect this directory or choose an empty --install-dir.\n`);
      process.exitCode = 1;
      break;
    }
  }
}
