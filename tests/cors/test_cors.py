"""Integration tests for the cors filter through Envoy -> ext_proc -> echo backend."""

import pytest
import requests

ENVOY_BASE_URL = "http://localhost:8080"
REQUEST_TIMEOUT = 10
APP_ORIGIN = "https://app.example.com"
EVIL_ORIGIN = "https://evil.example.net"


@pytest.fixture
def session():
    s = requests.Session()
    yield s
    s.close()


def lower_headers(resp):
    return {k.lower(): v for k, v in resp.headers.items()}


def preflight(session, path, origin, method="PUT", headers="authorization"):
    return session.options(
        f"{ENVOY_BASE_URL}{path}",
        headers={
            "Origin": origin,
            "Access-Control-Request-Method": method,
            "Access-Control-Request-Headers": headers,
        },
        timeout=REQUEST_TIMEOUT,
    )


def test_preflight_allowed_is_answered_by_gateway(session):
    resp = preflight(session, "/cors/public/items", APP_ORIGIN)
    assert resp.status_code == 204, resp.text
    h = lower_headers(resp)
    assert h.get("access-control-allow-origin") == APP_ORIGIN
    assert h.get("access-control-allow-methods") == "GET, POST, PUT"
    assert h.get("access-control-allow-headers") == "authorization, content-type"
    assert h.get("access-control-allow-credentials") == "true"
    assert h.get("access-control-max-age") == "600"
    assert "origin" in h.get("vary", "").lower()
    # Answered by the engine: the echo backend's JSON body never appears.
    assert resp.text == ""


def test_preflight_disallowed_origin_is_rejected(session):
    resp = preflight(session, "/cors/public/items", EVIL_ORIGIN)
    assert resp.status_code == 403
    assert "access-control-allow-origin" not in lower_headers(resp)


def test_actual_request_from_allowed_origin(session):
    resp = session.get(f"{ENVOY_BASE_URL}/cors/public/items", headers={"Origin": APP_ORIGIN}, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200
    h = lower_headers(resp)
    assert h.get("access-control-allow-origin") == APP_ORIGIN
    assert h.get("access-control-allow-credentials") == "true"
    assert h.get("access-control-expose-headers") == "x-request-id"
    assert "origin" in h.get("vary", "").lower()


def test_regex_origin_is_anchored(session):
    ok = session.get(f"{ENVOY_BASE_URL}/cors/public/items",
                     headers={"Origin": "https://pr-42.preview.example.com"}, timeout=REQUEST_TIMEOUT)
    assert lower_headers(ok).get("access-control-allow-origin") == "https://pr-42.preview.example.com"

    attack = session.get(f"{ENVOY_BASE_URL}/cors/public/items",
                         headers={"Origin": "https://pr-42.preview.example.com.evil.net"}, timeout=REQUEST_TIMEOUT)
    assert attack.status_code == 200  # forwarded, but without CORS headers
    assert "access-control-allow-origin" not in lower_headers(attack)


def test_request_without_origin_is_untouched(session):
    resp = session.get(f"{ENVOY_BASE_URL}/cors/public/items", timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200
    assert not any(k.startswith("access-control-") for k in lower_headers(resp))


def test_preflight_bypasses_auth_and_errors_carry_cors_headers(session):
    # Browsers send preflights without credentials: CORS must answer before auth runs.
    pre = preflight(session, "/cors/auth/me", APP_ORIGIN, method="GET")
    assert pre.status_code == 204

    # The real request without credentials is rejected by the auth step, and the
    # 401 still carries CORS headers so the page can read it.
    denied = session.get(f"{ENVOY_BASE_URL}/cors/auth/me", headers={"Origin": APP_ORIGIN}, timeout=REQUEST_TIMEOUT)
    assert denied.status_code == 401
    assert lower_headers(denied).get("access-control-allow-origin") == APP_ORIGIN

    allowed = session.get(f"{ENVOY_BASE_URL}/cors/auth/me",
                          headers={"Origin": APP_ORIGIN, "Authorization": "Bearer x"}, timeout=REQUEST_TIMEOUT)
    assert allowed.status_code == 200


def test_block_disallowed_origins(session):
    blocked = session.post(f"{ENVOY_BASE_URL}/cors/block/orders", headers={"Origin": EVIL_ORIGIN}, timeout=REQUEST_TIMEOUT)
    assert blocked.status_code == 403
    ok = session.post(f"{ENVOY_BASE_URL}/cors/block/orders", headers={"Origin": APP_ORIGIN}, timeout=REQUEST_TIMEOUT)
    assert ok.status_code == 200


def test_wildcard_origin_echoes_method_and_headers(session):
    pre = preflight(session, "/cors/wildcard/data", "https://any.site", method="DELETE", headers="x-custom")
    assert pre.status_code == 204
    h = lower_headers(pre)
    assert h.get("access-control-allow-origin") == "*"
    assert h.get("access-control-allow-methods") == "DELETE"
    assert h.get("access-control-allow-headers") == "x-custom"
    assert "access-control-allow-credentials" not in h
