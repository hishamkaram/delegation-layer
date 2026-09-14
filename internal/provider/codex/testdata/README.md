# Codex JSONL fixtures

These files contain sanitized, deterministic Codex 0.154.0 event streams. The
interpreter tests also build boundary cases in memory so malformed JSON, raw
invalid UTF-8, exact no-newline EOF, and byte-level writer faults stay visible
without relying on filesystem behavior.

Every fixture uses the UUID `123e4567-e89b-12d3-a456-426614174000`; it is a
fixture identity only. `codex-last-message.txt` is supplied separately by the
named-evidence test double when an output artifact is present.
