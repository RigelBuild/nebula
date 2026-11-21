"""Kubernetes secret values that leaked past agent.redact (RIG-11725).

The shapes match the upstream secret-redact extension: the ``data`` and ``stringData`` of a
``kind: Secret`` object, and the ``value`` of an env entry whose ``name`` is secret-named, in YAML,
JSON and ``kubectl describe`` output. Neutral keys, non-Secret kinds, ``valueFrom`` entries and
non-secret names stay byte-identical.
"""

import base64
import time

import pytest

from agent.redact import redact_sensitive_text

V = "zzqq-fixture-8k2Lm4Pq9Wx"
B64 = base64.b64encode(V.encode()).decode()
# The part of V that no head/tail mask shows.
MIDDLE = V[6:-4]

SECRET_YAML = f"apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\ntype: Opaque\ndata:\n  connection: {B64}\n"
SECRET_STRING_DATA_YAML = f"kind: Secret\nstringData:\n  connection: {V}\n"
SECRET_JSON_PRETTY = '{\n  "kind": "Secret",\n  "data": {\n    "connection": "' + B64 + '"\n  }\n}'
SECRET_JSON_COMPACT = '{"kind":"Secret","data":{"connection":"' + B64 + '"}}'
SECRET_STRING_DATA_JSON = '{"kind":"Secret","stringData":{"connection":"' + V + '"}}'

ENV_YAML = (
    "spec:\n  containers:\n  - name: app\n    env:\n"
    f"    - name: DB_PASSWORD\n      value: {V}\n"
    "    - name: LOG_LEVEL\n      value: info\n"
)
ENV_JSON_COMPACT = '{"name":"DB_PASSWORD","value":"' + V + '"}'
ENV_JSON_PRETTY = '{\n  "name": "DB_PASSWORD",\n  "value": "' + V + '"\n}'
ENV_JSON_REVERSED = '{"value":"' + V + '","name":"DB_PASSWORD"}'

DESCRIBE = (
    "Name:         web-0\nNamespace:    app\nContainers:\n  app:\n    Image:  example/app:1\n"
    "    Environment:\n"
    f"      DB_PASSWORD:  {V}\n"
    "      LOG_LEVEL:    info\n"
    "      API_TOKEN:    <set to the key 'token' in secret 'api'>\n"
    "    Mounts:\n      /data from data (rw)\n"
)


@pytest.fixture(autouse=True)
def _ensure_redaction_enabled(monkeypatch):
    monkeypatch.delenv("HERMES_REDACT_SECRETS", raising=False)
    monkeypatch.setattr("agent.redact._REDACT_ENABLED", True)


def _modes(text: str) -> list[str]:
    return [redact_sensitive_text(text, force=True),
            redact_sensitive_text(text, force=True, file_read=True, secret_file=True),
            redact_sensitive_text(text)]


class TestSecretData:
    """A value under ``data:`` or ``stringData:`` of a kind: Secret object is a credential."""

    @pytest.mark.parametrize("text, secret", [
        (SECRET_YAML, B64),
        (SECRET_JSON_PRETTY, B64),
        (SECRET_JSON_COMPACT, B64),
        (SECRET_STRING_DATA_YAML, V),
        (SECRET_STRING_DATA_JSON, V),
    ])
    def test_value_masked_in_every_mode(self, text, secret):
        for out in _modes(text):
            assert secret not in out, out

    @pytest.mark.parametrize("text", [SECRET_YAML, SECRET_JSON_COMPACT])
    def test_object_keys_kept(self, text):
        out = redact_sensitive_text(text, force=True)
        assert "connection" in out and "Secret" in out

    def test_configmap_data_kept(self):
        text = f"apiVersion: v1\nkind: ConfigMap\ndata:\n  connection: {B64}\n"
        assert _modes(text) == [text, text, text]


class TestEnvValues:
    """An env ``value`` whose sibling ``name`` is secret-named is masked; the rest stay."""

    @pytest.mark.parametrize("text", [ENV_YAML, ENV_JSON_COMPACT, ENV_JSON_PRETTY, ENV_JSON_REVERSED])
    def test_secret_named_value_masked(self, text):
        for out in _modes(text):
            assert V not in out, out
            assert MIDDLE not in out, out

    def test_truncated_secret_json_masked(self):
        text = '{"kind":"Secret","data":{"a":"' + B64 + '","connection":"' + B64[:20]
        for out in _modes(text):
            assert B64[:20] not in out, out

    @pytest.mark.parametrize("text", [ENV_YAML, "[" + ENV_JSON_COMPACT + ',{"name":"LOG_LEVEL","value":"info"}]'])
    def test_plain_name_value_kept(self, text):
        out = redact_sensitive_text(text, force=True)
        assert "LOG_LEVEL" in out
        assert "value: info" in out or '"value":"info"' in out

    @pytest.mark.parametrize("text", [
        "    - name: DB_PASSWORD\n      valueFrom:\n        secretKeyRef:\n          name: db\n          key: password\n",
        '{"name":"DB_PASSWORD","valueFrom":{"secretKeyRef":{"name":"db","key":"password"}}}',
    ])
    def test_value_from_kept(self, text):
        assert _modes(text) == [text, text, text]


class TestDescribe:
    """``kubectl describe`` env lines: ``NAME:  value``, read only inside an Environment block."""

    def test_secret_named_value_masked(self):
        for out in _modes(DESCRIBE):
            assert MIDDLE not in out, out

    def test_plain_name_and_reference_kept(self):
        for out in _modes(DESCRIBE):
            assert "LOG_LEVEL:    info" in out, out
            assert "<set to the key 'token' in secret 'api'>" in out, out


class TestPerformance:
    def test_one_megabyte_under_a_second(self):
        block = SECRET_YAML + ENV_YAML + SECRET_JSON_PRETTY + ENV_JSON_PRETTY
        big = block * (1_000_000 // len(block) + 1)
        assert len(big) >= 1_000_000
        start = time.perf_counter()
        out = redact_sensitive_text(big, force=True)
        elapsed = time.perf_counter() - start
        assert elapsed < 1.0, elapsed
        assert V not in out and B64 not in out

    def test_one_megabyte_mixed_shapes_leaks_nothing(self):
        block = SECRET_YAML + ENV_YAML + SECRET_JSON_COMPACT + ENV_JSON_COMPACT + DESCRIBE
        big = block * (1_000_000 // len(block) + 1)
        out = redact_sensitive_text(big, force=True)
        assert V not in out and B64 not in out
