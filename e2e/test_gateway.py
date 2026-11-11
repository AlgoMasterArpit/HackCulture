"""End-to-end check against a real Bifrost gateway with the plugin loaded.

    docker compose -f e2e/compose.yml up -d --build
    python e2e/test_gateway.py
"""
import json
import sys
import time
import urllib.error
import urllib.request

GATEWAY = "http://localhost:8080"
MOCK = "http://localhost:9000"


def post(url, payload, headers=None):
    request = urllib.request.Request(url, data=json.dumps(payload).encode(),
                                     headers={"Content-Type": "application/json", **(headers or {})})
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.loads(response.read()), response.headers


def ask(question, cache_key=None):
    headers = {"x-bf-cache-key": cache_key} if cache_key else {}
    return post(f"{GATEWAY}/v1/chat/completions",
                {"model": "mock/gpt-test", "messages": [{"role": "user", "content": question}]}, headers)


def model(name, messages=None):
    body, _ = post(f"{GATEWAY}/v1/chat/completions", {"model": name, "messages": messages or []})
    return body


def answer(body):
    return body["choices"][0]["message"]["content"]


def cache_hit(body):
    return body.get("extra_fields", {}).get("cache_debug", {}).get("cache_hit", False)


def set_web(**prices):
    post(f"{MOCK}/admin/web", prices)


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


def test_stale_answer_is_healed_and_the_cache_keeps_the_corrected_version():
    set_web(acme="$15")
    key = f"heal-{time.time()}"
    first, headers = ask("How much is Acme Pro?", cache_key=key)
    report = first["bestbefore"]
    assert report["status"] == "healed", report
    assert report["original_answer"] == "Acme Pro costs $10 per month.", report
    assert answer(first).startswith("Acme Pro costs $15 per month [1]."), answer(first)
    assert "https://acme.test/pricing" in answer(first)
    assert headers["x-bestbefore-healed"] == "true"
    assert not report["cache_evicted"], "a healed answer should stay cached"

    time.sleep(1)
    second, _ = ask("How much is Acme Pro?", cache_key=key)
    assert cache_hit(second), "the corrected answer should be served from Bifrost's cache"
    assert answer(second) == answer(first), "the cache must hold the corrected text, not the stale one"
    assert second["bestbefore"]["origin"] == "ledger", second["bestbefore"]


def test_sweeper_evicts_an_answer_that_went_stale_while_cached():
    set_web(globex="$30")
    key = f"sweep-{time.time()}"
    first, _ = ask("How much is Globex Team?", cache_key=key)
    assert first["bestbefore"]["status"] == "fresh", first["bestbefore"]

    set_web(globex="$45")
    deadline = time.time() + 20
    while time.time() < deadline:
        counts = model("bestbefore/status")["bestbefore_status"]["counts"]
        if counts["evicted_by_sweeper"] >= 1:
            break
        time.sleep(1)
    else:
        raise AssertionError(f"the background sweeper never evicted the stale answer: {counts}")

    second, _ = ask("How much is Globex Team?", cache_key=key)
    assert not cache_hit(second), "the stale entry should be gone before anyone asked again"
    assert second["bestbefore"]["status"] == "healed", second["bestbefore"]
    assert "$45 per month" in answer(second), answer(second)
    set_web(globex="$30")


def test_verify_model_heals_a_pasted_answer():
    set_web(acme="$15")
    body = model("bestbefore/verify", [
        {"role": "user", "content": "How much is Acme Pro?"},
        {"role": "assistant", "content": "Acme Pro costs $10 per month."},
    ])
    assert body["bestbefore"]["status"] == "healed", body
    assert "Corrected answer:\nAcme Pro costs $15 per month" in answer(body)


def test_status_model_lists_cached_answers_with_best_before_dates():
    status = model("bestbefore/status")["bestbefore_status"]
    questions = [entry["question"] for entry in status["entries"]]
    assert "How much is Acme Pro?" in questions, status
    assert all(entry["recheck_after"] for entry in status["entries"]), status


if __name__ == "__main__":
    wait_for_gateway()
    tests = [value for name, value in globals().items() if name.startswith("test_")]
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
