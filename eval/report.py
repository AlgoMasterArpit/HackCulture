"""Turn eval/results/*.json into eval/RESULTS.md and the charts in docs/img/.

    python eval/report.py
"""
import json
import pathlib
import statistics
from collections import Counter

HERE = pathlib.Path(__file__).parent
IMG = HERE.parent / "docs" / "img"

STATUSES = ["stale", "healed", "uncertain", "unchecked", "fresh", "timeless", "error"]
COLORS = {
    "stale": "#dc2626",
    "healed": "#0284c7",
    "uncertain": "#d97706",
    "unchecked": "#a8a29e",
    "fresh": "#16a34a",
    "timeless": "#64748b",
    "error": "#111827",
}
INK, MUTED, GRID = "#111827", "#6b7280", "#e5e7eb"


def load(name):
    path = HERE / "results" / name
    return json.loads(path.read_text(encoding="utf-8")) if path.exists() else None


def pct(part, whole):
    return 100 * part / whole if whole else 0


def flagged_without_grounding(report):
    return any(claim.get("model_verdict") == "contradicted" for claim in report.get("claims", []))


def claim_metrics(runs):
    by_label = {label: [r for r in runs if r["label"] == label] for label in ("stale", "fresh", "timeless")}
    stale, fresh, timeless = by_label["stale"], by_label["fresh"], by_label["timeless"]
    not_stale = fresh + timeless
    flagged = lambda r: r["report"]["status"] in ("stale", "healed")

    checked = [r for r in runs if r["report"].get("searches")]
    latencies = sorted(r["report"].get("latency_ms", 0) for r in checked)
    gated = [r for r in timeless if r["report"].get("judge_tokens", 0) == 0]
    rejected = [
        (r["id"], c) for r in runs for c in r["report"].get("claims", [])
        if c.get("model_verdict") in ("supported", "contradicted") and not c.get("grounded")
    ]

    with_grounding = {
        "recall": pct(sum(map(flagged, stale)), len(stale)),
        "false_alarm": pct(sum(map(flagged, not_stale)), len(not_stale)),
    }
    without_grounding = {
        "recall": pct(sum(flagged_without_grounding(r["report"]) for r in stale), len(stale)),
        "false_alarm": pct(sum(flagged_without_grounding(r["report"]) for r in not_stale), len(not_stale)),
    }
    true_flags = sum(map(flagged, stale))
    all_flags = sum(map(flagged, runs))
    healed = sum(r["report"]["status"] == "healed" for r in runs)
    return {
        "counts": {label: Counter(r["report"]["status"] for r in rows) for label, rows in by_label.items()},
        "sizes": {label: len(rows) for label, rows in by_label.items()},
        "with": with_grounding,
        "without": without_grounding,
        "precision": pct(true_flags, all_flags),
        "heal_rate": pct(healed, all_flags),
        "gate_skip": pct(len(gated), len(timeless)),
        "checked": len(checked),
        "tokens_per_checked": statistics.mean(r["report"]["judge_tokens"] for r in checked) if checked else 0,
        "searches_per_checked": statistics.mean(r["report"]["searches"] for r in checked) if checked else 0,
        "latency_p50": latencies[len(latencies) // 2] if latencies else 0,
        "latency_p95": latencies[int(len(latencies) * 0.95) - 1] if latencies else 0,
        "rejected": rejected,
    }


def cache_metrics(data):
    status = lambda ask: (ask["bestbefore"] or {}).get("status")
    out = {}
    for name in ("baseline", "bestbefore"):
        rows = [run[name] for run in data["runs"]]
        asks = [row[k] for row in rows for k in ("first", "second")]
        repeat = sorted(row["second"].get("seconds", 0) for row in rows)
        out[name] = {
            "questions": len(rows),
            "stale_first": sum(status(row["first"]) in ("stale", "healed") for row in rows),
            "stale_delivered": sum(status(ask) == "stale" for ask in asks),
            "unproven_delivered": sum(status(ask) in ("uncertain", "unchecked") for ask in asks),
            "asks": len(asks),
            "hits": sum(row["second"]["cache_hit"] for row in rows),
            "served_stale": sum(row["second"]["cache_hit"] and status(row["second"]) == "stale" for row in rows),
            "served_corrected": sum(row["second"]["cache_hit"] and status(row["second"]) == "healed" for row in rows),
            "repeat_seconds_p50": repeat[len(repeat) // 2] if repeat else 0,
        }
    return out


def svg(width, height, body, title):
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}" '
        f'font-family="system-ui,-apple-system,Segoe UI,Roboto,sans-serif" role="img" aria-label="{title}">'
        f'<rect width="{width}" height="{height}" rx="14" fill="#ffffff"/>'
        f'<text x="24" y="36" font-size="17" font-weight="700" fill="{INK}">{title}</text>{body}</svg>'
    )


