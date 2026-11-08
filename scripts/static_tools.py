"""Install and verify the pinned repository-local static-analysis tools."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parent.parent
SHELLCHECK_VERSION = "0.11.0"
MAX_SHELLCHECK_ARCHIVE_BYTES = 32 * 1024 * 1024
JACTIONLINT_VERSION = "2.4.0"
SHELLCHECK_DIGESTS = {
    ("Linux", "x86_64"): "b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6",
    ("Linux", "aarch64"): "68a8133197a50beb8803f8d42f9908d1af1c5540d4bb05fdfca8c1fa47decefc",
    ("Darwin", "x86_64"): "c2c15e08df0e8fbc374c335b230a7ee958c313fa5714817a59aa59f1aa594f51",
    ("Darwin", "aarch64"): "339b930feb1ea764467013cc1f72d09cd6b869ebf1013296ba9055ab2ffbd26f",
}


def checked(command: list[str], *, environment=None) -> None:
    subprocess.run(command, cwd=ROOT, env=environment, check=True, timeout=600)


def requirement_version(requirements: Path, package: str) -> str:
    for line in requirements.read_text().splitlines():
        match = re.fullmatch(re.escape(package) + r"==([0-9.]+) \\", line)
        if match:
            return match[1]
    raise ValueError(f"missing exact {package} pin in {requirements}")


def require_banner(command: list[str], expected: str) -> None:
    result = subprocess.run(command, cwd=ROOT, text=True, capture_output=True,
                            check=True, timeout=30)
    if expected not in result.stdout.splitlines():
        observed = ((result.stdout or "") + (result.stderr or "")).strip()[:200]
        raise ValueError(f"unexpected version from {command[0]}: expected {expected!r}, "
                         f"received {observed!r}; run the matching tools target")


def require_ty_version(executable: Path, expected: str) -> None:
    result = subprocess.run([str(executable), "version", "--output-format", "json"],
                            cwd=ROOT, text=True, capture_output=True, check=True, timeout=30)
    version: object = json.loads(result.stdout)
    if not isinstance(version, dict) or version.get("version") != expected:
        observed = result.stdout.strip()[:200]
        raise ValueError(f"unexpected ty version: expected {expected!r}, received {observed!r}; "
                         "run make diagnostic-tools")


def python_tools(group: str, *, install: bool) -> None:
    requirements = ROOT / "tools" / group / "requirements.txt"
    environment = ROOT / "bin" / group
    if install:
        checked([sys.executable, "-m", "venv", str(environment)])
        checked([str(environment / "bin/python"), "-m", "pip", "install",
                 "--disable-pip-version-check", "--only-binary=:all:", "--require-hashes",
                 "-r", str(requirements)])
    packages = ("ruff",) if group == "lint" else ("ty", "zizmor")
    for package in packages:
        expected = requirement_version(requirements, package)
        executable = environment / "bin" / package
        if package == "ty":
            require_ty_version(executable, expected)
        else:
            require_banner([str(executable), "--version"], package + " " + expected)


def shellcheck_target(system: str, machine: str) -> tuple[str, str]:
    architecture = {"arm64": "aarch64", "amd64": "x86_64"}.get(machine, machine)
    digest = SHELLCHECK_DIGESTS.get((system, architecture))
    if digest is None:
        raise ValueError(f"unsupported ShellCheck host: {system}/{machine}")
    return f"shellcheck-v{SHELLCHECK_VERSION}.{system.lower()}.{architecture}.tar.gz", digest


def install_shellcheck() -> None:
    archive, digest = shellcheck_target(platform.system(), platform.machine())
    with tempfile.TemporaryDirectory(prefix="static-tools-") as temporary:
        download = Path(temporary) / archive
        checked([str(ROOT / "scripts/download-tool-archive.sh"),
                 f"https://github.com/koalaman/shellcheck/releases/download/v{SHELLCHECK_VERSION}/{archive}",
                 str(download), str(MAX_SHELLCHECK_ARCHIVE_BYTES)])
        with download.open("rb") as source:
            actual = hashlib.file_digest(source, "sha256").hexdigest()
        if actual != digest:
            raise ValueError("ShellCheck archive checksum mismatch")
        with tarfile.open(download, "r:gz") as bundle:
            try:
                member = bundle.extractfile(f"shellcheck-v{SHELLCHECK_VERSION}/shellcheck")
            except KeyError as error:
                raise ValueError("ShellCheck archive lacks the executable") from error
            if member is None:
                raise ValueError("ShellCheck archive lacks the executable")
            destination = Path(temporary) / "shellcheck"
            with member, destination.open("wb") as output:
                shutil.copyfileobj(member, output)
        destination.chmod(0o755)
        shutil.copy2(destination, ROOT / "bin/shellcheck")


def node_tools(*, install: bool) -> None:
    source = ROOT / "tools/lint"
    destination = ROOT / "bin/.lint-js"
    if install:
        destination.mkdir(parents=True, exist_ok=True)
        for name in ("package.json", "package-lock.json"):
            shutil.copyfile(source / name, destination / name)
        checked(["npm", "ci", "--prefix", str(destination), "--ignore-scripts", "--no-audit", "--no-fund"])
    for name in ("package.json", "package-lock.json"):
        if (source / name).read_bytes() != (destination / name).read_bytes():
            raise ValueError("JavaScript tool lock changed; run make tools")
    manifest: object = json.loads((source / "package.json").read_text())
    if not isinstance(manifest, dict) or not isinstance(manifest.get("dependencies"), dict):
        raise ValueError("invalid JavaScript tool dependencies")
    dependencies = manifest["dependencies"]
    for package, expected in dependencies.items():
        if not isinstance(package, str) or not re.fullmatch(r"(?:@[a-z0-9_-]+/)?[a-z0-9][a-z0-9._-]*", package):
            raise ValueError("invalid JavaScript dependency name")
        if not isinstance(expected, str) or not re.fullmatch(r"\d+\.\d+\.\d+", expected):
            raise ValueError(f"JavaScript dependency {package} requires an exact version")
        installed: object = json.loads((destination / "node_modules" / package / "package.json").read_text())
        if not isinstance(installed, dict) or installed.get("name") != package or installed.get("version") != expected:
            raise ValueError(f"JavaScript dependency {package} does not match its pin; run make tools")
    version = dependencies.get("eslint")
    if not isinstance(version, str) or not re.fullmatch(r"\d+\.\d+\.\d+", version):
        raise ValueError("ESLint requires an exact version")
    require_banner(["node", str(destination / "node_modules/eslint/bin/eslint.js"), "--version"], "v" + version)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("install", "verify"))
    parser.add_argument("--diagnostic", action="store_true")
    args = parser.parse_args()
    install = args.action == "install"
    try:
        (ROOT / "bin").mkdir(exist_ok=True)
        if args.diagnostic:
            python_tools("diagnostics", install=install)
        else:
            if install:
                install_shellcheck()
                environment = dict(os.environ, GOBIN=str(ROOT / "bin"), GOTOOLCHAIN="local")
                checked(["go", "install", f"github.com/jdx/jactionlint/v2/cmd/jactionlint@v{JACTIONLINT_VERSION}"],
                        environment=environment)
            python_tools("lint", install=install)
            python_tools("diagnostics", install=install)
            node_tools(install=install)
            require_banner([str(ROOT / "bin/shellcheck"), "--version"], "version: " + SHELLCHECK_VERSION)
            require_banner([str(ROOT / "bin/jactionlint"), "--version"], "v" + JACTIONLINT_VERSION)
    except (OSError, ValueError, subprocess.SubprocessError, tarfile.TarError) as error:
        print(f"Static analysis tools {args.action} failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
