"""End-to-end check against a real Bifrost gateway with the plugin loaded.

    docker compose -f e2e/compose.yml up -d --build
    python e2e/test_gateway.py
"""
import json
import sys
import time
import urllib.error
import urllib.request

GATEWAY = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080"


def chat(messages, model="mock/gpt-test", cache_key=None):
    headers = {"Content-Type": "application/json"}
    if cache_key:
        headers["x-bf-cache-key"] = cache_key
    request = urllib.request.Request(
        f"{GATEWAY}/v1/chat/completions",
        data=json.dumps({"model": model, "messages": messages}).encode(),
        headers=headers,
    )
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.loads(response.read()), response.headers


def ask(question, cache_key=None):
    return chat([{"role": "user", "content": question}], cache_key=cache_key)


def cache_hit(body):
    return body.get("extra_fields", {}).get("cache_debug", {}).get("cache_hit", False)


def wait_for_gateway():
    for _ in range(60):
        try:
            ask("ping")
            return
        except (urllib.error.URLError, ConnectionError):
            time.sleep(1)
    raise SystemExit("gateway did not come up")


def test_timeless_answer_is_not_checked():
    body, headers = ask("How does binary search work?")
    assert headers["x-bestbefore-status"] == "timeless", headers.items()
    assert body["bestbefore"]["searches"] == 0


def test_stale_answer_is_flagged_and_never_stays_cached():
    key = f"stale-{time.time()}"
    first, headers = ask("How much is Acme Pro?", cache_key=key)
    report = first["bestbefore"]
    assert report["status"] == "stale", report
    assert report["claims"][0]["current_value"] == "$15 per month", report
    assert report["claims"][0]["source_url"] == "https://acme.test/pricing", report
    assert report["cache_evicted"] is True, report
    assert headers["x-bestbefore-cache-evicted"] == "true"

    second, _ = ask("How much is Acme Pro?", cache_key=key)
    assert not cache_hit(second), "a stale answer was served from cache"


def test_fresh_answer_is_cached_and_served_from_ledger():
    key = f"fresh-{time.time()}"
    first, _ = ask("How much is Globex Team?", cache_key=key)
    assert first["bestbefore"]["status"] == "fresh", first["bestbefore"]
    assert first["bestbefore"]["origin"] == "verified"

    time.sleep(1)
    second, _ = ask("How much is Globex Team?", cache_key=key)
    assert cache_hit(second), "fresh answer should be a cache hit"
    assert second["bestbefore"]["origin"] == "ledger", second["bestbefore"]
    assert second["bestbefore"]["status"] == "fresh"


def test_verify_model_scores_a_pasted_answer():
    body, _ = chat(
        [
            {"role": "user", "content": "How much is Acme Pro?"},
            {"role": "assistant", "content": "Acme Pro costs $10 per month."},
        ],
        model="bestbefore/verify",
    )
    assert body["bestbefore"]["status"] == "stale", body
    assert "$15 per month" in body["choices"][0]["message"]["content"]


if __name__ == "__main__":
    wait_for_gateway()
    tests = [value for name, value in sorted(globals().items()) if name.startswith("test_")]
    failed = 0
    for test in tests:
        try:
            test()
            print(f"PASS  {test.__name__}")
        except AssertionError as error:
            failed += 1
            print(f"FAIL  {test.__name__}: {error}")
    print(f"\n{len(tests) - failed} of {len(tests)} passed")
    sys.exit(1 if failed else 0)