def verdict_chart(metrics):
    width, left, bar_w, row_h, top = 760, 150, 560, 44, 64
    parts = []
    for i, label in enumerate(("stale", "fresh", "timeless")):
        y = top + i * row_h
        total = metrics["sizes"][label] or 1
        parts.append(f'<text x="{left - 12}" y="{y + 20}" font-size="14" text-anchor="end" fill="{INK}">{label} answers</text>')
        x = left
        for status in STATUSES:
            n = metrics["counts"][label].get(status, 0)
            if not n:
                continue
            w = bar_w * n / total
            parts.append(f'<rect x="{x:.1f}" y="{y}" width="{w:.1f}" height="28" fill="{COLORS[status]}"/>')
            if w > 26:
                parts.append(f'<text x="{x + w / 2:.1f}" y="{y + 19}" font-size="13" font-weight="700" text-anchor="middle" fill="#fff">{n}</text>')
            x += w
    legend_y = top + 3 * row_h + 14
    x = left
    for status in STATUSES[:-1]:
        parts.append(f'<rect x="{x}" y="{legend_y - 10}" width="12" height="12" rx="2" fill="{COLORS[status]}"/>')
        parts.append(f'<text x="{x + 18}" y="{legend_y}" font-size="13" fill="{MUTED}">{status}</text>')
        x += 92
    return svg(width, legend_y + 26, "".join(parts), "What BestBefore said about 60 hand-labelled answers")


def bar_pairs(title, groups, series, unit="%", max_value=100):
    width, left, top, group_h, bar_h = 760, 230, 60, 74, 24
    scale = 460 / max_value if max_value else 0
    parts = []
    for gi, (label, values) in enumerate(groups):
        y = top + gi * group_h
        parts.append(f'<text x="{left - 12}" y="{y + 30}" font-size="14" text-anchor="end" fill="{INK}">{label}</text>')
        for si, (name, color) in enumerate(series):
            value = values[si]
            by = y + si * (bar_h + 4)
            parts.append(f'<rect x="{left}" y="{by}" width="{max(value * scale, 2):.1f}" height="{bar_h}" rx="3" fill="{color}"/>')
            text = f"{value:.0f}{unit}" if unit == "%" else f"{value:g}{unit}"
            parts.append(f'<text x="{left + max(value * scale, 2) + 8:.1f}" y="{by + 17}" font-size="13" font-weight="700" fill="{INK}">{text}</text>')
    legend_y = top + len(groups) * group_h + 8
    x = left
    for name, color in series:
        parts.append(f'<rect x="{x}" y="{legend_y - 10}" width="12" height="12" rx="2" fill="{color}"/>')
        parts.append(f'<text x="{x + 18}" y="{legend_y}" font-size="13" fill="{MUTED}">{name}</text>')
        x += 24 + 8 * len(name)
    return svg(width, legend_y + 24, "".join(parts), title)


def write_charts(claims, cache):
    IMG.mkdir(parents=True, exist_ok=True)
    if claims:
        (IMG / "eval-verdicts.svg").write_text(verdict_chart(claims), encoding="utf-8")
        (IMG / "eval-grounding.svg").write_text(bar_pairs(
            "What the quote check costs on clean answers (its job is attacks, see docs/REVIEW.md)",
            [
                ("Stale answers caught", [claims["without"]["recall"], claims["with"]["recall"]]),
                ("Good answers wrongly flagged", [claims["without"]["false_alarm"], claims["with"]["false_alarm"]]),
            ],
            [("LLM verdict alone", "#94a3b8"), ("BestBefore (verdict + verbatim quote)", "#4f46e5")],
        ), encoding="utf-8")
    if cache:
        b, o = cache["baseline"], cache["bestbefore"]
        top = max(b["questions"], 1)
        top = max(b["asks"], 1)
        (IMG / "eval-cache.svg").write_text(bar_pairs(
            f"What users received: {b['questions']} live questions, each asked twice ({b['asks']} answers)",
            [
                ("Stale answers delivered", [b["stale_delivered"], o["stale_delivered"]]),
                ("Corrected answers from cache", [b["served_corrected"], o["served_corrected"]]),
            ],
            [("Bifrost cache, TTL only", "#94a3b8"), ("Bifrost cache + BestBefore", "#4f46e5")],
            unit="", max_value=top,
        ), encoding="utf-8")


