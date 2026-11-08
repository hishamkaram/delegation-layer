"""Exercise a source-built Delegate pair through a real isolated SSH daemon.

This proves Linux container SSH/lifecycle integration with a finite Pi protocol
fixture. It does not prove a real provider account or native macOS behavior.
"""

from __future__ import annotations

import argparse
import base64
from dataclasses import dataclass, field
import hashlib
import json
import os
from pathlib import Path
import secrets
import socket
import sys
import time
from typing import Literal, Mapping

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.delegate_acceptance.processes.contracts import BoundedCommandResult
from scripts.delegate_acceptance.private_cli.installation import build_candidate_pair


REPOSITORY = Path(__file__).resolve().parents[1]
IMAGE = "delegation-layer-acceptance:remote-ssh-v1"
PROVIDER = "pi:json"
REMOTE_ROOT = "/state"

# A maintained finite native-protocol fixture, not a real provider account.
# Its generated source is installed once and executed in place through a mount.
PI_SOURCE = r"""#!/usr/bin/python3
import json,os,sys,time,uuid
from pathlib import Path
if "--version" in sys.argv:
 print("pi remote fixture 1.0.0");sys.exit(0)
if "--help" in sys.argv:
 print("--mode --tools --model --thinking --session --session-dir --no-extensions --offline --list-models");sys.exit(0)
if "--list-models" in sys.argv:
 print("provider model context max-out thinking images\nfixture bounded 128000 8192 enabled enabled");sys.exit(0)
request=json.loads(sys.stdin.read())
task=request.get("task_id")
if not isinstance(task,str) or len(task)!=32 or any(c not in "0123456789abcdef" for c in task):
 raise RuntimeError("fixture task identity required")
receipts=Path("/root/.pi/remote-fixture-invocations")/task
receipts.mkdir(mode=0o700,parents=True,exist_ok=True)
with (receipts/(uuid.uuid4().hex+".json")).open("x") as receipt:
 json.dump({"task_id":task,"pid":os.getpid()},receipt)
print(json.dumps({"type":"session","id":str(uuid.uuid4())}),flush=True)
print(json.dumps({"type":"agent_start"}),flush=True)
print(json.dumps({"type":"turn_start"}),flush=True)
time.sleep(min(max(float(request.get("delay",0)),0),45))
answer=str(request.get("answer","remote SSH fixture completed"))
message={"role":"assistant","content":[{"type":"text","text":answer}],"stopReason":"stop"}
for event in ({"type":"message_start","message":{"role":"assistant","content":[]}},
 {"type":"message_end","message":message},{"type":"turn_end","message":message,"toolResults":[]},
 {"type":"agent_end","messages":[message]}):
 print(json.dumps(event),flush=True)
"""

# This finite transport fixture withholds the first admission response after
# the real source-built endpoint has completed it. Exit zero models a lost
# response without an endpoint failure; exit 17 deliberately reports endpoint
# failure after acceptance. The first must reconnect automatically; the second
# must retain uncertainty until explicit reconciliation of the same request.
LOST_ACK_SOURCE = r"""#!/usr/bin/python3
import json,struct,subprocess,sys,time
from pathlib import Path
def read_frame(stream):
 header=stream.read(4)
 if len(header)!=4:raise RuntimeError("incomplete frame")
 size=struct.unpack(">I",header)[0]
 if size<1 or size>1048576:raise RuntimeError("bad frame")
 data=stream.read(size)
 if len(data)!=size:raise RuntimeError("incomplete payload")
 return header+data,data
control,body=read_frame(sys.stdin.buffer)
request=json.loads(body)
wire=bytearray(control)
remaining=request.get("intent",{}).get("brief",{}).get("length",0) if request.get("operation") in ("admit","reconcile") else 0
while remaining:
 raw,data=read_frame(sys.stdin.buffer)
 if len(data)>remaining:raise RuntimeError("excess data")
 wire.extend(raw);remaining-=len(data)
result=subprocess.run([__DELEGATE__,"remote","serve","--stdio"],input=bytes(wire),stdout=subprocess.PIPE,check=False)
if result.returncode:sys.exit(result.returncode)
response=json.loads(result.stdout[4:4+struct.unpack(">I",result.stdout[:4])[0]])
if request.get("operation")=="admit" and response.get("existing") is True:
 task=request.get("task_id")
 if not isinstance(task,str) or len(task)!=32 or any(c not in "0123456789abcdef" for c in task):raise RuntimeError("invalid fixture task")
 # Admission alone may still be stashed. Require actual provider startup before
 # either lost-response simulation, so this exercises recovery of running work.
 receipts=Path("/root/.pi/remote-fixture-invocations")/task
 deadline=time.monotonic()+10
 while time.monotonic()<deadline:
  count=len(list(receipts.glob("*.json")))
  if count==1:break
  if count>1:raise RuntimeError("duplicate fixture invocation before acknowledgment loss")
  time.sleep(0.05)
 else:raise RuntimeError("accepted fixture did not start before acknowledgment loss")
 Path("/lost-ack-"+task).write_text("started provider response intentionally withheld\n")
 sys.exit(__ACK_EXIT__)
sys.stdout.buffer.write(result.stdout);sys.stdout.buffer.flush()
"""

