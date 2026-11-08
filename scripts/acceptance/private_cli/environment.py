"""Private CLI environment boundary."""


def phase_environment(environment, phase):
    value = dict(environment)
    value["DELEGATE_ACCEPTANCE_PHASE"] = phase
    return value
