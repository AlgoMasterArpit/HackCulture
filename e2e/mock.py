"""Offline stand-in for a model provider and Tavily, so the gateway and plugin
can be exercised end to end with no API keys.

The "model" answers from frozen training data (Acme Pro at $10), while the
"web" has moved on (Acme Pro at $15). Globex is unchanged in both.
"""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODEL_ANSWERS = {
    "acme": "Acme Pro costs $10 per month.",
    "globex": "Globex Team costs $30 per month.",
}
WEB = {
    "acme": {"url": "https://acme.test/pricing", "content": "Acme Pro is now $15 per month, up from $10."},
    "globex": {"url": "https://globex.test/pricing", "content": "Globex Team costs $30 per month for every seat."},
}
VERDICTS = {
    "acme": {"verdict": "contradicted", "quote": "Acme Pro is now $15 per month", "current_value": "$15 per month"},
    "globex": {"verdict": "supported", "quote": "Globex Team costs $30 per month"},
}


def topic(text):
    return next((name for name in WEB if name in text.lower()), None)


def complete(messages):
    system = next((m["content"] for m in messages if m["role"] == "system"), "")
    user = messages[-1]["content"]
    name = topic(user)
    if system.startswith("You extract"):
        claims = [{"text": MODEL_ANSWERS[name].rstrip("."), "kind": "pricing", "query": f"{name} price"}] if name else []
        return json.dumps({"claims": claims})
    if system.startswith("You check"):
        verdicts = [{"claim": 1, "source": 1, **VERDICTS[name]}] if name else []
        return json.dumps({"verdicts": verdicts})
    return MODEL_ANSWERS.get(name, "A binary search halves the search space on every step.")


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        if self.path.endswith("/chat/completions"):
            payload = {
                "id": "mock-1",
                "object": "chat.completion",
                "created": 0,
                "model": body.get("model", "mock"),
                "choices": [{
                    "index": 0,
                    "finish_reason": "stop",
                    "message": {"role": "assistant", "content": complete(body["messages"])},
                }],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            }
        elif self.path.endswith("/search"):
            name = topic(body.get("query", ""))
            payload = {"results": [{**WEB[name], "title": name, "score": 0.9}] if name else []}
        else:
            self.send_error(404)
            return
        raw = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 9000), Handler).serve_forever()