CLOSURE_CODE = r"""
import json,sys
from pathlib import Path
sys.path.insert(0,sys.argv[1])
from scripts.delegate_acceptance.private_cli.supervisor import await_managed_lifetime_closure
closure=await_managed_lifetime_closure(Path(sys.argv[2]),tuple(sys.argv[3:]),timeout=30)
print(json.dumps({"closed":True,"tasks":sorted(closure["tasks"])}))
"""

SHUTDOWN_CODE = r"""
import json,os,secrets,sys
from pathlib import Path
sys.path.insert(0,sys.argv[1])
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
state=Path(sys.argv[2]);owner=DirectoryOwner()
capture=owner.create("remote shutdown captures",Path("/cleanup-capture-"+secrets.token_hex(8)),Path("/"))
shutdown_private_supervisor(Path(sys.argv[3]),state/".supervisor"/"pueue.yml",dict(os.environ),
 timeout=30,state=state,task_ids=tuple(sys.argv[4:]),capture_root=capture)
owner.rollback()
print(json.dumps({"supervisor_closed":True}))
"""


class AcceptanceError(RuntimeError):
    """A concrete acceptance failure, with owned resources retained if needed."""


def command(
    argv: list[str], environment: Mapping[str, str], *, timeout: float = 45
) -> BoundedCommandResult:
    return run_bounded_command(
        argv,
        dict(environment),
        timeout=timeout,
        max_output=1024 * 1024,
        capture=True,
        capture_stderr=True,
        cwd=REPOSITORY,
    )


def checked(
    argv: list[str], environment: Mapping[str, str], *, timeout: float = 45
) -> bytes:
    result = command(argv, environment, timeout=timeout)
    if result.exit_code:
        raise AcceptanceError(
            f"control command {Path(argv[0]).name} failed with exit {result.exit_code}: "
            + (result.stderr or result.output).decode("utf-8", "replace")[-1500:]
        )
    return result.output


def object_value(value: object, label: str) -> dict[str, object]:
    if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
        raise AcceptanceError(f"{label} must be a JSON object")
    return value


def decode_object(data: bytes, label: str) -> dict[str, object]:
    return object_value(json.loads(data), label)


def remote_envelope(response: dict[str, object]) -> dict[str, object]:
    remote = object_value(response.get("remote"), "remote response")
    if remote.get("response") is None:
        raise AcceptanceError(
            "destination observation unavailable: "
            + json.dumps(remote.get("failure") or response.get("error"), sort_keys=True)
        )
    return object_value(remote.get("response"), "destination response")


