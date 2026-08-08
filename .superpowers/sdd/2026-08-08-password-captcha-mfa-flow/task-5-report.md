# Task 5: residual authentication lifecycle fixes — RED/GREEN report

Base commit: `6070a15f695f19c0d08ec4002412f02749062d6c`

## Scope and safety

- Repaired all four residual findings as one lifecycle-focused change.
- Preserved the Discord password modal → owner-triggered bot-host Chrome CAPTCHA on `authenticate.riotgames.com` → owner-bound Discord MFA modal flow.
- Used only deterministic fakes, local process seams, and `httptest` Discord endpoints. No live Riot or Discord credentials were used or recorded.
- Kept external I/O and `WaitGroup.Wait` outside `Server.mu`, handler lifecycle mutexes, and edit-guard mutex critical sections.

## Finding A — legacy browser-auth lifecycle drain

### RED

Each regression names the production mutation it catches immediately before the test body:

- `TestBeginAuthRejectsAfterShutdownWithoutState`: catches removal of closed-gate admission.
- `TestShutdownJoinsConcurrentBeginAuthAndRollsBackPendingState`: catches removal of lifecycle enrollment or post-persist rollback.
- `TestWaitBrowserLoginWakesOnShutdownWithServerClosed`: catches waiting only on the caller context.
- `TestShutdownCancelsAndJoinsCompleteFromRedirectURL`: catches redirect completion bypassing lifecycle cancellation/join.
- `TestShutdownJoinsFullCallbackAndPreventsPostCloseOutcome`: catches enrollment ending before callback outcome publication.

Focused RED command against unchanged production code:

```text
go test ./internal/authweb -run 'Test(BeginAuthRejectsAfterShutdownWithoutState|ShutdownJoinsConcurrentBeginAuthAndRollsBackPendingState|WaitBrowserLoginWakesOnShutdownWithServerClosed|ShutdownCancelsAndJoinsCompleteFromRedirectURL|ShutdownJoinsFullCallbackAndPreventsPostCloseOutcome)$' -count=1
```

Observed RED: all five regressions failed. `BeginAuth` returned nil after shutdown; concurrent begin outlived shutdown and leaked pending/outcome state; the browser waiter stayed blocked; redirect completion was neither canceled nor joined; and the full callback outlived shutdown and republished an outcome.

### GREEN implementation

- Enrolled `BeginAuth`, `WaitBrowserLogin`, and `CompleteFromRedirectURL` in the auth server lifecycle.
- Added a post-persist closed boundary to `BeginAuth`; a concurrent shutdown winner consumes the newly persisted state before returning `ErrServerClosed`.
- Composed browser waiting and redirect identity work with the root lifecycle context.
- Enrolled the entire callback, including form processing, completion, outcome publication, and response writing.
- Gated outcome publication on `Server.closed`.
- Moved legacy outcome clearing until after lifecycle workers drain, then consumed remaining persisted pending states outside `Server.mu`.
- Split redirect account preparation from irreversible commit so cancelable Riot work completes before the commit claim and no external I/O occurs under `Server.mu`.

Focused GREEN evidence:

```text
go test ./internal/authweb -run 'Test(BeginAuthRejectsAfterShutdownWithoutState|ShutdownJoinsConcurrentBeginAuthAndRollsBackPendingState|WaitBrowserLoginWakesOnShutdownWithServerClosed|ShutdownCancelsAndJoinsCompleteFromRedirectURL|ShutdownJoinsFullCallbackAndPreventsPostCloseOutcome)$' -count=1
go test -race ./internal/authweb -run 'Test(BeginAuthRejectsAfterShutdownWithoutState|ShutdownJoinsConcurrentBeginAuthAndRollsBackPendingState|WaitBrowserLoginWakesOnShutdownWithServerClosed|ShutdownCancelsAndJoinsCompleteFromRedirectURL|ShutdownJoinsFullCallbackAndPreventsPostCloseOutcome)$' -count=1
```

Result: both passed.

## Finding B — Discord REST lifecycle cancellation

### RED

Mutation-targeted regressions:

- `TestHandlersShutdownCancelsBlockedInteractionAcknowledgement`: catches an ACK that discards the interaction callback context.
- `TestHandlersShutdownCancelsBlockedQRWatcherTerminalEdit`: catches a QR watcher edit that discards its lifecycle context.
- `TestHandlersShutdownCancelsBlockedPasswordWatcherTerminalEdit`: catches a password watcher edit that discards its lifecycle context.

Focused RED command:

```text
go test ./internal/bot -run 'TestHandlersShutdownCancelsBlocked(InteractionAcknowledgement|QRWatcherTerminalEdit|PasswordWatcherTerminalEdit)$' -count=1
```

Observed RED: all three handler shutdowns reached their 500 ms caller deadline with `context deadline exceeded` while the local Discord REST endpoint remained blocked.

### GREEN implementation