def results_markdown(claims_data, claims, cache_data, cache):
    lines = ["# Evaluation results", "", "Generated by `python eval/report.py` from the JSON in `eval/results/`. Do not edit by hand.", ""]
    if claims:
        lines += [
            f"## Labelled answers ({sum(claims['sizes'].values())} answers, run {claims_data['ran_at']}, "
            f"took {claims_data.get('duration_seconds', 0) / 60:.0f} minutes)",
            "",
            "| Metric | LLM verdict alone | BestBefore |",
            "|---|---|---|",
            f"| Stale answers caught (recall) | {claims['without']['recall']:.0f}% | **{claims['with']['recall']:.0f}%** |",
            f"| Good answers wrongly flagged stale | {claims['without']['false_alarm']:.0f}% | **{claims['with']['false_alarm']:.0f}%** |",
            "",
            "| Cost and speed | Value |",
            "|---|---|",
            f"| Precision of a stale flag | {claims['precision']:.0f}% |",
            f"| Timeless answers skipped at zero cost (no model call, no search) | {claims['gate_skip']:.0f}% |",
            f"| Judge tokens per checked answer | {claims['tokens_per_checked']:.0f} |",
            f"| Tavily searches per checked answer | {claims['searches_per_checked']:.1f} |",
            f"| Verification latency p50 / p95 | {claims['latency_p50'] / 1000:.1f}s / {claims['latency_p95'] / 1000:.1f}s |",
            f"| Judge verdicts discarded because their quote did not prove them | {len(claims['rejected'])} |",
            "",
            "### Every answer",
            "",
            "| id | label | result | score | answer | evidence |",
            "|---|---|---|---|---|---|",
        ]
        for run in claims_data["runs"]:
            report = run["report"]
            first = (report.get("claims") or [{}])[0]
            evidence = first.get("current_value") or ""
            if first.get("source_url"):
                evidence = f"{evidence} ([source]({first['source_url']}))".strip()
            elif first.get("rejected_because"):
                evidence = f"judge said {first['model_verdict']}, rejected: {first['rejected_because']}"
            elif report.get("error"):
                evidence = report["error"]
            mark = "" if expected(run["label"], report["status"]) else " ⚠"
            score = report.get("freshness_score", 0)
            lines.append(f"| {run['id']} | {run['label']} | {report['status']}{mark} | {score:.2f} | {run['answer']} | {evidence} |")
        lines += ["", "⚠ marks a result that does not match the label: a stale answer not flagged, or a good answer flagged stale.", ""]
    if cache:
        lines += [
            f"## Semantic cache ({cache_data['model']}, run {cache_data['ran_at']})",
            "",
            "| | Bifrost cache, TTL only | Bifrost cache + BestBefore |",
            "|---|---|---|",
        ] + cache_rows(cache) + [""]
    (HERE / "RESULTS.md").write_text("\n".join(lines), encoding="utf-8")


def cache_rows(cache):
    b, o = cache["baseline"], cache["bestbefore"]
    return [
        f"| Live: {b['questions']} questions, each asked twice | Bifrost cache, TTL only | Bifrost cache + BestBefore |",
        "|---|---|---|",
        f"| The model's first answer was out of date | {b['stale_first']} of {b['questions']} | {o['stale_first']} of {o['questions']} |",
        f"| **Stale answers delivered to users** | **{b['stale_delivered']} of {b['asks']}** | **{o['stale_delivered']} of {o['asks']}** |",
        f"| Answers that could not be proven either way, delivered flagged | {b['unproven_delivered']} | {o['unproven_delivered']} |",
        f"| Repeat asks served from cache | {b['hits']} | {o['hits']} |",
        f"| ...with the corrected answer | {b['served_corrected']} | {o['served_corrected']} |",
        f"| Repeat-ask time, median | not comparable¹ | {o['repeat_seconds_p50']:.2f}s |",
        "",
        "¹ The baseline gateway runs the plugin as an observer so stale answers can be counted, and the observer re-checks "
        "every cache hit, which makes its repeat asks slow. A real TTL-only cache answers a hit as fast as BestBefore does.",
    ]


def replace_block(text, name, body):
    start, end = f"<!-- {name}:start -->", f"<!-- {name}:end -->"
    head, rest = text.split(start, 1)
    _, tail = rest.split(end, 1)
    return head + start + body + end + tail


