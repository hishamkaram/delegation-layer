"""Source fingerprints for release gate selection."""
from pathlib import Path
from scripts.acceptance.evidence.source import fingerprint_sources
from scripts.release_verification.contract import SOURCE_FILES

def source_seeds(kind: str) -> tuple[str, ...]:
    return ("scripts/verify_release_archive.py", *SOURCE_FILES[kind])

def source_fingerprint(kind: str) -> dict[str, object]:
    return fingerprint_sources(Path(__file__).resolve().parents[2], source_seeds(kind))
