# Upstream pull request draft

Bifrost asks contributors to open an issue first and target the `dev` branch. Issue [#9748](https://github.com/maximhq/bifrost/issues/9748) already proposes a freshness-check plugin and asks where third-party plugins should live, so the plan is to comment there first and offer this as the cache-aware half. If maintainers want it in-tree, it moves to `plugins/bestbefore/` unchanged: it is its own Go module with `main.go`, tests, `changelog.md` and `version`, like `plugins/jsonparser`.

Title: `[feat]: bestbefore plugin - self-healing semantic cache with best-before dates`

---

## Summary

The semantic cache removes an entry only when its TTL passes ([`isExpiredEntry`](https://github.com/maximhq/bifrost/blob/plugins/semanticcache/v1.7.7/plugins/semanticcache/search.go#L252)). When a cached answer contains a fact that changes (a price, a version, a CEO), the cache keeps replaying it until then. This plugin re-verifies the time-sensitive claims in each chat answer against live web search. It rewrites stale answers with the verified values before the cache stores them, gives every cached answer a best-before date, and re-checks entries in the background when that date passes.

## Changes

- `PostLLMHook` runs before the semantic cache's own post-hook (custom plugins sort after built-ins and post-hooks run in reverse), so it can replace a stale answer with a corrected one and the cache stores the fix. A stale answer that cannot be corrected sets `semantic_cache-no_store` and is not cached.
- Corrections are drafted by the judge model and accepted by Go only if every verified new value is present; Go appends the source URLs.
- `HTTPTransportPostHook` annotates every non-streaming answer (`bestbefore` field, `x-bestbefore-*` headers), serves stored reports for cache hits inside their best-before date, and re-checks, heals and evicts (`DELETE /api/cache/clear/{cache_id}`) hits past it.
- A background sweeper re-checks ledger entries past their best-before date and evicts the ones that went stale.
- `HTTPTransportPreHook` serves `bestbefore/verify`, `bestbefore/status` and `bestbefore/sweep` virtual models.
- The judge model must return a verbatim quote from the source it cites; status is computed in Go, and a verdict whose quote does not prove it is ignored.
- Judge calls go back through the gateway's `/v1/chat/completions`, with `fallbacks`, and carry a per-process token so the plugin skips them.
- Trade-offs: streams are not checked; the best-before ledger is in memory per process; the sweeper can evict but not insert cache entries.

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

Expected: `5 of 5 passed`, including healing into the cache and a sweeper eviction after the mock web changes a price. New config keys are documented in the README's Configuration table.

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
