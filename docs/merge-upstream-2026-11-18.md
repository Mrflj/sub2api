# Upstream Production Merge Review - 2026-11-18

## Revisions and Scope

- Local production before the saved billing fix: `16c9621be06612b78336871a0321cef8adcc4b61`.
- Saved balance-contention fix: `856d0d846`.
- Common ancestor: `dd84ac1da71391a9a112aec89f549c188a4ebe97`.
- Upstream: `ranxi2001/sub2api`, branch `production`, commit `701612f6e9f7b697eb36397b9fb49bf7324e56a1`.
- The upstream branch was checked again after verification and still pointed at that commit.
- Merge and tests ran in an isolated Git worktree. No production API, database, Redis instance, or deployment was modified.
- Backup branch: `backup/production-before-upstream-20261118` (includes the saved billing fix).

## Integration Decisions

- Preserve local configured same-account HTTP/SSE retries, the explicit partial-output retry policy, transport-reset handling, and normal-response streaming behavior.
- Preserve pool-mode safeguards against automatic account/model cooldown or disablement. Manual scheduling changes remain effective.
- Preserve requested/public model declarations and sanitized model errors in client responses, while retaining upstream model observation for internal usage/billing diagnostics.
- Preserve dedicated group web-search accounts, account search-only/disabled modes, and the local `top_k` compatibility policy.
- Preserve intact-history switching away from disabled owners. Retain upstream discovery bindings and native compaction validation without learning new ownership from compaction.
- Preserve the bounded inflight-balance reservation wait and its cancellation, quota-recheck, and reservation-release behavior.
- Keep both `web_search_account_id` and upstream `force_openai_ultrafast` through DTOs, repository mappings, and auth-cache round trips. Bump the auth snapshot version to 37 so either pre-merge snapshot is refreshed.

## Repairs Found During Verification

1. Regenerated Ent code from the merged schema. Automatic merging had left the Group descriptor indexes shifted, causing an initialization panic (`bool` asserted as `string`). Only generated mutation capacity and runtime descriptor indexes required correction.
2. Fixed an interaction between xAI server-side search reconstruction and local Anthropic custom-tool conversion. Search classification is an unexported, xAI-scoped flag; hosted search is not emitted as a client tool call, ordinary custom tools remain intact, and Responses history serialization is preserved.
3. Kept the bounded local Messages prelude staging instead of combining it with the upstream unbounded hold buffer. Retained upstream terminal side effects, search tracking, and SSE error formatting.
4. Added disabled-owner/native-compaction regression coverage: accept intact history on the replacement account, reject changed protected items, and do not learn a new owner from compaction.
5. Updated upstream usage-observation tests to assert the local public-model contract while still checking real upstream model/tier observations and billing.
6. Updated the unlimited-user concurrency SQL fixture for the upstream before/after live-handoff snapshot reads; concurrency assertions were retained.

## Verification

- All backend packages compile with `go test -mod=readonly -tags=unit -run '^$' ./...` (Windows amd64, Go 1.27.4).
- Service regression rerun: 16,096 passing test/subtest results, including the integration repairs above; 2 environment-dependent skips. Known baseline failures below were excluded only from this scoped rerun, after being reproduced separately.
- Handler regression rerun: 1,037 passing test/subtest results, no failures, and 431 environment-dependent skips; the 20 known baseline retry subtests below were excluded from this rerun.
- Complete API compatibility package: 993 passing test/subtest results. Complete DTO and migration unit packages: 53 and 175 passing results respectively.
- Frontend: 1,061 passing tests across 32 selected files, including account/group settings and local status-page coverage.
- Frontend `vue-tsc -b` and Vite production build passed.
- Embedded Windows amd64 and Linux amd64 server builds passed. Build artifacts and test logs are outside the repository.
- No conflict markers, unresolved index entries, or whitespace errors remain.

## Verification Limits and Existing Baseline Failures

This is not a claim that the entire repository test suite is green. A separate run on unmerged `856d0d846` reproduced 45 service and 21 handler failing test/subtest results, in these areas:

- Older retry/cooldown expectations: `TestAnthropicCompatAccountErrorPolicy`, `TestOpenAIChatConfiguredRetryReadFailures`, `TestHandleOpenAIUpstreamTransportError_ConfiguredPoolRetry`, and 20 subtests of `TestOpenAIResponsesConfiguredStatusRetriesSameAccount`.
- `TestOpenAIResponseFlush_KeepaliveDoesNotSplitOpenEvent` and `TestOpenAIWSHTTPBridgeSessionIsolationAcrossSameSessionHash`.
- Regional auth/scheduler persistence tests on Windows (`cannot persist regional sync state` / directory fsync behavior, including runtime tests that wait for that state).

These failures were not introduced by this merge and were not hidden by changing production behavior or loosening their assertions. Their resolution is outside this upstream integration change. The existing dedicated pool/reset, privacy, history, and balance tests remain in the scoped regression run.

An additional complete repository-package run passed 2,475 test/subtest results, with 19 skips and 8 failing results. All 8 failures were reproduced separately on unmerged `856d0d846`. Three pg-dump fixture tests require `sh` (absent from this Windows PATH); credential-encryption initialization tests fail when fsyncing a directory on Windows (`Access is denied`). The corresponding test and implementation files are unchanged by this merge. These environment failures also prevent claiming a green full repository suite.

Tests requiring an isolated real Redis server or PostgreSQL database were not validated in this environment; existing skips remain skips. New upstream SQL migrations were inspected and migration unit tests were run, but no database migration was executed. The two `251_*.sql` files have distinct filenames; the migration runner keys and orders migrations by full filename.

No remote push or deployment is part of this merge review.
