"""Integration tests for the jwt_auth filter through Envoy -> ext_proc -> echo backend."""

import base64
import hashlib
import hmac
import json
import time

import pytest
import requests

ENVOY_BASE_URL = "http://localhost:8080"
REQUEST_TIMEOUT = 10
SECRET = "integration-test-secret"
ISSUER = "https://issuer.hypergate.test"
AUDIENCE = "hypergate-tests"


def _b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def make_jwt(claims: dict, secret: str = SECRET) -> str:
    """Sign an HS256 JWT with the standard library only."""
    header = {"alg": "HS256", "typ": "JWT"}
    signing_input = ".".join(
        _b64(json.dumps(part, separators=(",", ":")).encode()) for part in (header, claims)
    )
    sig = hmac.new(secret.encode(), signing_input.encode(), hashlib.sha256).digest()
    return f"{signing_input}.{_b64(sig)}"


def valid_claims(**extra) -> dict:
    now = int(time.time())
    claims = {"sub": "alice", "iss": ISSUER, "aud": AUDIENCE, "iat": now, "exp": now + 3600}
    claims.update(extra)
    return claims


@pytest.fixture
def session():
    s = requests.Session()
    yield s
    s.close()


def upstream(resp) -> dict:
    """The echo backend reflects what it received."""
    data = resp.json()
    headers = data.get("request", {}).get("headers", {}) or data.get("headers", {})
    return {
        "headers": {k.lower(): v for k, v in headers.items()},
        "url": data.get("http", {}).get("originalUrl", "") or data.get("request", {}).get("url", ""),
        "cookies": data.get("request", {}).get("cookies", {}),
    }


def get(session, path, **kwargs):
    return session.get(f"{ENVOY_BASE_URL}{path}", timeout=REQUEST_TIMEOUT, **kwargs)


def test_missing_token_is_challenged(session):
    resp = get(session, "/jwt/header/me")
    assert resp.status_code == 401
    assert resp.headers.get("www-authenticate", "").startswith("Bearer")
    assert resp.json()["message"] == "missing authentication token"


@pytest.mark.parametrize("token,label", [
    (make_jwt(valid_claims(), secret="wrong-secret"), "bad signature"),
    (make_jwt(valid_claims(exp=int(time.time()) - 60)), "expired"),
    (make_jwt(valid_claims(iss="https://other-issuer")), "wrong issuer"),
    (make_jwt(valid_claims(aud="other-api")), "wrong audience"),
    ("not.a.jwt", "garbage"),
])
def test_invalid_tokens_are_rejected(session, token, label):
    resp = get(session, "/jwt/header/me", headers={"Authorization": f"Bearer {token}"})
    assert resp.status_code == 401, label
    assert 'error="invalid_token"' in resp.headers.get("www-authenticate", ""), label


@pytest.mark.parametrize("scheme", ["Bearer", "bearer", "BEARER"])
def test_valid_token_maps_claims_and_strips_token(session, scheme):
    token = make_jwt(valid_claims(tenantId="acme", roles=["admin", "dev"]))
    resp = get(session, "/jwt/header/me", headers={"Authorization": f"{scheme} {token}"})
    assert resp.status_code == 200, resp.text
    up = upstream(resp)["headers"]
    assert up.get("x-user-id") == "alice"
    assert up.get("x-tenant") == "acme"            # camelCase claim mapped
    assert up.get("x-roles") == "admin,dev"        # arrays are comma-separated
    assert "authorization" not in up               # strip_token


def test_client_cannot_spoof_mapped_headers(session):
    token = make_jwt(valid_claims())  # no email claim
    resp = get(session, "/jwt/header/me", headers={
        "Authorization": f"Bearer {token}",
        "X-User-Email": "admin@corp.example",   # forged
        "X-User-Id": "mallory",                 # forged, must be replaced by the claim
    })
    assert resp.status_code == 200
    up = upstream(resp)["headers"]
    assert "x-user-email" not in up
    assert up.get("x-user-id") == "alice"


def test_query_token_is_stripped_from_upstream_path(session):
    token = make_jwt(valid_claims(sub="bob"))
    resp = get(session, "/jwt/query/data", params={"access_token": token, "page": "2"})
    assert resp.status_code == 200, resp.text
    up = upstream(resp)
    assert up["headers"].get("x-user-id") == "bob"
    assert up["url"], "echo backend did not report the request URL"
    assert "access_token" not in up["url"]
    assert "page=2" in up["url"]


def test_cookie_token_is_stripped_and_other_cookies_kept(session):
    token = make_jwt(valid_claims(sub="carol"))
    resp = get(session, "/jwt/cookie/data", headers={"Cookie": f"theme=dark; session_jwt={token}"})
    assert resp.status_code == 200, resp.text
    up = upstream(resp)
    assert up["headers"].get("x-user-id") == "carol"
    cookie_header = up["headers"].get("cookie", "")
    assert "session_jwt" not in cookie_header
    assert "theme=dark" in cookie_header
