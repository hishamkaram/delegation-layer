import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { chmod, lstat, mkdir, mkdtemp, readFile, readdir, readlink, realpath, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const entrypoint = await realpath(process.argv[2] ?? join(repositoryRoot, "scripts/install-agent-integration.mjs"));
const packageRoot = resolve(dirname(entrypoint), "..");
const { version } = JSON.parse(await readFile(join(packageRoot, "package.json"), "utf8"));
const root = await realpath(await mkdtemp(join(tmpdir(), "delegate-publication-test-")));
const originalPath = process.env.PATH ?? "/usr/bin:/bin";
const roles = ["delegate", "delegate-run", "pueue", "pueued"];

function run(executable, args, options = {}) {
  const result = spawnSync(executable, args, { encoding: "utf8", ...options });
  assert.ifError(result.error);
  assert.equal(result.signal, null, "fixture child must close naturally");
  return result;
}

async function script(path, content) {
  await writeFile(path, content);
  await chmod(path, 0o755);
}

try {
  const archiveDirectory = join(root, "archive-source");
  await mkdir(archiveDirectory);
  const doctorSource = `#!${process.execPath}
const path = require('node:path');
const directory = path.dirname(process.argv[1]);
const report = {
 schema_version:1,command:'doctor',root:{path:path.join(process.env.HOME,'state'),status:'not_initialized'},
 installation:{scope:'installation_bundle',verification:'filesystem_metadata',compatibility:'not_checked',status:'observed',roles:Object.fromEntries([['delegate','delegate'],['delegate_run','delegate-run'],['pueue','pueue'],['pueued','pueued']].map(([name,role])=>[name,{path:path.join(directory,role),status:'available'}]))},
 supervisor:{binding:'not_initialized',liveness:'not_checked',active_work:'unknown'},issues:[]
};
process.stdout.write(JSON.stringify(report));
`;
  for (const role of roles) await script(join(archiveDirectory, role), role === "delegate" ? doctorSource : "#!/bin/sh\nexit 0\n");
  const os = process.platform === "darwin" ? "Darwin" : "Linux";
  const arch = process.arch === "arm64" ? "arm64" : "amd64";
  const archiveName = `delegation-layer_${version}_${os}_${arch}.tar.gz`;
  const archive = join(root, archiveName);
  const tar = run("tar", ["-czf", archive, "-C", archiveDirectory, ...roles]);
  assert.equal(tar.status, 0, tar.stderr);
  // Distribution-archive integrity only; never hash an executable.
  const digest = createHash("sha256").update(await readFile(archive)).digest("hex");
  const checksums = join(root, "checksums.txt");
  await writeFile(checksums, `${digest}  ${archiveName}\n`);

  async function scenario(name, raceKind) {
    const directory = join(root, name);
    const home = join(directory, "home");
    const bin = join(directory, "bin");
    const shim = join(directory, "shim");
    await Promise.all([home, bin, shim].map((path) => mkdir(path, { recursive: true })));
    const target = join(bin, "pueue");
    const foreign = join(directory, "foreign-target");
    await writeFile(foreign, "foreign symlink target");
    await script(join(shim, "curl"), `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
const destination = args[args.indexOf('-o') + 1];
const url = args[args.length - 1];
const base = ${JSON.stringify(`https://github.com/hishamkaram/delegation-layer/releases/download/v${version}/`)};
if (url === base + ${JSON.stringify(archiveName)}) {
 const kind = ${JSON.stringify(raceKind)};
 if (kind === 'file') fs.writeFileSync(${JSON.stringify(target)}, 'foreign role');
 if (kind === 'symlink') fs.symlinkSync(${JSON.stringify(foreign)}, ${JSON.stringify(target)});
 if (kind === 'directory') { fs.mkdirSync(${JSON.stringify(target)}); fs.writeFileSync(${JSON.stringify(join(target, "keep"))}, 'foreign directory'); }
 fs.copyFileSync(${JSON.stringify(archive)}, destination);
} else if (url === base + 'checksums.txt') fs.copyFileSync(${JSON.stringify(checksums)}, destination);
else { process.stderr.write('Unexpected release request'); process.exitCode = 1; }
`);
    const env = {
      ...process.env, HOME: home, DELEGATION_LAYER_HOME: home,
      DELEGATION_LAYER_INSTALL_DIR: bin, DELEGATION_LAYER_VERSION: "v0.0.0",
      PATH: `${shim}:${originalPath}`, CLAUDE_CONFIG_DIR: "", CODEX_HOME: "",
    };
    const result = run(process.execPath, [entrypoint, "setup", "--install-dir", bin, "--harness", "universal", "--routing", "skill-only", "--yes"], { cwd: directory, env });
    return { directory, home, bin, target, foreign, env, result };
  }

  for (const kind of ["file", "symlink", "directory"]) {
    const f = await scenario(`racing-${kind}`, kind);
    assert.equal(f.result.status, 1, f.result.stderr);
    assert.match(f.result.stderr, /Could not publish pueue .*destination already exists and was preserved/);
    assert.match(f.result.stderr, /Published roles: delegate-run/);
    assert.match(f.result.stdout, /Setup stopped at: exact-version CLI installation/);
    assert.doesNotMatch(f.result.stdout, /Setup completed/);
    if (kind === "file") assert.equal(await readFile(f.target, "utf8"), "foreign role");
    if (kind === "symlink") {
      assert.equal(await readlink(f.target), f.foreign);
      assert.equal(await readFile(f.foreign, "utf8"), "foreign symlink target");
    }
    if (kind === "directory") assert.deepEqual(await readdir(f.target), ["keep"]);
    assert.ok((await lstat(join(f.bin, "delegate-run"))).isFile(), "partial publication must be reported and retained");
    await assert.rejects(lstat(join(f.bin, "delegate")), { code: "ENOENT" });
    await assert.rejects(lstat(join(f.home, ".agents/skills/agent-integration")), { code: "ENOENT" });
    assert.ok((await readdir(f.bin)).every((name) => !name.startsWith(".delegation-layer-stage.")), "owned staging directory must be cleaned");
    const retry = run(process.execPath, [entrypoint, "setup", "--install-dir", f.bin, "--harness", "universal", "--yes", "--force"], { cwd: f.directory, env: f.env });
    assert.equal(retry.status, 1);
    assert.match(retry.stderr, /Partial CLI installation/);
  }

  const fresh = await scenario("fresh", "none");
  assert.equal(fresh.result.status, 0, fresh.result.stderr);
  assert.match(fresh.result.stdout, /Setup completed/);
  await writeFile(fresh.target, "old installation role");
  const legacyUpgrade = run(process.execPath, [entrypoint, "install-cli", "--version", version, "--install-dir", fresh.bin], { cwd: fresh.directory, env: fresh.env });
  assert.equal(legacyUpgrade.status, 0, legacyUpgrade.stderr);
  assert.equal(await readFile(fresh.target, "utf8"), "#!/bin/sh\nexit 0\n", "ordinary install-cli must retain its upgrade behavior");
  console.log("CLI publication race checks passed (file, symlink, directory, fresh install, legacy upgrade).");
} finally {
  await rm(root, { recursive: true, force: true });
}