def write_json(path: Path, value: object) -> None:
    path.write_text(json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n")
    path.chmod(0o600)


def host_fingerprint(public: Path) -> str:
    fields = public.read_text().split()
    if len(fields) < 2:
        raise AcceptanceError("invalid generated SSH public key")
    digest = hashlib.sha256(base64.b64decode(fields[1], validate=True)).digest()
    return "SHA256:" + base64.b64encode(digest).decode().rstrip("=")


# Selected canonical control records are read only inside this owned fixture.
# Only these scalar facts leave it; environments, raw output and native
# process authority never enter retained diagnostics.
CONTROL_EVIDENCE_CODE = r"""import json,os,stat,sys
from pathlib import Path
root=Path("/state/tasks")
keys=set("schema_version root_id task_id owner_kind scope_kind assurance deadline created_at started_at requested_at replied_at observed_at budget_nanos diagnostic_nanos cause result exit_code absence_proof terminated acknowledged state action numeric_task_id status stage code".split())
fixed=("task.json","meta.json","submit.json","supervisor.ref.json","provider.start","provider.started.json","outcome.json","lifetime/intent.json","lifetime/registration.json","lifetime/worker.json","lifetime/worker-wait.json","lifetime/terminated.json")
result={}
for task in sys.argv[1:]:
 if len(task)!=32 or any(c not in "0123456789abcdef" for c in task):raise RuntimeError("invalid owned task")
 base=root/task
 names=list(fixed)
 for sub in ("stop","lifetime"):
  directory=base/sub
  if directory.is_dir():
   names.extend(str(p.relative_to(base)) for p in sorted(directory.glob("*.json"))[:16])
 records={}
 for name in dict.fromkeys(names):
  path=base/name
  try:
   fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
  except FileNotFoundError:
   records[name]={"present":False};continue
  try:
   info=os.fstat(fd)
   if not stat.S_ISREG(info.st_mode):raise RuntimeError("control evidence must be regular")
   with os.fdopen(fd,"rb",closefd=False) as stream:data=stream.read(65537)
   if len(data)>65536:records[name]={"present":True,"summary":"over_bound"};continue
   value=json.loads(data)
   if not isinstance(value,dict):raise RuntimeError("control evidence must be an object")
   summary={k:v for k,v in value.items() if k in keys and (v is None or isinstance(v,(str,int,bool))) and (not isinstance(v,str) or len(v)<=256)}
   records[name]={"present":True,"facts":summary}
  finally:os.close(fd)
 receipts=Path("/root/.pi/remote-fixture-invocations")/task
 result[task]={"records":records,"actual_fixture_invocations":len(list(receipts.glob("*.json")))}
print(json.dumps(result,sort_keys=True))
"""


@dataclass
class Exercise:
    candidate: Path
    directory: Path
    supervisor: Path
    environment: dict[str, str]
    container: str = ""
    container_name: str = ""
    tasks: list[str] = field(default_factory=list)
    scenarios: list[dict[str, object]] = field(default_factory=list)
    control_count: int = 0
    closure_verified: bool = False
    container_removed: bool = False
    evidence_failures: list[str] = field(default_factory=list)

    def docker(self, *args: str, timeout: float = 45) -> bytes:
        return checked(["docker", *args], self.environment, timeout=timeout)

    def inside(self, *args: str, timeout: float = 45) -> bytes:
        return self.docker(
            "exec",
            "--env",
            f"PATH=/remote/fixture:{self.supervisor.parent}:/usr/local/bin:/usr/bin:/bin",
            "--env",
            "HOME=/root",
            self.container,
            *args,
            timeout=timeout,
        )

    def delegate(
        self, *args: str, allow: tuple[int, ...] = (0,), timeout: float = 60
    ) -> dict[str, object]:
        selected = list(args)
        if args[0] in {"devices", "dispatch"}:
            selected.extend(
                [
                    "--ssh-config",
                    str(self.directory / "origin-home" / ".ssh" / "config"),
                ]
            )
        result = command(
            [
                str(self.candidate / "delegate"),
                "--root",
                str(self.directory / "origin-state"),
                *selected,
                "--json",
            ],
            self.origin_environment(),
            timeout=timeout,
        )
        response = decode_object(result.output, "origin response")
        self.control_count += 1
        write_json(self.directory / f"control-{self.control_count:03d}.json", response)
        if result.exit_code not in allow:
            raise AcceptanceError(
                f"Delegate {args[0]} exit {result.exit_code}: {response.get('error', '')}"
            )
        return response

    def origin_environment(self) -> dict[str, str]:
        return {
            **self.environment,
            "HOME": str(self.directory / "origin-home"),
            "XDG_CONFIG_HOME": str(self.directory / "origin-home" / ".config"),
        }

    def prepare(self) -> None:
        self.directory.mkdir(mode=0o700)
        (self.directory / "fixture").mkdir(mode=0o700)
        fixture = self.directory / "fixture" / "pi"
        fixture.write_text(PI_SOURCE)
        fixture.chmod(0o700)
        for name, exit_code in (("remote-wrapper", 0), ("remote-failed-wrapper", 17)):
            wrapper = self.directory / "fixture" / name
            wrapper.write_text(
                LOST_ACK_SOURCE.replace(
                    "__DELEGATE__", repr(str(self.candidate / "delegate"))
                ).replace("__ACK_EXIT__", str(exit_code))
            )
            wrapper.chmod(0o700)
        for stem in ("client", "server"):
            checked(
                [
                    "ssh-keygen",
                    "-q",
                    "-t",
                    "ed25519",
                    "-N",
                    "",
                    "-f",
                    str(self.directory / stem),
                ],
                self.environment,
            )
        path = f"/remote/fixture:{self.supervisor.parent}:/usr/local/bin:/usr/bin:/bin"
        config = (
            "Port 2222\nListenAddress 0.0.0.0\nHostKey /remote/server\n"
            "AuthorizedKeysFile /remote/client.pub\nStrictModes no\nUsePAM no\n"
            "PermitRootLogin prohibit-password\nPasswordAuthentication no\n"
            "KbdInteractiveAuthentication no\nPubkeyAuthentication yes\n"
            f"SetEnv PATH={path} HOME=/root\nLogLevel ERROR\n"
        )
        (self.directory / "sshd_config").write_text(config)
        name = "delegate-remote-" + secrets.token_hex(8)
        self.container_name = name
        argv = [
            "create",
            "--name",
            name,
            "--label",
            "delegate.acceptance=" + name,
            "-p",
            "127.0.0.1::2222",
            "--mount",
            f"type=bind,src={self.directory},dst=/remote,readonly",
            "--mount",
            f"type=bind,src={self.candidate},dst={self.candidate},readonly",
            "--mount",
            f"type=bind,src={self.supervisor.parent},dst={self.supervisor.parent},readonly",
            "--mount",
            f"type=bind,src={REPOSITORY},dst={REPOSITORY},readonly",
            IMAGE,
        ]
        created = command(["docker", *argv], self.environment)
        if created.exit_code:
            self.recover_container_identity()
            raise AcceptanceError(
                "Docker container creation failed; checked recorded ownership"
            )
        self.container = created.output.decode().strip()
        if len(self.container) != 64:
            raise AcceptanceError("Docker did not return the owned container identity")
        self.docker("start", self.container)
        self.configure_origin()

    def recover_container_identity(self) -> None:
        if not self.container_name:
            return
        result = command(
            ["docker", "inspect", "--format", "{{json .}}", self.container_name],
            self.environment,
        )
        if result.exit_code:
            return
        description = decode_object(result.output, "owned container description")
        configuration = object_value(
            description.get("Config"), "container configuration"
        )
        labels = object_value(configuration.get("Labels"), "container labels")
        identity = description.get("Id")
        if labels.get("delegate.acceptance") != self.container_name or not isinstance(
            identity, str
        ):
            raise AcceptanceError("container name does not match recorded ownership")
        self.container = identity

    def configure_origin(self) -> None:
        ports = json.loads(
            self.docker(
                "inspect", "--format", "{{json .NetworkSettings.Ports}}", self.container
            )
        )
        mappings = object_value(ports, "Docker port map").get("2222/tcp")
        if not isinstance(mappings, list) or not mappings:
            raise AcceptanceError("SSH port is not published")
        mapping = object_value(mappings[0], "Docker port binding")
        port = mapping.get("HostPort")
        if not isinstance(port, str) or not port.isdigit():
            raise AcceptanceError("SSH port is not numeric")
        home = self.directory / "origin-home"
        (home / ".ssh").mkdir(parents=True, mode=0o700)
        config = (
            "Host remote-fixture\n HostName 127.0.0.1\n User root\n"
            f" Port {port}\n IdentityFile {self.directory / 'client'}\n IdentitiesOnly yes\n"
            " IdentityAgent none\n UserKnownHostsFile none\n GlobalKnownHostsFile none\n"
        )
        (home / ".ssh" / "config").write_text(config)
        self.wait_for_ssh(int(port))
        self.delegate(
            "devices",
            "trust",
            "--device",
            "remote-fixture",
            "--fingerprint",
            host_fingerprint(self.directory / "server.pub"),
        )
        probe = self.delegate(
            "devices",
            "probe",
            "--device",
            "remote-fixture",
            "--remote-root",
            REMOTE_ROOT,
            "--remote-executable",
            str(self.candidate / "delegate"),
            allow=(1,),
        )
        if object_value(probe.get("remote"), "probe response").get("failure") is None:
            raise AcceptanceError("unprepared destination probe incorrectly succeeded")
        absent = self.inside(
            "python3",
            "-c",
            "from pathlib import Path;print(int(not Path('/state').exists()))",
        )
        if absent.strip() != b"1":
            raise AcceptanceError("passive probe initialized remote state")
        self.scenarios.append({"name": "passive-probe", "state_initialized": False})
        self.inside("mkdir", "-p", "/work")
        self.inside("chmod", "700", "/work")
        initial_brief = self.directory / "initial-brief.json"
        write_json(
            initial_brief, {"answer": "must not launch before resident preparation"}
        )
        initial_id = secrets.token_hex(16)
        refused = self.delegate(
            "dispatch",
            "--device",
            "remote-fixture",
            "--provider",
            PROVIDER,
            "--remote-root",
            REMOTE_ROOT,
            "--remote-executable",
            str(self.candidate / "delegate"),
            "--remote-cwd",
            "/work",
            "--id",
            initial_id,
            "--brief",
            str(initial_brief),
            allow=(1,),
        )
        if not refused.get("error"):
            raise AcceptanceError("unprepared destination admitted a provider")
        created = self.inside(
            "python3",
            "-c",
            "from pathlib import Path;import sys;print(int((Path('/state/tasks')/sys.argv[1]).exists()))",
            initial_id,
        )
        if created.strip() != b"0":
            raise AcceptanceError("readiness refusal created an executable task")
        self.scenarios.append(
            {"name": "first-start-readiness", "provider_admitted": False}
        )
        self.inside(
            str(self.candidate / "delegate"),
            "--root",
            REMOTE_ROOT,
            "supervisor",
            "ensure-owner",
            "--json",
            timeout=90,
        )

    @staticmethod
    def wait_for_ssh(port: int) -> None:
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=1):
                    return
            except OSError:
                time.sleep(0.1)
        raise AcceptanceError("owned SSH daemon did not listen")

    def dispatch(
        self,
        name: str,
        *,
        delay: int,
        budget: str,
        snapshot: bool = False,
        ack_failure: Literal["disconnect", "endpoint"] | None = None,
    ) -> str:
        task = secrets.token_hex(16)
        brief = self.directory / f"{name}-brief.json"
        write_json(
            brief, {"task_id": task, "delay": delay, "answer": "remote SSH " + name}
        )
        executable = str(self.candidate / "delegate")
        if ack_failure is not None:
            wrapper = (
                "remote-wrapper"
                if ack_failure == "disconnect"
                else "remote-failed-wrapper"
            )
            executable = "/remote/fixture/" + wrapper
        args = [
            "dispatch",
            "--device",
            "remote-fixture",
            "--provider",
            PROVIDER,
            "--remote-root",
            REMOTE_ROOT,
            "--remote-executable",
            executable,
            "--id",
            task,
            "--brief",
            str(brief),
            "--budget",
            budget,
        ]
        if snapshot:
            source = self.directory / "input-source"
            source.mkdir(mode=0o700)
            (source / "selected.txt").write_text("declared SSH input\n")
            manifest = self.directory / "inputs.json"
            output = self.directory / "outputs.json"
            write_json(manifest, {"source": str(source), "paths": ["selected.txt"]})
            write_json(output, ["selected.txt"])
            args.extend(
                ["--input-manifest", str(manifest), "--output-manifest", str(output)]
            )
        else:
            args.extend(["--remote-cwd", "/work"])
        response = self.delegate(
            *args, allow=(3,) if ack_failure == "endpoint" else (0,), timeout=120
        )
        handle = response.get("task_id")
        if handle != "remote:" + task:
            raise AcceptanceError("origin did not return the exact remote handle")
        self.tasks.append(task)
        if ack_failure == "endpoint":
            error = response.get("error")
            if (
                not isinstance(error, str)
                or "remote acceptance is unknown" not in error
            ):
                raise AcceptanceError(
                    "failed endpoint did not preserve admission uncertainty"
                )
        self.scenarios.append(
            {
                "name": name,
                "task_id": task,
                "admission": "unknown" if ack_failure == "endpoint" else "created",
            }
        )
        return task

    def await_terminal(self, task: str, *, timeout: float = 60) -> dict[str, object]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            response = self.delegate("collect", "remote:" + task, allow=(0, 1, 3, 4))
            envelope = remote_envelope(response)
            if envelope.get("outcome") is not None or envelope.get("status") in {
                "timed_out",
                "interrupted",
            }:
                return envelope
            time.sleep(0.2)
        raise AcceptanceError("remote task did not reach a terminal observation")

    def exercise(self) -> None:
        task = self.dispatch("snapshot", delay=4, budget="30s", snapshot=True)
        (self.directory / "input-source" / "selected.txt").write_text(
            "changed after admission\n"
        )
        # The dispatch control process and SSH channel have both exited. The
        # remote provider's delayed result must still complete independently.
        envelope = self.await_terminal(task)
        if envelope.get("publication") != "committed":
            raise AcceptanceError("snapshot task did not commit")
        output = self.directory / "downloads"
        self.delegate("collect", "remote:" + task, "--out", str(output))
        selected = output / task / "files" / "selected.txt"
        if selected.read_text() != "declared SSH input\n":
            raise AcceptanceError("selected snapshot output bytes differ")
        if (
            output / task / "result" / "result.txt"
        ).read_text() != "remote SSH snapshot":
            raise AcceptanceError(
                "committed result did not fulfill the synthetic brief"
            )
        self.delegate("remote", "reconcile", "remote:" + task)
        self.exercise_acknowledgments()
        canceled = self.dispatch("cancel", delay=30, budget="35s")
        self.await_fixture_invocation(canceled)
        self.delegate("cancel", "remote:" + canceled)
        canceled_observation = self.await_terminal(canceled)
        continuation = canceled_observation.get("continuation")
        failure = canceled_observation.get("failure")
        if (
            canceled_observation.get("status") != "interrupted"
            or canceled_observation.get("outcome") is not None
            or not isinstance(continuation, dict)
            or continuation.get("reason") != "task was cancelled by the user"
            or continuation.get("resumable") is not False
            or not isinstance(failure, dict)
            or failure.get("code") != "task_interrupted"
            or failure.get("next_action") != "stop_and_report"
        ):
            raise AcceptanceError(
                "active cancellation did not retain its public interruption and user-stop evidence"
            )
        self.exercise_expiry()
        self.verify_closure()

    def exercise_acknowledgments(self) -> None:
        recovered = self.dispatch(
            "lost-ack", delay=2, budget="30s", ack_failure="disconnect"
        )
        if self.await_terminal(recovered).get("publication") != "committed":
            raise AcceptanceError(
                "accepted task was not recovered after acknowledgment loss"
            )
        self.verify_withheld_ack_task(recovered)
        uncertain = self.dispatch(
            "endpoint-failure", delay=2, budget="30s", ack_failure="endpoint"
        )
        reconciled = self.delegate("remote", "reconcile", "remote:" + uncertain)
        if (
            reconciled.get("task_id") != "remote:" + uncertain
            or object_value(reconciled.get("remote"), "reconciled response").get(
                "existing"
            )
            is not True
        ):
            raise AcceptanceError(
                "explicit reconciliation did not recover the same accepted task"
            )
        if self.await_terminal(uncertain).get("publication") != "committed":
            raise AcceptanceError(
                "accepted task was not recovered after explicit endpoint failure"
            )
        self.verify_withheld_ack_task(uncertain)

    def verify_withheld_ack_task(self, task: str) -> None:
        self.inside("test", "-f", "/lost-ack-" + task)
        launches = self.inside(
            "python3",
            "-c",
            "from pathlib import Path;import sys;print(len(list((Path('/root/.pi/remote-fixture-invocations')/sys.argv[1]).glob('*.json'))))",
            task,
        ).strip()
        if launches != b"1":
            raise AcceptanceError(
                "withheld acknowledgment actual provider invocation count must be one, observed "
                + launches.decode()
            )

    def exercise_expiry(self) -> None:
        # Require running work before testing expiry, so startup failure cannot
        # be mistaken for enforcement of the active provider's original budget.
        expired = self.dispatch("expiry", delay=30, budget="10s")
        self.await_fixture_invocation(expired)
        observation = self.await_terminal(expired)
        if observation.get("status") != "timed_out":
            raise AcceptanceError("original execution budget was not enforced")

    def capture_control_evidence(self, stage: str) -> None:
        if self.container and self.tasks:
            try:
                summary = decode_object(
                    self.inside("python3", "-c", CONTROL_EVIDENCE_CODE, *self.tasks),
                    "bounded fixture control summary",
                )
                write_json(self.directory / f"control-evidence-{stage}.json", summary)
            except Exception:
                # Diagnostic failure cannot prevent closure or expose private output.
                self.evidence_failures.append(f"control_evidence_{stage}_failed")

    def await_fixture_invocation(self, task: str) -> None:
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            count = self.inside(
                "python3",
                "-c",
                "from pathlib import Path;import sys;p=Path('/root/.pi/remote-fixture-invocations')/sys.argv[1];print(len(list(p.glob('*.json'))))",
                task,
            )
            if count.strip() == b"1":
                return
            self.delegate("status", "remote:" + task)
            time.sleep(0.1)
        raise AcceptanceError("finite provider did not start before cancellation")

    def verify_closure(self) -> None:
        result = decode_object(
            self.inside(
                "python3",
                "-c",
                CLOSURE_CODE,
                str(REPOSITORY),
                REMOTE_ROOT,
                *self.tasks,
                timeout=50,
            ),
            "closure proof",
        )
        if result.get("closed") is not True:
            raise AcceptanceError("canonical task closure is unproven")
        self.closure_verified = True

    def cleanup(self) -> None:
        try:
            self.capture_control_evidence("before-cleanup")
        finally:
            self._cleanup_resources()

    def _cleanup_resources(self) -> None:
        if not self.container:
            self.recover_container_identity()
            if not self.container:
                return
        running = self.docker(
            "inspect", "--format", "{{.State.Running}}", self.container
        ).strip()
        if running != b"true":
            self.docker("rm", self.container)
            self.container_removed = True
            return
        for task in self.tasks:
            self.delegate("cancel", "remote:" + task, allow=(0, 1, 3, 4))
        state_exists = (
            self.inside(
                "python3",
                "-c",
                "from pathlib import Path;print(int(Path('/state/root.json').is_file()))",
            ).strip()
            == b"1"
        )
        if state_exists:
            actual = json.loads(
                self.inside(
                    "python3",
                    "-c",
                    "import json;from pathlib import Path;p=Path('/state/tasks');print(json.dumps([x.name for x in p.iterdir()] if p.exists() else []))",
                )
            )
            if not isinstance(actual, list) or any(
                not isinstance(task, str) or len(task) != 32 for task in actual
            ):
                raise AcceptanceError("owned remote task inventory is invalid")
            for task in actual:
                self.inside(
                    str(self.candidate / "delegate"),
                    "--root",
                    REMOTE_ROOT,
                    "cancel",
                    task,
                    "--json",
                )
            self.tasks = sorted(set(self.tasks).union(actual))
            self.verify_closure()
        has_supervisor = (
            self.inside(
                "python3",
                "-c",
                "from pathlib import Path;print(int(Path('/state/.supervisor/pueue.yml').is_file()))",
            ).strip()
            == b"1"
        )
        if has_supervisor:
            self.inside(
                "python3",
                "-c",
                SHUTDOWN_CODE,
                str(REPOSITORY),
                REMOTE_ROOT,
                str(self.supervisor),
                *self.tasks,
                timeout=60,
            )
        try:
            self.capture_control_evidence("closed")
        finally:
            self.inside(
                str(self.candidate / "delegate"),
                "--root",
                REMOTE_ROOT,
                "supervisor",
                "stop-owner",
                "--json",
            )
            self.docker("stop", "--time", "10", self.container)
            self.docker("rm", self.container)
            self.container_removed = True
            for name in ("client", "server"):
                (self.directory / name).unlink(missing_ok=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--candidate", type=Path, required=True)
    parser.add_argument("--supervisor", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument(
        "--build",
        action="store_true",
        help="build the candidate pair directly from this checkout",
    )
    scenarios = parser.add_mutually_exclusive_group()
    scenarios.add_argument(
        "--ack-only",
        action="store_true",
        help="isolate both after-start acknowledgment-loss scenarios",
    )
    scenarios.add_argument(
        "--expiry-only",
        action="store_true",
        help="isolate the active-provider expiry scenario",
    )
    args = parser.parse_args()
    candidate = args.candidate.resolve()
    environment = dict(os.environ)
    if args.build:
        candidate.mkdir(mode=0o700, parents=True)
        build_candidate_pair(REPOSITORY, candidate)
    checked(
        [
            "docker",
            "build",
            "-t",
            IMAGE,
            str(REPOSITORY / "scripts" / "remote-acceptance"),
        ],
        environment,
        timeout=300,
    )
    args.output.resolve().parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    exercise = Exercise(
        candidate, args.output.resolve(), args.supervisor.resolve(), environment
    )
    failure = ""
    cleanup_failure = ""
    try:
        exercise.prepare()
        if args.expiry_only:
            exercise.exercise_expiry()
            exercise.verify_closure()
        elif args.ack_only:
            exercise.exercise_acknowledgments()
            exercise.verify_closure()
        else:
            exercise.exercise()
    except Exception as error:
        failure = str(error)
    finally:
        try:
            exercise.cleanup()
        except Exception as error:
            cleanup_failure = str(error)
    receipt = {
        "scope": "Linux container SSH with finite Pi fixture",
        "candidate": str(candidate),
        "scenarios": exercise.scenarios,
        "observed_task_ids": exercise.tasks,
        "failure": failure,
        "cleanup_failure": cleanup_failure,
        "evidence_failure": "; ".join(exercise.evidence_failures),
        "canonical_closure": exercise.closure_verified,
        "container_removed": exercise.container_removed,
        "remaining_container": (exercise.container or exercise.container_name)
        if not exercise.container_removed
        else "",
        "unverified": ["real provider account", "native macOS", "logind logout policy"],
    }
    receipt_path = args.output.resolve() / "receipt.json"
    if args.output.is_dir():
        write_json(receipt_path, receipt)
    print(json.dumps(receipt, sort_keys=True))
    return 1 if failure or cleanup_failure or exercise.evidence_failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
