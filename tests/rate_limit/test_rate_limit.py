import pytest
import time
import requests
import subprocess

BASE_URL = "http://localhost:8080"
REQUEST_TIMEOUT = 10

def stop_redis():
    subprocess.run(["docker", "compose", "-f", "tests/docker-compose.yaml", "--project-directory", "tests", "stop", "redis"], check=True)

def start_redis():
    subprocess.run(["docker", "compose", "-f", "tests/docker-compose.yaml", "--project-directory", "tests", "start", "redis"], check=True)
    time.sleep(2)

def test_fixed_window(session, get_response_headers):
    time.sleep(2)

    for i in range(5):
        resp = session.get(f"{BASE_URL}/baseline", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200, f"Request {i+1} failed"
        headers = get_response_headers(resp)
        remaining = int(headers.get("ratelimit-remaining"))
        assert remaining == 5 - (i + 1), f"Expected remaining {5 - (i+1)}, got {remaining}"

    resp = session.get(f"{BASE_URL}/baseline", timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429

    time.sleep(10)

    resp = session.get(f"{BASE_URL}/baseline", timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200
    headers = get_response_headers(resp)
    assert int(headers.get("ratelimit-remaining")) == 4

def test_token_bucket_burst_and_refill(session, get_response_headers):
    time.sleep(10)

    for i in range(10):
        resp = session.get(f"{BASE_URL}/token", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200
        headers = get_response_headers(resp)
        remaining = int(headers.get("ratelimit-remaining"))
        assert remaining <= 10 - i

    resp = session.get(f"{BASE_URL}/token", timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429

    time.sleep(2)

    resp1 = session.get(f"{BASE_URL}/token", timeout=REQUEST_TIMEOUT)
    assert resp1.status_code == 200

    resp2 = session.get(f"{BASE_URL}/token", timeout=REQUEST_TIMEOUT)
    assert resp2.status_code == 200

    resp3 = session.get(f"{BASE_URL}/token", timeout=REQUEST_TIMEOUT)
    assert resp3.status_code == 429

def test_dynamic_costing(session, get_response_headers):
    time.sleep(10)

    resp = session.get(f"{BASE_URL}/token", timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200
    initial_remaining = int(get_response_headers(resp).get("ratelimit-remaining"))

    resp = session.get(f"{BASE_URL}/token", headers={"X-Cost": "3"}, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200
    headers = get_response_headers(resp)
    new_remaining = int(headers.get("ratelimit-remaining"))

    assert new_remaining <= initial_remaining - 3

def test_redis_failure_fail_open(session):
    stop_redis()
    try:
        resp = session.get(f"{BASE_URL}/fail_open", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200
    finally:
        start_redis()

def test_redis_failure_fail_closed(session):
    stop_redis()
    try:
        resp = session.get(f"{BASE_URL}/fail_closed", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 500
    finally:
        start_redis()

def test_empty_client_ip_fallback(session, get_response_headers):
    time.sleep(10)

    for i in range(5):
        resp = session.get(f"{BASE_URL}/baseline", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200

    resp = session.get(f"{BASE_URL}/baseline", timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429

    resp = session.get(f"{BASE_URL}/baseline", headers={"X-Forwarded-For": "12.34.56.78"}, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200

def test_burst_traffic_token_vs_leaky(session):
    time.sleep(10)

    for i in range(20):
        resp = session.get(f"{BASE_URL}/token", timeout=REQUEST_TIMEOUT)
        if i < 10:
            assert resp.status_code == 200
        else:
            assert resp.status_code == 429
    
    for i in range(20):
        resp = session.get(f"{BASE_URL}/leaky", timeout=REQUEST_TIMEOUT)
        if i < 10:
            assert resp.status_code == 200
        else:
            assert resp.status_code == 429

def test_l1_cache_fairness_dynamic_costing(session):
    time.sleep(10)

    resp_expensive = session.get(f"{BASE_URL}/token", headers={"X-Cost": "25"}, timeout=REQUEST_TIMEOUT)
    assert resp_expensive.status_code == 429

    resp_cheap = session.get(f"{BASE_URL}/token", headers={"X-Cost": "1"}, timeout=REQUEST_TIMEOUT)
    assert resp_cheap.status_code == 200

def test_window_boundary_fixed_vs_sliding(session):
    current_sec = time.localtime().tm_sec
    wait_time = (57 - current_sec) % 60
    if wait_time < 2:
        wait_time += 60
    time.sleep(wait_time)

    for i in range(19):
        resp = session.get(f"{BASE_URL}/fixed", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200

    time.sleep(4)

    for i in range(19):
        resp = session.get(f"{BASE_URL}/fixed", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200

    current_sec = time.localtime().tm_sec
    wait_time = (57 - current_sec) % 60
    if wait_time < 2:
        wait_time += 60
    time.sleep(wait_time)

    for i in range(19):
        resp = session.get(f"{BASE_URL}/sliding", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200

    time.sleep(4)

    blocked = False
    for i in range(19):
        resp = session.get(f"{BASE_URL}/sliding", timeout=REQUEST_TIMEOUT)
        if resp.status_code == 429:
            blocked = True
            break
            
    assert blocked

def test_chained_filter_integration(session, get_response_headers, extract_received_headers):
    time.sleep(10)

    resp = session.get(f"{BASE_URL}/token", timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200
    
    headers = get_response_headers(resp)
    assert "x-request-id" in headers
    req_id = headers["x-request-id"]
    assert len(req_id) > 0

    assert headers.get("x-test-downstream") == "modified"

    upstream_headers = extract_received_headers(resp)
    assert upstream_headers.get("x-test-upstream") == "processed"

    for _ in range(15):
        resp_block = session.get(f"{BASE_URL}/token", timeout=REQUEST_TIMEOUT)
        if resp_block.status_code == 429:
            block_headers = get_response_headers(resp_block)
            assert "x-request-id" in block_headers
            assert len(block_headers["x-request-id"]) > 0
            assert block_headers.get("x-test-downstream") == "modified"
            break


def test_sliding_window_log_threshold_enforcement(session, get_response_headers):
    time.sleep(10)

    for i in range(5):
        resp = session.get(f"{BASE_URL}/sliding_log",headers={"X-Forwarded-For": "7.7.7.7"}, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200

    resp = session.get(f"{BASE_URL}/sliding_log",headers={"X-Forwarded-For": "7.7.7.7"}, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429

    time.sleep(10)

    resp = session.get(f"{BASE_URL}/sliding_log",headers={"X-Forwarded-For": "7.7.7.7"}, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200


def test_leaky_bucket_steady_state_leaking(session):
    time.sleep(10)

    for i in range(10):
        resp = session.get(f"{BASE_URL}/leaky",headers={"X-Forwarded-For": "8.8.8.8"}, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200

    resp = session.get(f"{BASE_URL}/leaky",headers={"X-Forwarded-For": "8.8.8.8"}, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429

    time.sleep(3)

    for i in range(3):
        resp = session.get(f"{BASE_URL}/leaky",headers={"X-Forwarded-For": "8.8.8.8"}, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200

    resp = session.get(f"{BASE_URL}/leaky",headers={"X-Forwarded-For": "8.8.8.8"}, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429


def test_sliding_window_counter_degradation(session, get_response_headers):
    time.sleep(10)

    for i in range(5):
        resp = session.get(f"{BASE_URL}/sliding",headers={"X-Forwarded-For": "9.9.9.9"}, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200

    time.sleep(30)

    for i in range(3):
        resp = session.get(f"{BASE_URL}/sliding",headers={"X-Forwarded-For": "9.9.9.9"}, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200

    headers = get_response_headers(resp)
    remaining = int(headers.get("ratelimit-remaining"))
    limit = int(headers.get("ratelimit-limit"))
    
    # Verify remaining is between (limit - 8) and (limit - 3)
    # This proves the 5 older requests were partially degraded but not fully forgotten
    assert limit - 8 < remaining < limit - 3



def test_domino_effect_chaining(session, get_response_headers):
    """
    Integration test verifying the 'Domino Effect' sequential filter execution.
    Ensures that context mutations made by upstream filters (Header Modifier)
    are successfully propagated and evaluated by downstream filters (Rate Limiter)
    within the exact same request lifecycle.
    """
    cheat_headers = {
        "X-Cost": "100"
    }
    # Execute HTTP GET request without providing any cost headers in the client request.
    resp = session.get(f"{BASE_URL}/domino",headers=cheat_headers, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200

    # Parse the injected downstream rate limit headers from the response.
    headers = get_response_headers(resp)
    remaining = int(headers.get("ratelimit-remaining"))
    limit = int(headers.get("ratelimit-limit"))

    # Assert that the remaining rate limit decreased by exactly 10 units (the cost injected
    # by the header_modifier filter) instead of the default 1 unit decrement.
    assert remaining == limit - 10

def test_rate_limit_shadow_mode(session, get_response_headers):
    """
    Integration test for 'Shadow Mode' in rate limiting.
    Verifies that requests exceeding the limit are still allowed (200 OK)
    but rate limit response headers correctly reflect '0' remaining.
    """
    path = "/shadow"
    initial_limit = 5 # As configured in config.yaml

    # 1. Consume the allowed limit
    for i in range(initial_limit):
        resp = session.get(f"{BASE_URL}{path}", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200, f"Request {i+1} unexpectedly blocked in shadow mode."
        headers = get_response_headers(resp)
        remaining = int(headers.get("ratelimit-remaining"))
        assert remaining == initial_limit - (i + 1), f"Remaining count incorrect for request {i+1}."

    # 2. Send requests *beyond* the limit while in shadow mode
    # These requests should still return 200 OK, but with 0 remaining.
    for i in range(initial_limit, initial_limit + 3): # Send 3 extra requests
        resp = session.get(f"{BASE_URL}{path}", timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200, f"Request {i+1} unexpectedly blocked in shadow mode (expected 200 OK)."
        
        headers = get_response_headers(resp)
        remaining = int(headers.get("ratelimit-remaining"))
        limit = int(headers.get("ratelimit-limit"))

        # In shadow mode, once limit is hit, remaining should always be 0.
        assert remaining == 0, f"Remaining count in shadow mode not zero for request {i+1}."
        assert limit == initial_limit, f"Limit header incorrect in shadow mode for request {i+1}."




def test_malicious_cost_defense(session, get_response_headers):
    """
    Integration test verifying defense against invalid or out-of-bound dynamic cost headers.
    Validates that:
    1. An extremely large cost is clamped to 'max_allowed_cost'.
    2. A non-numeric/invalid cost falls back gracefully to 'default_fallback_cost' without crashing.
    """
    path = "/malicious_cost"
    initial_tokens = 10

    # Test Case 1: Out-of-bound Cost (Hacker sends X-Cost: 999999)
    # The system must clamp this to max_allowed_cost (5).
    resp = session.get(f"{BASE_URL}{path}", headers={"X-Cost": "999999"}, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200

    headers = get_response_headers(resp)
    remaining = int(headers.get("ratelimit-remaining"))
    
    # 10 (initial) - 5 (clamped max) = 5 remaining (instead of being completely depleted).
    assert remaining == initial_tokens - 5

    # Test Case 2: Malformed/Invalid Cost (Hacker sends X-Cost: "invalid_string")
    # The system must fallback to default_fallback_cost (1) and proceed safely.
    resp = session.get(f"{BASE_URL}{path}", headers={"X-Cost": "invalid_string"}, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200

    headers = get_response_headers(resp)
    remaining = int(headers.get("ratelimit-remaining"))

    # 5 (previous remaining) - 1 (fallback cost) = 4 remaining.
    assert remaining == 4




def test_composite_role_limits(session, get_response_headers):
    """
    Integration test verifying composite-key rate limiting (Role + Billing Cycle).
    Validates specific limits for 'manager' (30/min), 'client' (10/min),
    and the 'OTHER' fallback rule (5/min), as well as post-window reset recovery.
    """
    # Defensive check: Wait for a clean minute boundary to prevent transient window splits
    current_sec = time.localtime().tm_sec
    wait_time = (60 - current_sec) % 60
    if wait_time < 5:
        wait_time += 60
    time.sleep(wait_time)

    # 1. Verify Client limit (10 requests per minute)
    client_headers = {
        "X-User-Role": "client",
        "X-Billing-Cycle": "cycle-1"
    }
    for i in range(10):
        resp = session.get(f"{BASE_URL}/composite", headers=client_headers, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200, f"Client request {i+1} unexpectedly blocked"
        headers = get_response_headers(resp)
        remaining = int(headers.get("ratelimit-remaining"))
        assert remaining == 10 - (i + 1), f"Expected remaining {10 - (i+1)}, got {remaining}"

    # 11th request must be blocked
    resp = session.get(f"{BASE_URL}/composite", headers=client_headers, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429, "Client should be blocked on the 11th request"

    # 2. Verify Manager limit (30 requests per minute)
    manager_headers = {
        "X-User-Role": "manager",
        "X-Billing-Cycle": "cycle-2"
    }
    for i in range(30):
        resp = session.get(f"{BASE_URL}/composite", headers=manager_headers, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200, f"Manager request {i+1} unexpectedly blocked"
        headers = get_response_headers(resp)
        remaining = int(headers.get("ratelimit-remaining"))
        assert remaining == 30 - (i + 1), f"Expected remaining {30 - (i+1)}, got {remaining}"

    # 31st request must be blocked
    resp = session.get(f"{BASE_URL}/composite", headers=manager_headers, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429, "Manager should be blocked on the 31st request"

    # 3. Verify Fallback / OTHER limit (5 requests per minute for undefined roles like 'guest')
    fallback_headers = {
        "X-User-Role": "guest",
        "X-Billing-Cycle": "cycle-3"
    }
    for i in range(5):
        resp = session.get(f"{BASE_URL}/composite", headers=fallback_headers, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200, f"Fallback request {i+1} unexpectedly blocked"
        headers = get_response_headers(resp)
        remaining = int(headers.get("ratelimit-remaining"))
        assert remaining == 5 - (i + 1), f"Expected remaining {5 - (i+1)}, got {remaining}"

    # 6th request must be blocked
    resp = session.get(f"{BASE_URL}/composite", headers=fallback_headers, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429, "Fallback should be blocked on the 6th request"

    # 4. Wait for the window boundary to slide and reset
    time.sleep(60)

    # 5. Verify reset (client can successfully request again with renewed limit)
    resp = session.get(f"{BASE_URL}/composite", headers=client_headers, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200, "Request failed after rate limit window reset"
    headers = get_response_headers(resp)
    assert int(headers.get("ratelimit-remaining")) == 9


def test_composite_sliding_limits(session, get_response_headers):
    """
    Integration test verifying composite-key rate limiting (Role + Billing Cycle)
    using the stateful Sliding Window Counter algorithm.
    Validates limits for 'manager' (20/min), 'client' (5/min), and 'OTHER' (2/min).
    """
    current_sec = time.localtime().tm_sec
    wait_time = (60 - current_sec) % 60
    if wait_time < 5:
        wait_time += 60
    time.sleep(wait_time)

    client_headers = {
        "X-User-Role": "client",
        "X-Billing-Cycle": "cycle-99"
    }
    for i in range(5):
        resp = session.get(f"{BASE_URL}/composite_sliding", headers=client_headers, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200, f"Client sliding request {i+1} unexpectedly blocked"
        headers = get_response_headers(resp)
        remaining = int(headers.get("ratelimit-remaining"))
        assert remaining == 5 - (i + 1), f"Expected remaining {5 - (i+1)}, got {remaining}"

    resp = session.get(f"{BASE_URL}/composite_sliding", headers=client_headers, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429, "Client should be blocked on the 6th sliding request"

    manager_headers = {
        "X-User-Role": "manager",
        "X-Billing-Cycle": "cycle-88"
    }
    for i in range(20):
        resp = session.get(f"{BASE_URL}/composite_sliding", headers=manager_headers, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200, f"Manager sliding request {i+1} unexpectedly blocked"
        headers = get_response_headers(resp)
        remaining = int(headers.get("ratelimit-remaining"))
        assert remaining == 20 - (i + 1), f"Expected remaining {20 - (i+1)}, got {remaining}"

    resp = session.get(f"{BASE_URL}/composite_sliding", headers=manager_headers, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429, "Manager should be blocked on the 21st sliding request"

    fallback_headers = {
        "X-User-Role": "operator",
        "X-Billing-Cycle": "cycle-77"
    }
    for i in range(2):
        resp = session.get(f"{BASE_URL}/composite_sliding", headers=fallback_headers, timeout=REQUEST_TIMEOUT)
        assert resp.status_code == 200, f"Fallback sliding request {i+1} unexpectedly blocked"
        headers = get_response_headers(resp)
        remaining = int(headers.get("ratelimit-remaining"))
        assert remaining == 2 - (i + 1), f"Expected remaining {2 - (i+1)}, got {remaining}"

    resp = session.get(f"{BASE_URL}/composite_sliding", headers=fallback_headers, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 429, "Fallback should be blocked on the 3rd sliding request"

    # FIXED: Sleep 75s instead of 60s to allow the sliding weight to decay enough
    # for the stateful sliding window counter to allow a new request.
    time.sleep(75)

    resp = session.get(f"{BASE_URL}/composite_sliding", headers=client_headers, timeout=REQUEST_TIMEOUT)
    assert resp.status_code == 200, "Sliding request failed after rate limit window reset"