def update_readme(claims, cache, run1=None, repeat=None):
    path = HERE.parent / "README.md"
    text = path.read_text(encoding="utf-8")
    proof, rows = [], []
    if claims:
        total = sum(claims["sizes"].values())
        cost = claims["without"]["recall"] - claims["with"]["recall"]
        saved = claims["without"]["false_alarm"] - claims["with"]["false_alarm"]
        if saved > 0:
            grounding = f"The quote check cut false alarms by {saved:.0f} points at a cost of {cost:.0f} points of recall."
        else:
            grounding = (
                f"On this clean set the quote check cost {cost:.0f} points of recall and prevented no false alarm, because "
                "nothing here tries to fool the judge; it exists for poisoned pages and invented quotes, which the "
                "[attack tests](docs/REVIEW.md) show the judge alone would act on."
            )
        proof.append(
            f"On {total} hand-labelled answers, BestBefore caught {claims['with']['recall']:.0f}% of the stale ones, "
            f"healed {claims['heal_rate']:.0f}% of those it caught, and wrongly flagged {claims['with']['false_alarm']:.0f}% "
            f"of the good ones. {grounding} "
            f"{claims['gate_skip']:.0f}% of timeless answers cost nothing at all."
        )
        rows += [
            "| Labelled answers | LLM verdict alone | BestBefore |",
            "|---|---|---|",
            f"| Stale answers caught | {claims['without']['recall']:.0f}% | **{claims['with']['recall']:.0f}%** |",
            f"| Good answers wrongly flagged stale | {claims['without']['false_alarm']:.0f}% | **{claims['with']['false_alarm']:.0f}%** |",
            f"| A stale flag is right | | {claims['precision']:.0f}% |",
            f"| Stale answers rewritten with the verified values | | {claims['heal_rate']:.0f}% |",
            f"| Timeless answers checked for free (no model, no search) | | {claims['gate_skip']:.0f}% |",
            f"| Judge tokens per checked answer | | {claims['tokens_per_checked']:.0f} |",
            f"| Tavily searches per checked answer | | {claims['searches_per_checked']:.1f} |",
            f"| Added latency, p50 / p95 | | {claims['latency_p50'] / 1000:.1f}s / {claims['latency_p95'] / 1000:.1f}s |",
            f"| Judge verdicts thrown out because the quote did not prove them | | {len(claims['rejected'])} |",
            "",
        ]
        if run1:
            repeat_note = (
                f"; a repeat with the same rule gave {repeat['with']['recall']:.0f}% and {repeat['with']['false_alarm']:.0f}%"
                if repeat else ""
            )
            rows += [
                "**Earlier runs, also published.** Our first run used a stricter quote rule, which threw out real contradictions "
                "worded differently from the source (\"JDK 25\" for \"Java 25\", a three-word \"Latest version: 19.3\"). "
                f"It caught {run1['with']['recall']:.0f}% of stale answers and wrongly flagged {run1['with']['false_alarm']:.0f}% "
                f"of good ones{repeat_note}. We changed the rule after reading those misses, then re-ran the same 60 answers; "
                "the table above is that run. Because the rule was tuned after seeing this data, treat these as development "
                "numbers, not a held-out test. Every run is in [eval/results/](eval/results/), and every rejected verdict "
                "carries its reason.",
                "",
            ]
    if cache:
        b, o = cache["baseline"], cache["bestbefore"]
        proof.insert(0,
            f"Live, {b['questions']} time-sensitive questions asked twice through each gateway: a TTL-only cache delivered "
            f"{b['stale_delivered']} stale answers out of {b['asks']}; with BestBefore, {o['stale_delivered']}, and "
            f"{o['served_corrected']} repeat asks were served the corrected answer straight from the cache "
            f"(median {o['repeat_seconds_p50']:.2f}s). {o['unproven_delivered']} answers could not be proven either way "
            "and were delivered flagged."
        )
        rows = cache_rows(cache) + [""] + rows
    text = replace_block(text, "proof", " ".join(proof) or "Run eval/report.py.")
    text = replace_block(text, "results", "\n" + "\n".join(rows) + "\n")
    path.write_text(text, encoding="utf-8")


def expected(label, status):
    caught = status in ("stale", "healed")
    return caught if label == "stale" else not caught


def main():
    claims_data, cache_data, run1_data = load("claims.json"), load("cache.json"), load("claims-run1.json")
    claims = claim_metrics(claims_data["runs"]) if claims_data else None
    run1 = claim_metrics(run1_data["runs"]) if run1_data else None
    repeat_data = load("claims-run1-repeat.json")
    repeat = claim_metrics(repeat_data["runs"]) if repeat_data else None
    cache = cache_metrics(cache_data) if cache_data else None
    write_charts(claims, cache)
    results_markdown(claims_data, claims, cache_data, cache)
    update_readme(claims, cache, run1, repeat)
    keep = lambda m: {k: v for k, v in (m or {}).items() if k not in ("rejected", "counts")}
    summary = {"claims": keep(claims), "claims_run1": keep(run1), "claims_run1_repeat": keep(repeat), "cache": cache}
    (HERE / "results" / "summary.json").write_text(json.dumps(summary, indent=2, default=str), encoding="utf-8")
    print(json.dumps(summary, indent=2, default=str))


if __name__ == "__main__":
    main()
