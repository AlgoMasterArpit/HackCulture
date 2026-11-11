# Adversarial review

On 11 Nov 2026 we gave the code, not our conclusions, to a separate AI reviewer and asked it to break the plugin. It wrote attack tests and ran them in Docker. It found 12 problems. All are fixed except two parts that are accepted and listed in the README's Limits (see the end of this page). Each fix has a test that fails without it, except the demo page fix, which was checked by hand.

| # | Severity | What broke | Fix | Test |
|---|---|---|---|---|
| 1 | High | The quote check only asked "is this text somewhere in the source?". A judge could say `contradicted`, quote the claim's own old value ("It was $10 per month last year") or a fragment like "per month", invent a `current_value`, and evict a good answer. | Quotes need at least five words. A contradiction counts only if its `current_value` appears inside the quote and does not appear in the claim. | `TestContradictionsThatProveNothingAreUncertain` (6 attack cases) |
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