- Made ACK, modal response, component defer, ephemeral response, component update, source-message edit, and file-edit helpers accept the active lifecycle context.
- Passed `discordgo.WithContext(ctx)` to every lifecycle-path `discordgo` REST request.
- Propagated callback contexts through command, component, and modal handlers and watcher contexts through QR/password terminal edits.
- Retained only thin background wrappers used by direct tests/non-lifecycle compatibility paths.

Focused GREEN evidence:

```text
go test ./internal/bot -run 'TestHandlersShutdownCancelsBlocked(InteractionAcknowledgement|QRWatcherTerminalEdit|PasswordWatcherTerminalEdit)$' -count=1
go test -race ./internal/bot -run 'TestHandlersShutdownCancelsBlocked(InteractionAcknowledgement|QRWatcherTerminalEdit|PasswordWatcherTerminalEdit)$' -count=1
```

Result: both passed; handler shutdown canceled the real local HTTP requests and drained the enrolled workers.

## Finding C — Chrome process-group lifetime

### RED

Mutation-targeted regressions:

- `TestWaitForCaptchaProcessGroupExitIgnoresLeaderExitWhileGroupLives`: catches consulting only the leader exit channel.
- `TestCaptchaBrowserCloseRetriesAfterLeaderExitWhileGroupLives`: catches profile deletion while a surviving group member may still own it.

Focused RED command:

```text
go test ./internal/authweb -run 'Test(WaitForCaptchaProcessGroupExitIgnoresLeaderExitWhileGroupLives|CaptchaBrowserCloseRetriesAfterLeaderExitWhileGroupLives)$' -count=1
```

Observed RED: the test package did not compile because the process-group probe seam and owned-process wait abstraction did not exist (`waitForCaptchaProcessGroupExitWithProbe` undefined and `waitProcessExit` missing).

### GREEN implementation

- Added a Unix owned-process-group existence probe using signal 0 against the negative process-group ID.
- Treats `ESRCH` as gone, `EPERM` as alive, and unknown probe errors as possibly alive.
- Added bounded polling after TERM and KILL and deliberately ignored leader exit as proof of group disappearance on Unix.
- Made controller profile removal depend on the platform-owned exit predicate on every close/retry path, including failed launch cleanup.
- Preserved Windows leader-process behavior behind the platform abstraction.

Focused GREEN evidence:

```text
go test ./internal/authweb -run 'Test(WaitForCaptchaProcessGroupExitIgnoresLeaderExitWhileGroupLives|CaptchaBrowserCloseRetriesAfterLeaderExitWhileGroupLives)$' -count=1
go test -race ./internal/authweb -run 'Test(WaitForCaptchaProcessGroupExitIgnoresLeaderExitWhileGroupLives|CaptchaBrowserCloseRetriesAfterLeaderExitWhileGroupLives)$' -count=1
go test ./internal/authweb -run 'Test(StartChromeLogged|ImmediateLaunchFailure|CaptchaBrowserClose)' -count=1
```

Result: all passed. A live simulated group retains the profile with `ProcessExited:false`; a later retry removes it only after group disappearance.

## Finding D — unreachable CAPTCHA-to-MFA continuation

### RED

Mutation-targeted regressions:

- `TestCancelPasswordMFAIsOwnerBoundIdempotentAndExpiredSafe`: catches deletion before owner validation and rejection of valid expired-state cleanup.
- `TestCancelPasswordMFADetachesCancelsAndWaitsOutsideServerMutex`: catches waiting under `Server.mu` or returning before in-flight MFA work drains.
- `TestPasswordCaptchaWatcherRollsBackOnlyUndeliveredOwnedMFA`: catches missing failed-delivery rollback and over-broad cancellation after success, no MFA, or owner mismatch.

Focused RED commands:

```text
go test ./internal/authweb -run 'TestCancelPasswordMFA(IsOwnerBoundIdempotentAndExpiredSafe|DetachesCancelsAndWaitsOutsideServerMutex)$' -count=1
go test ./internal/bot -run '^TestPasswordCaptchaWatcherRollsBackOnlyUndeliveredOwnedMFA$' -count=1
```

Observed RED: the server test did not compile because `CancelPasswordMFA` did not exist. The watcher regression then showed zero cancellation calls on failed MFA delivery and retained the cached hint/control guard.

### GREEN implementation

- Added `CancelPasswordMFA` to the production auth interface and server.
- Under `Server.mu`, validates ownership and atomically detaches the state; after unlocking, cancels the flow and waits for in-flight MFA work.
- Missing/already-consumed states are idempotent success; an expired but still-present owner state is removable; wrong-owner attempts return `ErrMFAOwner` without mutation.
- On a failed terminal Discord edit with a non-empty MFA state, the password watcher cancels using the interaction owner. Only a successful cancellation clears the cached hint and retires the local submission guard.
- Successful delivery, no-MFA completion, and wrong-owner cancellation retain the appropriate continuation state.

