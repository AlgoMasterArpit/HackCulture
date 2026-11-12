"""Score every labelled answer in a dataset through the bestbefore/verify model.

    python eval/run.py --gateway http://localhost:8080 --pace 8
    python eval/run.py --dataset holdout
"""
import argparse
import json
import pathlib
import sys
import time
from datetime import datetime, timezone

from gateway import chat

HERE = pathlib.Path(__file__).parent


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--gateway", default="http://localhost:8080")
    parser.add_argument("--pace", type=float, default=8, help="seconds between checked answers, for free-tier rate limits")
    parser.add_argument("--only", help="comma-separated item ids")
    parser.add_argument("--dataset", default="claims", help="claims (development set) or holdout")
    args = parser.parse_args()

    dataset = json.loads((HERE / f"{args.dataset}.json").read_text(encoding="utf-8"))
    items = dataset["items"]
    if args.only:
        wanted = set(args.only.split(","))
        items = [item for item in items if item["id"] in wanted]

    started = time.monotonic()
    runs = []
    for index, item in enumerate(items, 1):
        messages = [
            {"role": "user", "content": item["question"]},
            {"role": "assistant", "content": item["answer"]},
        ]
        try:
            body, seconds = chat(args.gateway, "bestbefore/verify", messages)
            report = body["bestbefore"]
        except Exception as error:
            report, seconds = {"status": "error", "error": str(error), "claims": []}, 0
        runs.append({**item, "report": report, "wall_seconds": round(seconds, 2)})
        print(f"[{index:2}/{len(items)}] {item['id']} {item['label']:9} -> {report['status']:9} {item['answer'][:60]}", flush=True)
        if len(runs) >= 3 and all(r["report"]["status"] in ("unchecked", "error") for r in runs[-3:]):
            print("\nthree checks in a row failed (network or quota); not saving a broken run", flush=True)
            return 2
        if report.get("searches"):
            time.sleep(args.pace)

    out = {
        "ran_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "gateway": args.gateway,
        "labelled_on": dataset["labelled_on"],
        "duration_seconds": round(time.monotonic() - started),
        "runs": runs,
    }
    results = HERE / "results"
    results.mkdir(exist_ok=True)
    out_path = results / f"{args.dataset}.json"
    out_path.write_text(json.dumps(out, indent=2), encoding="utf-8")
    print(f"\nwrote {out_path}")
    return 0 if all(run["report"]["status"] != "error" for run in runs) else 1


if __name__ == "__main__":
    sys.exit(main())
