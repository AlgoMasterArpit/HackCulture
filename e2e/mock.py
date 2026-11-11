"""Offline stand-in for a model provider and Tavily, so the gateway and plugin
can be exercised end to end with no API keys.

The "model" answers from frozen training data (Acme Pro at $10, Globex Team
at $30). The "web" starts with Acme Pro at $15 and Globex Team at $30, and a
test can change it with POST /admin/web to make a cached answer go stale.
"""
import json
import re
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PRODUCTS = {"acme": "Acme Pro", "globex": "Globex Team"}
MODEL_PRICES = {"acme": "$10", "globex": "$30"}
web_prices = {"acme": "$15", "globex": "$30"}
DOLLARS = re.compile(r"\$\d+")


def topic(text):
    return next((key for key in PRODUCTS if key in text.lower()), None)


def page(key):
    return f"{PRODUCTS[key]} now costs {web_prices[key]} per month for every seat."


def complete(messages):
    system = next((m["content"] for m in messages if m["role"] == "system"), "")
    user = messages[-1]["content"]
    if system.startswith("You extract"):
        sentences = user[user.index("Sentences"):]
        key = topic(sentences)
        claims = []
        if key:
            claimed = DOLLARS.search(sentences).group()
            claims = [{"text": f"{PRODUCTS[key]} costs {claimed} per month", "kind": "pricing", "query": f"{key} price"}]
        return json.dumps({"claims": claims})
    if system.startswith("You check"):
        claim = user[user.index("Claim 1:"):].splitlines()[0]
        key, claimed = topic(claim), DOLLARS.search(claim).group()
        quote = f"{PRODUCTS[key]} now costs {web_prices[key]} per month"
        verdict = {"claim": 1, "source": 1, "quote": quote}
        if claimed == web_prices[key]:
            verdict["verdict"] = "supported"
        else:
            verdict.update(verdict="contradicted", current_value=f"{web_prices[key]} per month")
        return json.dumps({"verdicts": [verdict]})
    if system.startswith("You correct"):
        key = topic(user)
        return json.dumps({"answer": f"{PRODUCTS[key]} costs {web_prices[key]} per month [1]."})
    key = topic(user)
    if key:
        return f"{PRODUCTS[key]} costs {MODEL_PRICES[key]} per month."
    return "A binary search halves the search space on every step."


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        if self.path == "/admin/web":
            web_prices.update(body)
            payload = web_prices
        elif self.path.endswith("/chat/completions"):
            payload = {
                "id": f"mock-{uuid.uuid4().hex[:12]}",
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
            key = topic(body.get("query", ""))
            payload = {"results": [{"url": f"https://{key}.test/pricing", "title": key, "content": page(key), "score": 0.9}] if key else []}
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