Focused GREEN evidence:

```text
go test ./internal/authweb -run 'TestCancelPasswordMFA(IsOwnerBoundIdempotentAndExpiredSafe|DetachesCancelsAndWaitsOutsideServerMutex)$' -count=1
go test -race ./internal/authweb -run 'TestCancelPasswordMFA(IsOwnerBoundIdempotentAndExpiredSafe|DetachesCancelsAndWaitsOutsideServerMutex)$' -count=1
go test ./internal/bot -run '^TestPasswordCaptchaWatcherRollsBackOnlyUndeliveredOwnedMFA$' -count=1
go test -race ./internal/bot -run '^TestPasswordCaptchaWatcherRollsBackOnlyUndeliveredOwnedMFA$' -count=1
```

Result: all passed.

## Shared edit-guard lock constraint

The existing CAPTCHA and MFA serialization guards held their mutexes across Discord or Riot I/O. The task's global constraint required separating logical serialization from mutex ownership.

Mutation-targeted RED tests:

- `TestCaptchaEditDoesNotHoldGuardMutexAcrossDiscordIO`
- `TestMFASubmitDoesNotHoldGuardMutexAcrossRiotIO`

RED command:

```text
go test ./internal/bot -run 'Test(CaptchaEditDoesNotHoldGuardMutexAcrossDiscordIO|MFASubmitDoesNotHoldGuardMutexAcrossRiotIO)$' -count=1
```

Observed RED: both tests reported that the corresponding edit-guard mutex remained held while the deterministic external call was blocked.

GREEN implementation: introduced a short-lock `begin`/`finish` logical lease with context-aware waiting. The mutex now only protects `busy`, `terminal`, and waiter notification state; Riot and Discord calls occur after it is released. Existing terminal suppression and concurrent-MFA ordering semantics remain serialized.

GREEN evidence:

```text
go test ./internal/bot -run 'Test(CaptchaEditDoesNotHoldGuardMutexAcrossDiscordIO|MFASubmitDoesNotHoldGuardMutexAcrossRiotIO|CaptchaTerminalEditSuppressesConcurrentReopenStatus|ConcurrentMFAModalResultsDoNotOverwriteSuccess|PasswordCaptchaWatcherRollsBackOnlyUndeliveredOwnedMFA)$' -count=1
go test -race ./internal/bot -run 'Test(CaptchaEditDoesNotHoldGuardMutexAcrossDiscordIO|MFASubmitDoesNotHoldGuardMutexAcrossRiotIO|CaptchaTerminalEditSuppressesConcurrentReopenStatus|ConcurrentMFAModalResultsDoNotOverwriteSuccess|PasswordCaptchaWatcherRollsBackOnlyUndeliveredOwnedMFA)$' -count=1
```

Result: both passed.

## Package and repository verification

All commands below exited 0:

```text
go test ./internal/authweb -count=1
go test ./internal/bot -count=1
go test -race ./internal/authweb -count=1
go test -race ./internal/bot -count=1
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./cmd/bot
git diff --check
GOOS=windows GOARCH=amd64 go test -c ./internal/authweb -o <temp>/authweb.test.exe
GOOS=windows GOARCH=amd64 go test -c ./internal/bot -o <temp>/bot.test.exe
GOOS=linux GOARCH=arm64 go build -o <temp>/valorant-bot-linux-arm64 ./cmd/bot
```

The changed Go files also produced no `gofmt -d` output.

The combined new concurrency regressions were additionally repeated 10 times
under the normal runtime and three times under `-race` for each changed package;
all repeat runs passed.

## Diff and credential inspection

- Reviewed every production diff and all four new regression files.
- Ran a targeted high-risk credential literal scan over every changed/new Go file; it returned no matches. `gitleaks` was not installed.
- All interaction IDs/tokens, account names, MFA codes, JWT material, and states in tests are fixed synthetic placeholders or locally generated test values.
- No user-installed helper, public CAPTCHA route, user-side localhost path, tunnel token flow, credential/state logging, or headless password-CAPTCHA claim was introduced.

## Changed files

- `internal/authweb/server.go`, `shutdown.go`: lifecycle enrollment, outcome gate/rollback, owner-bound MFA cancellation.
- `internal/authweb/captcha_launch.go`, `captcha_process_unix.go`, `captcha_process_windows.go`: platform-owned exit predicate and Unix group polling.
- `internal/bot/discord.go`, `handlers.go`, `types.go`: Discord context propagation, failed MFA delivery rollback, short-lock edit guards.
- `internal/authweb/legacy_lifecycle_test.go`, `captcha_process_unix_test.go`, `mfa_cancel_test.go`, `internal/bot/discord_context_test.go`: new deterministic regressions.
- `internal/bot/discord_internal_test.go`, `handlers_test.go`: watcher/guard regressions and interface fakes.

## Concerns

None identified. Verification is local execution evidence from the authoring context; no external services or live authentication flows were exercised.
