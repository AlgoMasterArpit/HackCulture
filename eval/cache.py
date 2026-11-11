"""Ask a live model the same time-sensitive questions twice through two gateways:
a plain TTL-only cache (the plugin only observes) and BestBefore. Record what
users actually receive on each ask: a stale answer, a corrected one, or a fresh
one, whether it came from the cache, and how long it took.

    docker compose --profile baseline up -d
    python eval/cache.py
"""
import argparse
import json
import pathlib
import time
from datetime import datetime, timezone

from gateway import chat

HERE = pathlib.Path(__file__).parent

QUESTIONS = [
    "What is the latest stable version of Python? Answer in one sentence.",
    "What is the newest Kubernetes release? Answer in one sentence.",
    "What is the latest version of Go? Answer in one sentence.",
    "Which Node.js version is the current active LTS? Answer in one sentence.",
    "What is the newest iPhone model? Answer in one sentence.",
    "What is the latest major version of PostgreSQL? Answer in one sentence.",
    "What is OpenAI's newest flagship model? Answer in one sentence.",
    "What is the latest version of macOS? Answer in one sentence.",
    "What is the latest LTS release of Java? Answer in one sentence.",
    "What is the newest Ubuntu LTS release? Answer in one sentence.",
]


def ask_twice(gateway, model, question, key):
    messages = [{"role": "user", "content": question}]
    first, first_seconds = chat(gateway, model, messages, cache_key=key)
    time.sleep(3)
    second, second_seconds = chat(gateway, model, messages, cache_key=key)
    pick = lambda body, seconds: {
        "answer": body["choices"][0]["message"]["content"],
        "cache_hit": body.get("extra_fields", {}).get("cache_debug", {}).get("cache_hit", False),
        "seconds": round(seconds, 2),
        "bestbefore": body.get("bestbefore"),
    }
    return pick(first, first_seconds), pick(second, second_seconds)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--bestbefore", default="http://localhost:8080")
    parser.add_argument("--baseline", default="http://localhost:8081")
    parser.add_argument("--model", default="gemini/gemma-4-26b-a4b-it")
    parser.add_argument("--pace", type=float, default=10)
    args = parser.parse_args()

    stamp = int(time.time())
    runs = []
    for index, question in enumerate(QUESTIONS, 1):
        row = {"question": question}
        for name, gateway in (("baseline", args.baseline), ("bestbefore", args.bestbefore)):
            first, second = ask_twice(gateway, args.model, question, key=f"eval-{stamp}-{index}-{name}")
            row[name] = {"first": first, "second": second}
            status = (first["bestbefore"] or {}).get("status")
            print(f"[{index:2}] {name:10} first={status:9} second_hit={second['cache_hit']}  {question[:50]}", flush=True)
            time.sleep(args.pace)
        runs.append(row)

    out = {"ran_at": datetime.now(timezone.utc).isoformat(timespec="seconds"), "model": args.model, "runs": runs}
    results = HERE / "results"
    results.mkdir(exist_ok=True)
    (results / "cache.json").write_text(json.dumps(out, indent=2), encoding="utf-8")
    print(f"\nwrote {results / 'cache.json'}")


if __name__ == "__main__":
    main()
