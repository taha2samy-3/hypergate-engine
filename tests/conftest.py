import pytest
import requests
import redis

ENVOY_BASE_URL = "http://localhost:8080"
REQUEST_TIMEOUT = 10

@pytest.fixture(scope="session")
def redis_client():
	client = redis.Redis(host="localhost", port=6379, db=0, decode_responses=True)
	yield client
	for key in client.scan_iter("user_meta:*"):
		client.delete(key)
	client.close()

@pytest.fixture
def session():
	s = requests.Session()
	yield s
	s.close()

@pytest.fixture
def get_response_headers():
	def _get(response):
		return {k.lower(): v for k, v in response.headers.items()}
	return _get

@pytest.fixture
def extract_received_headers():
	def _extract(response):
		try:
			data = response.json()
		except ValueError as exc:
			pytest.fail(f"Failed to parse echo-server response as JSON: {exc}")
		received = data.get("request", {}).get("headers", {}) or data.get("headers", {})
		return {k.lower(): v for k, v in received.items()}
	return _extract
