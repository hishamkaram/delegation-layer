"""Finite process ownership and exact-byte evidence for Phase 2 acceptance."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def sha(data):
    return hashlib.sha256(data).hexdigest()


def digest(path):
    value = hashlib.sha256()
    with Path(path).open("rb") as stream:
        while block := stream.read(1024 * 1024):
            value.update(block)
    return value.hexdigest()


def write_json(path, value, replace=False):
    path = Path(path)
    data = (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()
    if replace:
        stage = path.with_name(path.name + ".staging")
        with stage.open("xb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(stage, path)
    else:
        with path.open("xb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
    os.chmod(path, 0o600)


def read_json(path, bound=1024 * 1024):
    with Path(path).open("rb") as stream:
        data = stream.read(bound + 1)
    require(len(data) <= bound, "JSON evidence exceeds bound: " + str(path))
    def pairs(entries):
        obj = {}
        for key, value in entries:
            require(key not in obj, "duplicate evidence key: " + key)
            obj[key] = value
        return obj
    return json.loads(data, object_pairs_hook=pairs)


def inherited_environment():
    # Each included coordinate is inherited without relocation or PATH shadowing.
    keys = {"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL",
            "LC_CTYPE", "TZ", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
            "XDG_RUNTIME_DIR", "__CF_USER_TEXT_ENCODING"}
    return {key: value for key, value in os.environ.items() if key in keys}


def config_for(base):
    base = Path(base)
    return {
        "shared": {
            "pueue_directory": str(base / "state"),
            "runtime_directory": str(base / "run"),
            "unix_socket_path": str(base / "run" / "p.sock"),
            "alias_file": str(base / "aliases.yml"),
            "pid_path": str(base / "run" / "p.pid"),
            "shared_secret_path": str(base / "run" / "secret"),
            "daemon_cert": str(base / "run" / "cert.pem"),
            "daemon_key": str(base / "run" / "key.pem"),
            "use_unix_socket": True, "unix_socket_permissions": 384,
            "host": "127.0.0.1", "port": "6924",
        },
        "client": {
            "restart_in_place": False, "read_local_logs": True,
            "show_confirmation_questions": False, "edit_mode": "toml",
            "show_expanded_aliases": False, "dark_mode": False,
            "max_status_lines": None, "status_time_format": "%H:%M:%S",
            "status_datetime_format": "%Y-%m-%d\n%H:%M:%S",
        },
        "daemon": {
            "pause_group_on_failure": False, "pause_all_on_failure": False,
            "compress_state_file": False, "callback": None, "env_vars": {},
            "callback_log_lines": 10,
            "shell_command": ["/bin/sh", "-c", "{{ pueue_command_string }}"],
        },
        "profiles": {},
    }


class Process:
    def __init__(self, directory, argv, cwd, environment):
        self.directory = Path(directory)
        self.directory.mkdir(mode=0o700)
        self.argv = [str(arg) for arg in argv]
        executable = Path(self.argv[0])
        require(executable.is_absolute(), "executable must be absolute")
        self.stdout = None
        self.stderr = None
        self.result = None
        self.process = None
        write_json(self.directory / "invocation.json", {
            "argv": self.argv, "cwd": str(cwd),
            "executable_sha256": digest(executable),
            "environment_sha256": sha(json.dumps(environment, sort_keys=True).encode()),
            "stdin": "DEVNULL", "shell": False,
        })
        try:
            self.stdout = (self.directory / "stdout").open("xb")
            self.stderr = (self.directory / "stderr").open("xb")
            self.process = subprocess.Popen(
                self.argv, cwd=cwd, env=environment, stdin=subprocess.DEVNULL,
                stdout=self.stdout, stderr=self.stderr, shell=False, close_fds=True)
        except BaseException:
            if self.stdout is not None:
                self.stdout.close()
            if self.stderr is not None:
                self.stderr.close()
            raise

    @property
    def pid(self):
        return self.process.pid

    def poll(self):
        if self.result is not None:
            completed = self.directory / "completed.json"
            require(not completed.is_symlink(),
                    "completed process receipt is a symlink: " + str(completed))
            if not completed.is_file():
                # A previous observation may have recorded the in-memory
                # result before a receipt write failed.  Retry that receipt
                # before treating the process as fully observed.
                write_json(completed, self.result)
            else:
                require(read_json(completed) == self.result,
                        "completed process receipt does not match observation: " + str(completed))
            return self.result
        if self.process.poll() is None:
            return None
        code = self.process.wait()
        self.stdout.close()
        self.stderr.close()
        self.result = {
            "pid": self.pid, "exit_code": code, "natural_wait": code >= 0,
            "ended_ns": time.time_ns(),
            "stdout_sha256": digest(self.directory / "stdout"),
            "stderr_sha256": digest(self.directory / "stderr"),
            "stdout_bytes": (self.directory / "stdout").stat().st_size,
            "stderr_bytes": (self.directory / "stderr").stat().st_size,
        }
        write_json(self.directory / "completed.json", self.result)
        return self.result

    def wait(self, timeout=15, expected=None):
        deadline = time.monotonic() + timeout
        while self.poll() is None:
            if time.monotonic() >= deadline:
                marker = self.directory / "observation-expired.json"
                if not marker.exists():
                    write_json(marker, {"pid": self.pid, "timeout_seconds": timeout,
                                        "termination": "unknown", "signals_sent": 0})
                raise RuntimeError("process observation expired; ownership retained: " + str(self.directory))
            time.sleep(0.025)
        require(self.result["natural_wait"], "process ended by signal: " + str(self.directory))
        if expected is not None:
            allowed = {expected} if isinstance(expected, int) else set(expected)
            require(self.result["exit_code"] in allowed,
                    "unexpected exit %s for %s" % (self.result["exit_code"], self.directory))
        return self.result["exit_code"]

    def json(self):
        require(self.result is not None, "output requested before observed completion")
        return read_json(self.directory / "stdout", 8 * 1024 * 1024)


class Processes:
    def __init__(self, directory, environment=None):
        self.directory = Path(directory)
        self.directory.mkdir(mode=0o700, parents=True)
        self.environment = inherited_environment() if environment is None else dict(environment)
        self.entries = []

    def start(self, name, argv, cwd):
        directory = self.directory / ("%03d-%s" % (len(self.entries) + 1, name))
        process = Process(directory, argv, cwd, self.environment)
        self.entries.append(process)
        # Retain the actual Popen before any fallible post-Start receipt write.
        write_json(directory / "started.json", {
            "pid": process.pid, "started_ns": time.time_ns()})
        return process

    def run(self, name, argv, cwd, expected=0, timeout=15):
        process = self.start(name, argv, cwd)
        process.wait(timeout, expected)
        return process

    def drain(self, timeout=70, exclude=()):
        """Observe finite owned processes after a failed assertion; never signal."""
        deadline = time.monotonic() + timeout
        active = [p for p in self.entries if p not in exclude and p.poll() is None]
        while active and time.monotonic() < deadline:
            time.sleep(0.1)
            active = [p for p in active if p.poll() is None]
        return [{"pid": p.pid, "argv": p.argv, "directory": str(p.directory)} for p in active]


def wait_until(predicate, timeout=15, description="condition"):
    deadline = time.monotonic() + timeout
    while True:
        value = predicate()
        if value:
            return value
        require(time.monotonic() < deadline, "observation expired: " + description)
        time.sleep(0.05)
