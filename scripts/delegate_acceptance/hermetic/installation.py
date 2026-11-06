"""Build known fixture sources directly into the caller's owned installation."""

from pathlib import Path
from types import MappingProxyType

from scripts.delegate_acceptance.hermetic.files import canonical_executable
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError
from scripts.delegate_acceptance.processes.observation import inherited_environment
from scripts.delegate_acceptance.processes.operations import run_bounded_command

REPOSITORY = Path(__file__).resolve().parents[3]
SOURCE_PACKAGES = MappingProxyType({
    "harness_delegate": "./internal/testutil/harnesscli/cmd/delegate",
    "harness_runner": "./internal/testutil/harnesscli/cmd/delegate-run",
    "fake_provider": "./internal/testutil/fakeprovider/cmd/provider",
    "fake_supervisor": "./internal/testutil/fakesupervisor/cmd/pueue",
    "harness_probe": "./internal/testutil/harnessprobe",
    "scheduler_fixture": "./internal/scheduler",
    "contributor_delegate": "./internal/testutil/contributorcli/cmd/delegate",
    "contributor_runner": "./internal/testutil/contributorcli/cmd/delegate-run",
    "contributor_provider": "./internal/testutil/contributorprovider/cmd/provider",
})


def require_source_root(source_root):
    source = Path(source_root).resolve(strict=True)
    if not source.is_dir() or not (source / "go.mod").is_file():
        raise RuntimeError("fixture source root must contain go.mod: " + str(source))
    return source


def build_source_command(source_root, role, destination):
    """The directory owner retains this installation until the command's Wait is known."""
    source = require_source_root(source_root)
    if role not in SOURCE_PACKAGES:
        raise RuntimeError("unknown fixture source role: " + str(role))
    package = SOURCE_PACKAGES[role]
    if not (source / package).is_dir():
        raise RuntimeError("fixture source package is unavailable: " + package)
    target = Path(destination)
    if not target.is_absolute() or target.parent.resolve(strict=True) != target.parent:
        raise RuntimeError("fixture installation parent must be canonical: " + str(target))
    if target.exists() or target.is_symlink():
        raise RuntimeError("fixture installation destination is occupied: " + str(target))
    arguments = ["go", "test", "-c"] if role == "scheduler_fixture" else ["go", "build"]
    arguments += ["-buildvcs=false", "-o", str(target), package]
    environment = inherited_environment()
    environment.update({"GOTOOLCHAIN": "local", "CGO_ENABLED": "0"})
    try:
        result = run_bounded_command(
            arguments, environment, cwd=source, timeout=900,
            capture=True, capture_stderr=True, max_output=1024 * 1024,
        )
    except BoundedCommandError as error:
        raise RuntimeError("fixture installation failed: " + error.reason.value) from error
    if result.exit_code:
        raise RuntimeError("fixture installation failed with exit " + str(result.exit_code))
    return canonical_executable(target)
