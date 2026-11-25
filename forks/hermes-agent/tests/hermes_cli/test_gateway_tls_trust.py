"""Gateway TLS trust is scoped to the macOS gateway process."""
import ssl
import sys
from collections import namedtuple

import pytest
import truststore

from agent import ssl_verify


@pytest.fixture
def trust_state(monkeypatch):
    from truststore._ssl_constants import _original_SSLContext

    monkeypatch.setattr(ssl, "SSLContext", _original_SSLContext)
    monkeypatch.setattr(ssl_verify, "_gateway_openssl", False, raising=False)
    monkeypatch.setattr(ssl_verify, "_installed", None)
    monkeypatch.setattr(ssl_verify, "_CA_CONTEXTS", {})
    calls = []
    monkeypatch.setattr(truststore, "inject_into_ssl", lambda: calls.append("inject"))
    monkeypatch.setattr(truststore, "extract_from_ssl", lambda: calls.append("extract"))
    return calls


@pytest.mark.parametrize("argv", [
    ["hermes", "gateway", "run"],
    ["hermes", "gateway", "--accept-hooks"],
    ["hermes", "-p", "x", "gateway", "run"],
], ids=["macmini", "accept-hooks", "profile"])
def test_gateway_cli_starts_without_truststore(monkeypatch, trust_state, argv):
    from hermes_cli import main as cli
    from hermes_cli import gateway

    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setattr(sys, "argv", argv)
    if "-p" in argv:
        monkeypatch.setattr("hermes_cli.profiles.resolve_profile_env", lambda name: "/tmp/profiles/x")
        monkeypatch.setenv("HERMES_HOME", "/tmp/profiles/x")
        monkeypatch.setattr(cli, "_explicit_cli_profile", None)
        cli._apply_profile_override()
    monkeypatch.setattr(cli, "_prepare_agent_startup", lambda args: None)
    class Started(Exception):
        pass

    def stop(*args, **kwargs):
        ssl_verify.install_truststore()
        raise Started

    monkeypatch.setattr(gateway, "_guard_official_docker_root_gateway", stop)
    with pytest.raises(Started):
        cli.main()
    assert trust_state == ["inject", "extract"]


def test_gateway_module_entry(monkeypatch, trust_state):
    from gateway import run

    monkeypatch.setattr(sys, "platform", "darwin")
    class Started(Exception):
        pass

    def stop():
        ssl_verify.install_truststore()
        raise Started

    monkeypatch.setattr(run, "_guard_corrupt_user_config", stop)
    with pytest.raises(Started):
        run.main()
    assert trust_state == []


def test_non_gateway_still_injects(monkeypatch, trust_state):
    from hermes_cli import main as cli

    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setattr(sys, "argv", ["hermes", "gateway", "status"])
    monkeypatch.setattr(cli, "_prepare_agent_startup", lambda args: None)
    monkeypatch.setattr(cli, "cmd_gateway", lambda args: None)
    cli.main()
    assert trust_state == ["inject"]


def test_non_darwin_keeps_truststore(monkeypatch, trust_state):
    monkeypatch.setattr(sys, "platform", "linux")
    ssl_verify.use_openssl_trust()
    ssl_verify.install_truststore()
    assert trust_state == ["inject"]


def test_gateway_context_uses_certifi_when_openssl_has_no_store(monkeypatch, trust_state, tmp_path):
    import certifi

    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setenv("SSL_CERT_FILE", str(tmp_path / "missing"))
    monkeypatch.setenv("SSL_CERT_DIR", str(tmp_path / "missing"))
    monkeypatch.delenv("NIX_SSL_CERT_FILE", raising=False)
    paths = namedtuple("Paths", "cafile capath")(None, None)
    monkeypatch.setattr(ssl, "get_default_verify_paths", lambda: paths)
    expected = len(ssl.create_default_context(cafile=certifi.where()).get_ca_certs())
    ssl_verify.use_openssl_trust()
    assert len(ssl_verify.platform_ssl_context().get_ca_certs()) == expected > 0
    assert trust_state == []


def test_gateway_context_prefers_existing_store(monkeypatch, trust_state, tmp_path):
    import certifi

    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setenv("SSL_CERT_FILE", str(tmp_path / "missing"))
    monkeypatch.setenv("SSL_CERT_DIR", str(tmp_path / "missing"))
    monkeypatch.delenv("NIX_SSL_CERT_FILE", raising=False)
    paths = namedtuple("Paths", "cafile capath")(certifi.where(), None)
    monkeypatch.setattr(ssl, "get_default_verify_paths", lambda: paths)
    ssl_verify.use_openssl_trust()
    assert len(ssl_verify.platform_ssl_context().get_ca_certs()) == 0


def test_restart_extracts_before_api_server_import(monkeypatch, trust_state):
    from hermes_cli import main as cli, gateway as gw
    from truststore._ssl_constants import _original_SSLContext

    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setattr(sys, "argv", ["hermes", "gateway", "restart"])
    monkeypatch.setattr(cli, "_prepare_agent_startup", lambda args: None)
    monkeypatch.setattr(truststore, "inject_into_ssl", lambda: setattr(ssl, "SSLContext", truststore.SSLContext))
    monkeypatch.setattr(truststore, "extract_from_ssl", lambda: setattr(ssl, "SSLContext", _original_SSLContext))
    monkeypatch.setattr(gw, "_refuse_from_inside_gateway", lambda *a: None)
    monkeypatch.setattr("hermes_cli.gateway_profile_lifecycle.profile_lifecycle", lambda *a: False)
    monkeypatch.setattr(gw, "_guard_named_profile_under_multiplexer", lambda **k: None)
    monkeypatch.setattr("hermes_cli.gateway_lifecycle_guards.refuse_while_sessions_live", lambda *a: None)
    monkeypatch.setattr(gw, "_dispatch_via_service_manager_if_s6", lambda *a: False)
    monkeypatch.setattr(gw, "_reap_unsupervised_gateway_orphans", lambda: None)
    monkeypatch.setattr(gw, "_installed_service_kind_for", lambda *a: None)
    monkeypatch.setattr("gateway.status.get_running_pid", lambda: None)
    monkeypatch.setattr("hermes_cli.gateway_supervised_restart.refuse_restart_of_service_managed_gateway", lambda *a: None)
    monkeypatch.setattr(gw, "stop_profile_gateway", lambda: False)
    monkeypatch.setattr(gw, "_wait_for_gateway_exit", lambda **k: None)

    class Reached(Exception):
        pass

    def check():
        assert ssl.SSLContext is _original_SSLContext
        raise Reached

    monkeypatch.setattr(gw, "_wait_for_api_server_port_free", check)
    with pytest.raises(Reached):
        cli.main()


def test_legacy_gateway_entry(monkeypatch, trust_state):
    import cli
    from hermes_cli import resource_limits

    monkeypatch.setattr(sys, "platform", "darwin")
    class Started(Exception):
        pass

    def stop():
        ssl_verify.install_truststore()
        raise Started

    monkeypatch.setattr(resource_limits, "apply_malloc_arena_max", stop)
    with pytest.raises(Started):
        cli._run_legacy_gateway()
    assert trust_state == []


def test_gateway_does_not_change_environment(monkeypatch, trust_state):
    import os

    monkeypatch.setattr(sys, "platform", "darwin")
    before = dict(os.environ)
    ssl_verify.use_openssl_trust()
    assert dict(os.environ) == before
