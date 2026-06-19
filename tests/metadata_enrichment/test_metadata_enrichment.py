import pytest
import requests
import redis
import time
import json

ENVOY_BASE_URL = "http://localhost:8080"
REQUEST_TIMEOUT = 10

@pytest.fixture(scope="session")
def redis_client():
    """Connect directly to the local Redis instance on port 6379."""
    client = redis.Redis(host="localhost", port=6379, db=0, decode_responses=True)
    yield client
    # Cleanup any wildcard keys created during the test lifecycle
    for key in client.scan_iter("user_meta:*"):
        client.delete(key)
    client.close()

@pytest.fixture
def session():
    """Local self-contained session fixture to bypass conftest.py lookup issues."""
    s = requests.Session()
    yield s
    s.close()

def extract_received_headers(response: requests.Response) -> dict[str, str]:
    """Helper to extract headers received by the backend echo-server."""
    try:
        data = response.json()
    except ValueError as exc:
        pytest.fail(f"Failed to parse echo-server response as JSON: {exc}")
    
    received = data.get("request", {}).get("headers", {}) or data.get("headers", {})
    return {k.lower(): v for k, v in received.items()}

def get_response_headers(response: requests.Response) -> dict[str, str]:
    """Helper to extract response headers returned downstream to the client."""
    return {k.lower(): v for k, v in response.headers.items()}


def test_doctor_enrichment_and_limit(session, redis_client):
    """
    Test Case 1: Specific Role (Doctor)
    1. Pre-seed Redis with a JSON payload representing a 'doctor'.
    2. Request /metadata with X-User-ID set to 'doctor-user'.
    3. Verify that the echo backend received 'X-User-Tier: doctor' and the full raw JSON.
    4. Verify that the rate limit (3 req/min) is correctly enforced.
    """
    # Wait for a clean minute boundary to prevent transient fixed window splits
    current_sec = time.localtime().tm_sec
    wait_time = (60 - current_sec) % 60
    if wait_time < 5:
        wait_time += 60
    time.sleep(wait_time)

    user_id = "doctor-user"
    redis_key = f"user_meta:{user_id}"
    user_payload = {
        "profile": {
            "tier": "doctor"
        },
        "status": "active"
    }

    # Inject JSON payload into Redis
    redis_client.set(redis_key, json.dumps(user_payload))

    try:
        headers = {"X-User-ID": user_id}

        # 3 requests must pass successfully (Limit is 3)
        for i in range(3):
            resp = session.get(f"{ENVOY_BASE_URL}/metadata", headers=headers, timeout=REQUEST_TIMEOUT)
            assert resp.status_code == 200, f"Doctor request {i+1} unexpectedly blocked"
            
            # Verify response headers (Limit headers)
            resp_headers = get_response_headers(resp)
            assert int(resp_headers.get("ratelimit-remaining")) == 3 - (i + 1)
            
            # Verify that Envoy successfully injected the parsed headers upstream
            upstream_headers = extract_received_headers(resp)
            assert upstream_headers.get("x-user-tier") == "doctor"
            assert "active" in upstream_headers.get("x-user-full-data")

        # 4th request must be blocked (429)
        resp = session.get(f"{ENVOY_BASE_URL}/metadata", headers=headers, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 429

    finally:
        # Cleanup Redis to ensure test isolation
        redis_client.delete(redis_key)


def test_engineer_fallback_limit(session, redis_client):
    """
    Test Case 2: Defined Role with Fallback Limit (Engineer)
    1. Pre-seed Redis with a JSON payload representing an 'engineer'.
    2. Request /metadata with X-User-ID set to 'engineer-user'.
    3. Verify that the echo backend received 'X-User-Tier: engineer'.
    4. Verify that the fallback limit (1 req/min) is enforced (since engineer has no specific rule).
    """
    time.sleep(5) # short grace sleep

    # Wait for a clean minute boundary
    current_sec = time.localtime().tm_sec
    wait_time = (60 - current_sec) % 60
    if wait_time < 5:
        wait_time += 60
    time.sleep(wait_time)

    user_id = "engineer-user"
    redis_key = f"user_meta:{user_id}"
    user_payload = {
        "profile": {
            "tier": "engineer"
        },
        "status": "active"
    }

    # Inject JSON payload into Redis
    redis_client.set(redis_key, json.dumps(user_payload))

    try:
        headers = {"X-User-ID": user_id}

        # 1st request must pass successfully (Limit is 1 for non-doctors)
        resp = session.get(f"{ENVOY_BASE_URL}/metadata", headers=headers, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200
        
        resp_headers = get_response_headers(resp)
        assert int(resp_headers.get("ratelimit-remaining")) == 0
        
        upstream_headers = extract_received_headers(resp)
        assert upstream_headers.get("x-user-tier") == "engineer"

        # 2nd request must be blocked (429)
        resp = session.get(f"{ENVOY_BASE_URL}/metadata", headers=headers, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 429

    finally:
        redis_client.delete(redis_key)


def test_anonymous_missing_header_fallback(session, redis_client):
    """
    Test Case 3: Missing Header Fallback (Anonymous)
    1. Pre-seed Redis with the fallback 'user_meta:anonymous' key.
    2. Request /metadata WITHOUT the 'X-User-ID' header.
    3. The engine must fall back to 'anonymous', pull 'user_meta:anonymous', and extract 'guest'.
    4. Verify that the rate limit is enforced at 1 req/min (fallback limit).
    """
    time.sleep(5)

    current_sec = time.localtime().tm_sec
    wait_time = (60 - current_sec) % 60
    if wait_time < 5:
        wait_time += 60
    time.sleep(wait_time)

    redis_key = "user_meta:anonymous"
    user_payload = {
        "profile": {
            "tier": "guest"
        },
        "status": "restricted"
    }

    # Inject the anonymous payload into Redis
    redis_client.set(redis_key, json.dumps(user_payload))

    try:
        # We send NO headers (X-User-ID is missing)
        resp = session.get(f"{ENVOY_BASE_URL}/metadata", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200
        
        resp_headers = get_response_headers(resp)
        assert int(resp_headers.get("ratelimit-remaining")) == 0
        
        upstream_headers = extract_received_headers(resp)
        assert upstream_headers.get("x-user-tier") == "guest"
        assert "restricted" in upstream_headers.get("x-user-full-data")

        # 2nd request must be blocked (429)
        resp = session.get(f"{ENVOY_BASE_URL}/metadata", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 429

    finally:
        redis_client.delete(redis_key)