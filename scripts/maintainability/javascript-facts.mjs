// Parse source only: never link, evaluate, or import inspected modules.
import { readFileSync } from "node:fs";
import { dirname, posix } from "node:path";
import { SourceTextModule } from "node:vm";

const paths = JSON.parse(readFileSync(0, "utf8"));
if (!Array.isArray(paths) || !paths.every((path) => typeof path === "string")) {
  throw new Error("expected a list of source paths");
}

const facts = paths.map((path) => {
  const parsed = new SourceTextModule(readFileSync(path, "utf8"), { identifier: path });
  const imports = parsed.dependencySpecifiers.map((specifier) => ({
    target: `js:${specifier.startsWith(".") ? posix.normalize(posix.join(dirname(path), specifier)) : specifier}`,
    // Module metadata identifies dependencies without observing their positions.
    line: 0,
  }));
  if (parsed.status !== "unlinked") throw new Error("source parser evaluated input");
  return { path, module: `js:${path}`, imports };
});
process.stdout.write(JSON.stringify(facts));
