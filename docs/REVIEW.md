# Adversarial review

We ran three adversarial reviews, each by a separate AI reviewer given the code, not our conclusions.

## First review: the checker

On 11 Nov 2026 we gave the code, not our conclusions, to a separate AI reviewer and asked it to break the plugin. It wrote attack tests and ran them in Docker. It found 12 problems. All are fixed except two parts that are accepted and listed in the README's Limits (see the end of this page). Each fix has a test that fails without it, except the demo page fix, which was checked by hand.

| # | Severity | What broke | Fix | Test |
|---|---|---|---|---|
| 1 | High | The quote check only asked "is this text somewhere in the source?". A judge could say `contradicted`, quote the claim's own old value ("It was $10 per month last year") or a fragment like "per month", invent a `current_value`, and evict a good answer. | A contradiction counts only if the words of its `current_value` that are not already in the claim all appear in the quote, so quoting the old value back or inventing a value proves nothing. Quotes need five words for support and three for a contradiction. (Our first version matched the whole value as one string and required five words everywhere; the first eval run showed that threw out real contradictions, so it was loosened. See DECISIONS.md.) | `TestContradictionsThatProveNothingAreUncertain` (6 attack cases) |
| 1b | High | Web pages and the user's question went into prompts raw, so a page could forge another `Source` block. | Sources and the question are wrapped in tags, and any tag-like text inside them is defanged. The prompts say tagged text is data. | `TestPromptsCannotBeForged` |
| 2 | High | If the extractor returned no claims, the answer was marked timeless and its ledger entry never expired, so one bad or injected reply could pin a stale cached answer for good. | Timeless-by-model now expires after a day. Only answers the regex gate skips are timeless forever. | `TestEmptyExtractionIsOnlyTrustedForADay`, `TestLedgerExpiresOnRecheckWindow` |
| 3 | Medium | A Tavily outage looked like "no evidence", so the answer became `uncertain` and was trusted for 24 hours. | If every search fails, the result is `unchecked` and is not stored. | `TestSearchOutageIsUncheckedNotUncertain` |
| 4 | Medium | The demo's nginx proxied the cache admin API, and any client could send `x-bestbefore-internal` to skip checking. | nginx proxies only `/v1/chat/completions` and strips the header. The header's value is now a random token made at start-up, so a forged one is ignored. | `TestForgedInternalHeaderIsStillChecked` |
| 5 | Medium | A negative relevance score flipped a contradiction into a fresh verdict. | The relevance weight is clamped to 0–1. | `TestWeakEvidencePullsScoreTowardUncertain` |
| 6 | Medium | `max_claims: 0` removed the cap and `timeout_seconds: 0` failed every check; eviction had no deadline. | Limits must be positive or `Init` fails; eviction has a 5 s deadline. | `TestInitRejectsBadConfig` |
| 7 | Medium | A `bestbefore/verify` response could in theory be checked again by the post-hook. | The post-hook skips the verify model. | `TestSkipsStreamsErrorsAndOwnCalls` |
| 8 | Low | `cache_id` was not escaped in the eviction URL. | `url.PathEscape`. | `TestCacheIDIsEscapedInEvictionURL` |
| 9 | Low | Error text sent to callers named internal hosts. | Callers get "search failed" and similar; the detail goes to the gateway log. | `TestModelFailureIsUncheckedNotStale` |
| 10 | Low | The demo page threw on a malformed source URL and would link a `javascript:` URL. | Only `http` and `https` URLs become links. | manual |
| 11 | Low | With two verdicts for one claim, the last won. | The first wins. | `TestFirstVerdictPerClaimWins` |
| 12 | Low | "costume" matched "cost", "lawn" matched "law", and years after 2029 were missed. | Word boundaries; `20\d\d`. | `TestDetect` |

Test-suite problems it also found, now fixed: a data race in the search fake (caught with `go test -race`), and a ledger test that used a timeless report, so it never tested the recheck window.

