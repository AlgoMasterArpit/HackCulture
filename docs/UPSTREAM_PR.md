# Upstream pull request draft

Bifrost asks contributors to open an issue first and target the `dev` branch. Issue [#9748](https://github.com/maximhq/bifrost/issues/9748) already proposes a freshness-check plugin and asks where third-party plugins should live, so the plan is to comment there first and offer this as the cache-aware half. If maintainers want it in-tree, it moves to `plugins/bestbefore/` unchanged: it is its own Go module with `main.go`, tests, `changelog.md` and `version`, like `plugins/jsonparser`.

Title: `[feat]: bestbefore plugin - re-verify time-sensitive claims and evict stale semantic-cache entries`

---

## Summary

The semantic cache removes an entry only when its TTL passes ([`isExpiredEntry`](https://github.com/maximhq/bifrost/blob/plugins/semanticcache/v1.7.7/plugins/semanticcache/search.go#L252)). When a cached answer contains a fact that changes (a price, a version, a CEO), the cache keeps replaying it until then. This plugin re-verifies the time-sensitive claims in each chat answer against live web search, adds a freshness report to the response, and evicts stale entries by `cache_id`. It runs on cache hits too, and re-checks a hit only when its most volatile claim is due.

## Changes

- `HTTPTransportPostHook` reads the answer and `extra_fields.cache_debug`, runs the check, writes `bestbefore` into the body and `x-bestbefore-status` / `x-bestbefore-score` headers, and calls `DELETE /api/cache/clear/{cache_id}` for stale answers.
- `HTTPTransportPreHook` serves a `bestbefore/verify` virtual model that checks an answer passed in as the last message.
- A regex gate skips answers with no time-sensitive wording, at zero cost.
- The judge model must return a verbatim quote from the source it cites. The status is computed in Go, and a verdict whose quote is not in the source is ignored, so a hallucinated verdict cannot evict a good answer.
- Judge calls go back through the gateway's `/v1/chat/completions`, with `fallbacks`, and carry `x-bestbefore-internal` so the plugin skips them.
- Trade-offs: streams are not checked (headers are committed before the answer exists); the recheck ledger is in memory per process.

## Type of change

- [x] Feature

## Affected areas

- [x] Plugins

## How to test

```sh
go version            # go1.27.3
go test ./...         # unit tests with stub search and judge
docker compose -f e2e/compose.yml up -d --build
python e2e/test_gateway.py   # a dynamic Bifrost build with the plugin, mock provider and mock search
```

Expected: `4 of 4 passed`. New config keys are documented in the README's Configuration table.

## Breaking changes

- [x] No

## Related issues

Relates to #9748 (freshness-check proposal) and #2414 (expired entries are only removed lazily).

## Security considerations

- The Tavily key is read from `env.` references, never logged, and never returned to callers.
- The plugin calls the gateway's own cache-clear endpoint on loopback. Deployments that protect `/api/*` with auth need `virtual_key` or a loopback exemption.
- Answer text is sent to the configured judge model and to Tavily as search queries. Operators who must not send content off-box should point `judge_model` at a local provider and `tavily_url` at a self-hosted search.

## Checklist

- [x] I added/updated tests where appropriate
- [x] I updated documentation where needed
- [x] I verified builds succeed (Go)
