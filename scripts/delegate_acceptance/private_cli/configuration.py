"""Private acceptance constants and repository location."""



TASK_BUDGET = "2m"


COLLECT_WATCH = "150s"


COLLECT_TIMEOUT_SECONDS = 165


CAPACITY_TASK_BUDGET = "30m"


CAPACITY_TARGET_TASKS = 200


DEFAULT_STOP_SCENARIO = "default-stop"


EXTERNAL_INSPECTION_HANDOFF_SCENARIO = "external-inspection-handoff"


SUPERVISOR_UPGRADE_RECOVERY_SCENARIO = "supervisor-upgrade-recovery"


DEFAULT_STOP_TASK_COUNT = 2


EXTERNAL_INSPECTION_HANDOFF_TASK_COUNT = 2


EXTERNAL_INSPECTION_HANDOFF_TASK_BUDGET = "5m"


DEFAULT_STOP_TASK_BUDGET = "5s"


DEFAULT_STOP_BUDGET = "900ms"


DEFAULT_DISPATCH_WORKERS = 16


PRIVATE_ACCEPTANCE_PREFIX = ".dl-"


LEASE_HANDSHAKE_TIMEOUT_SECONDS = 5


STARTUP_CONTROL_COMMANDS = frozenset(
    {"clean", "kill", "pause", "parallel", "remove", "reset", "restart", "shutdown", "start", "unpause"}
)


PROTECTED_CALLER_PHASES = frozenset(
    {
        "inspection-worker",
        "inspection-observation",
        "managed-runner",
        "managed-runner-observation",
        "managed-runner-collection",
    }
)