**Accepted, not fixed** (in the README's Limits): there is no request coalescing, so many concurrent hits on one due entry each pay for a check; and a stale page that Tavily ranks highly can still make a stale answer look fresh, because one grounded source is enough.

`go test -race ./...` passes after the fixes.

## Second review: healing and the sweeper

After adding self-healing and the background sweeper, we ran the same kind of review on the new code. It wrote attack tests, ran them under `-race`, and read Bifrost v2.5.1's source to check our assumptions about hook order and cache ids. It found 13 problems; these are fixed, each with a test:

| # | Severity | What broke | Fix | Test |
|---|---|---|---|---|
| 1 | High | The rewrite check only looked for the new value. A draft that left the answer unchanged (when the new value already appeared elsewhere), kept the old value, or added a phishing link and new sentences was accepted, and the cache stored it as "healed". | A rewrite must change the answer, may use only words from the answer and the verified values, must not contain any number that was only in the outdated claim, must not contain a link, and must stay within twice the original length. | `TestHealRejectsDraftsThatAreStillStaleOrAddText` (6 attacks) |
| 2 | High | Reloading the plugin wrote the shared best-before map while requests read it: a Go concurrent map write, which crashes the process. Overrides also leaked across reloads. | Each plugin instance gets its own copy; the package defaults are never written. | `TestRecheckWindowsBelongToOnePluginInstance` |
| 3 | High | `bestbefore/sweep` let any caller trigger paid re-checks of every cached answer, any number of times in parallel, and `bestbefore/status` listed every cached question. | Both are off unless `admin_models` is set; only one sweep runs at a time. | `TestAdminModelsAreOffByDefault`, `TestOnlyOneSweepRunsAtATime` |
| 4 | Medium | Results were matched to responses by the provider's response id alone; ids that repeat could attach one answer's result to another, and a cache hit could consume a miss's result. | Matched by id plus a hash of the answer text; cache hits never consume a result. | `TestResultsWithTheSameResponseIDDoNotCross` |
| 5 | Medium | When a background re-check failed, the old "fresh" verdict kept being served and the failed check was repeated every minute. | The entry is marked `unchecked` and retried with exponential backoff, up to an hour. | `TestSweeperBacksOffAndStopsVouchingWhenACheckFails` |
| 6 | Medium | A slow background re-check could overwrite or evict a newer, healed entry stored under the same cache id meanwhile, and the delayed second eviction could delete a fresh re-store. | The sweeper only updates or deletes the entry it actually checked (compare-and-swap on the check time); the second eviction is skipped if a newer answer was recorded. | `TestLedgerConditionalUpdates` |
| 7 | Medium | A sweep in progress ignored plugin shutdown and kept spending. | Checks run under the plugin's context, which is cancelled on cleanup or reload. | covered by the race-enabled suite |
| 8–13 | Low | Multi-choice responses were only half checked; the ledger could evict due entries first and hold unbounded text; healing a healed answer duplicated its sources list; the active-plugin pointer was unsynchronised; counters double-counted. | Multi-choice responses are skipped; timeless entries are evicted first and text is capped; old source lists are stripped before healing; the pointer is atomic; the sweeper counts an eviction only when it succeeds (the overall eviction count includes the sweeper's). | `TestHealingAHealedAnswerKeepsOneSourcesList` and the suite under `-race` |

**Accepted, documented:** the plugin assumes Bifrost's default placement for custom plugins (after the built-ins), which is what makes healing before the cache write work; a custom `pre_builtin` placement would break that ordering. It is listed in the README's Limits.

## Third review: streaming, background revalidation, scope and official-site search

After adding streaming, background revalidation, `check_scope` and the official-site search, a third reviewer attacked that code with scratch tests (four failed exactly as predicted before our fixes). Ten findings; these are fixed:

| # | Severity | What broke | Fix | Test |
|---|---|---|---|---|
| 1 | High | A fresh streamed answer was cached with no best-before date: the ledger never sees streams, so it could be replayed for the whole cache TTL without a re-check. | Every answer BestBefore lets into the cache now carries a per-request cache TTL equal to its best-before date (the cache's own `semantic_cache-ttl` switch), so the cache itself drops it when it is due. This also covers answers whose ledger entry was lost on restart. | `TestCachedAnswersExpireAtTheirBestBeforeDate`, `TestFreshStreamIsCachedOnlyUntilItsBestBeforeDate`, e2e `test_fresh_stream_is_cached_and_replayed` |
| 2 | Medium-high | When a request asked for the cache but the cache declined to store it (long conversations, large payloads), the answer could be rewritten yet arrive with no report. | A pending result is always reported before the scope check. | `TestHealedAnswerIsReportedEvenWhenTheCacheSkippedIt` |
| 3 | Medium | Two background re-checks of an id unknown to the ledger could race, and the slower one evicted a newer healed answer. | After claiming the id, a re-check stops if the ledger holds anything newer than its snapshot. | `TestRecheckSkipsWhenTheLedgerHasANewerAnswer` |
| 4 | Medium | The model-chosen "official site" could be steered by the question, or land on shared hosts (github.io, pypi.org, blogspot.com), making a planted page "official". | A site is searched as official only if the general results already cite it, and never if it is a shared host. | `TestBadOrMissingSiteFallsBackToOneSearch` |
| 5 | Medium | A failed re-check of an unknown id stored nothing, so every hit paid again; background checks had no global bound. | Failures are stored with backoff; background revalidation runs in a pool of four and skips when full. | `TestFailedRecheckOfAnUnknownEntryIsRemembered` |
| 6 | Medium | The end of a stream could wait up to two full timeouts for the check. | Streams get a 20-second budget; if it runs out the stream is simply not cached. | `TestStreamThatCannotBeCheckedIsNotCached` |
| 9 | Test gap | The e2e stream test would have passed even if streams were never cached. | A positive control: a fresh stream is replayed from the cache. | e2e |
| 10 | Low | `searches` counted claims, not Tavily calls. | It counts calls. | covered |

**Accepted and documented** (README Limits): with raw-response capture turned on in Bifrost, a stream's report can be dropped from the final chunk (finding 7); a cache configured with `default_cache_key` caches requests without the header, which `check_scope: "cached"` does not see, so such deployments should set `check_scope: "all"`; only the first choice of a multi-choice stream is checked (finding 8).
