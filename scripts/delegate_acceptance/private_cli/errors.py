"""Private CLI errors boundary."""


class AcceptanceBlocked(RuntimeError):
    """A required native prerequisite was unavailable or inconclusive."""

    def __init__(self, code):
        super().__init__(code)
        self.code = code
