# Decisions

| When | Decision | Why |
|---|---|---|
| 10 Nov | Build a Bifrost plugin, not a standalone checker app | Problem statement 06 needs both Tavily and Bifrost. As a plugin, every app behind the gateway gets checked with no code change, which also fits statement 04 (a Bifrost extension). |
| 11 Nov | Lead with the semantic cache, not just answer checking | Issue maximhq/bifrost#9748 already proposes a freshness-check plugin, but it does not touch the cache. Bifrost's cache expires entries by TTL only (`plugins/semanticcache/search.go`, lazy `expires_at` check; issue #2414), so a stale answer is replayed until the TTL runs out. |
| 11 Nov | A regex gate before any model call | Most answers have nothing that can go out of date. Skipping them costs zero tokens and zero searches, which matters for a check that runs on every response. |
| 11 Nov | The status is decided in Go, not by the model | The judge model's verdict only counts when its quote is found word for word in the source it cites. An ungrounded verdict falls back to "uncertain", so a hallucination can never evict a good answer. |
| 11 Nov | A model or search failure gives "unchecked", never "stale" | Fail safe toward serving: an outage of the checker must not wipe the cache. |
| 11 Nov | Judge calls go back through Bifrost itself | A `.so` plugin gets no Bifrost client, so the plugin calls the gateway's own `/v1/chat/completions`. That gives the checker Bifrost's fallbacks, retries, logs and budgets for free. A header marks these calls so the plugin skips them. |
| 11 Nov | Use the HTTP transport hooks | They run on cache hits as well as misses. The LLM post-hook does not run for plugins after a cache short-circuit, so it cannot see the answers that matter most. |
| 11 Nov | Evict with the cache's own `DELETE /api/cache/clear/{id}` | It is a public, documented endpoint, and the `cache_id` is already in `extra_fields.cache_debug`. No change to Bifrost is needed. |
| 11 Nov | Recheck windows per claim kind (prices 24h, versions 72h, leadership 7d) | A cache hit is re-verified only when its most volatile claim is due, so repeated hits do not each pay for a search. |
| 11 Nov | Add a `bestbefore/verify` virtual model | Statement 06 asks for a system that "accepts an LLM-generated answer". A virtual model keeps that inside the OpenAI-compatible API, with no new route or service. |
| 11 Nov | Build the gateway and plugin in one Dockerfile | The official image is statically linked and cannot load Go plugins, and a plugin must match the host's Go version, libc and dependency versions exactly. |
| 11 Nov | Skip the dashboard's TypeScript check in the image build | It only validates upstream's own source, and it stalled for over 25 minutes in a 4 GB Docker Desktop VM. |
| 11 Nov | Gemini 2.5 Flash as judge, falling back to Flash-Lite-latest and then 3.5 Flash | Free tier and JSON mode. `gemini-2.5-flash-lite` returned 404 for our key, so it was dropped from the chain after a live probe. |
| 11 Nov | Copy the gateway's `replace` directives into the plugin module | The plugin first failed to load: "plugin was built with a different version of package github.com/mark3labs/mcp-go/server". Bifrost replaces mcp-go with its own fork, and Go ignores replaces declared by dependencies, so `scripts/align-deps.sh` now copies them. |
| 11 Nov | Provider retries (4, backoff 1–10 s) and a 60 s checker budget | Gemini returned intermittent 503s. Bifrost's own retries absorb them, but with a 25 s budget the judge call timed out and the answer came back `unchecked`. |
| 11 Nov | Mock provider needs `allow_private_network` | Bifrost blocks providers on private IPs by default (SSRF protection), which includes the e2e mock's Docker network address. |
