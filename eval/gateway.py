import json
import time
import urllib.error
import urllib.request


def chat(gateway, model, messages, cache_key=None, retries=4):
    headers = {"Content-Type": "application/json"}
    if cache_key:
        headers["x-bf-cache-key"] = cache_key
    body = json.dumps({"model": model, "messages": messages}).encode()
    for attempt in range(retries + 1):
        request = urllib.request.Request(f"{gateway}/v1/chat/completions", data=body, headers=headers)
        started = time.monotonic()
        try:
            with urllib.request.urlopen(request, timeout=180) as response:
                return json.loads(response.read()), time.monotonic() - started
        except urllib.error.HTTPError as error:
            if error.code not in (429, 500, 502, 503) or attempt == retries:
                raise RuntimeError(f"{error.code}: {error.read()[:300]!r}") from error
        time.sleep(15 * (attempt + 1))
