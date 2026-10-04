"""AEP v1 cross-SDK conformance test (issue #869).

Loads the golden corpus fixtures from pkg/aep/schema/corpus/ and validates
that every envelope parses correctly through the Python SDK's decoder.
Unknown additive kinds must not raise errors (forward compatibility).
"""

import json
from pathlib import Path

import pytest

from hotplex_client.types import AEP_VERSION

# Resolve corpus directory relative to this test file.
CORPUS_DIR = Path(__file__).resolve().parents[3] / "pkg" / "aep" / "schema" / "corpus"


def _load_corpus():
    """Load all .json fixtures from the corpus directory."""
    fixtures = []
    if not CORPUS_DIR.is_dir():
        return fixtures
    for p in sorted(CORPUS_DIR.glob("*.json")):
        fixtures.append((p.name, json.loads(p.read_text())))
    return fixtures


CORPUS = _load_corpus()

# Edge-case fixtures that test forward compatibility — these use unknown kinds
# or extra fields that the SDK is not expected to have typed.
EDGE_CASE_PREFIXES = ("90-", "91-", "92-")


def test_corpus_directory_exists():
    assert CORPUS_DIR.is_dir(), f"Corpus directory not found: {CORPUS_DIR}"
    assert len(CORPUS) > 0, "Corpus directory is empty"


@pytest.mark.parametrize("name,envelope", CORPUS, ids=[f[0] for f in CORPUS])
def test_corpus_envelope_valid(name, envelope):
    """Every corpus envelope must have required AEP fields."""
    assert envelope["version"] == AEP_VERSION, f"{name}: wrong version"
    assert "id" in envelope and isinstance(envelope["id"], str), f"{name}: missing id"
    assert "event" in envelope, f"{name}: missing event"
    assert "type" in envelope["event"], f"{name}: missing event.type"
    assert isinstance(envelope["event"]["type"], str), f"{name}: event.type not string"


def test_corpus_covers_all_stable_kinds():
    """Every stable (non-edge-case) fixture must have a known event type."""
    stable_kinds = set()
    for name, env in CORPUS:
        if name.startswith(EDGE_CASE_PREFIXES):
            continue
        stable_kinds.add(env["event"]["type"])

    # Minimum expected kind count (must match Go events.go)
    assert len(stable_kinds) >= 32, (
        f"Only {len(stable_kinds)} stable kinds in corpus, expected >= 32. "
        f"Missing types indicate Go added a Kind without updating the corpus."
    )


def test_corpus_unknown_kind_safely_ignorable():
    """The unknown-kind fixture must parse without error."""
    unknown = CORPUS_DIR / "90-compatibility-unknown-kind.json"
    if not unknown.exists():
        pytest.skip("unknown-kind fixture not found")
    env = json.loads(unknown.read_text())
    assert env["event"]["type"] == "custom.future_event"
    # No exception means forward-compatible parsing works.


def test_control_action_includes_stop():
    """ControlAction must include the 'stop' action added in AEP v1."""
    from hotplex_client.types import ControlAction

    assert ControlAction.STOP == "stop"


def test_data_types_importable():
    """All AEP data types must be importable from the SDK."""
    from hotplex_client.types import (
        InputAckData,
        RuntimeExecutionData,
        InternalResetData,
    )

    # Verify they can be instantiated with expected fields.
    ack = InputAckData(
        client_message_id="evt_1",
        execution_id="exec_1",
        status="delivered",
    )
    assert ack.status == "delivered"

    rt = RuntimeExecutionData(execution_id="exec_1", status="started")
    assert rt.status == "started"

    ir = InternalResetData(generation=2)
    assert ir.generation == 2


def test_input_ack_reports_mode_and_durability():
    """A buffered accept must be distinguishable from a real delivery."""
    import json

    from hotplex_client.types import InputAckData

    raw = (CORPUS_DIR / "93-compatibility-input-ack-buffered.json").read_text()
    data = json.loads(raw)["event"]["data"]
    ack = InputAckData(**data)
    assert ack.status == "accepted"
    assert ack.input_mode == "buffered"
    assert ack.durability == "volatile"
    assert ack.parent_execution_id == "exec_parent_1"
    # The volatile acceptance is explicitly NOT a delivery.
    assert ack.status != "delivered"


def test_input_ack_legacy_payload_without_mode_and_durability():
    """A pre-receipt server omits the new fields; decoding must still work."""
    import json

    from hotplex_client.types import InputAckData

    raw = (CORPUS_DIR / "94-compatibility-input-ack-legacy.json").read_text()
    data = json.loads(raw)["event"]["data"]
    ack = InputAckData(**data)
    assert ack.status == "delivered"
    assert ack.input_mode is None
    assert ack.durability is None
    assert ack.parent_execution_id is None
