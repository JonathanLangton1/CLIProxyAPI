# Home hub fork

This fork of [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) adds one routing strategy,
`quota-pacing`, and runs on the home hub (`home-server`). Upstream treats quota-aware routing as out
of scope ([#4715](https://github.com/router-for-me/CLIProxyAPI/issues/4715)), so it lives here.

## What `quota-pacing` does

It uses the rate-limit headers Claude and Codex subscriptions already return; the proxy keeps the
latest ones per account. It makes no extra requests. It only decides where a **new** session goes.
Sessions already on an account stay there (session affinity), so their prompt cache keeps hitting.

1. **Weekly window: avoid waste.** Each account's urgency is the extra burn rate per hour it needs
   to use its remaining weekly quota before the reset:

   `(1 - used) / hours_to_reset - current_burn_rate`

   New sessions are shared by smooth weighted rotation, weighted by urgency × 5-hour headroom. The
   account most at risk of losing weekly quota takes most new sessions, but a burst that arrives
   before any response has updated the readings isn't piled onto one 5-hour window.
2. **5-hour window: avoid mass failovers.** Each account's 5-hour use at its next reset is
   projected from its current burn rate, plus 5% for each session admitted in the last 5 minutes
   (capped at 25%, so the burst of rebinds after a restart can't flip every account to pressured).
   An account in any of these states only gets new sessions when every account is in one of them:
   - projected to run out before its 5-hour reset;
   - 5-hour window 95% used;
   - weekly window 95% used.

   Among such accounts, the one with the most 5-hour room goes first.
3. **Safety rules:**
   - Accounts with no data yet get one probe session every 10 minutes.
   - Rejected accounts go last, ordered by earliest recovery.
   - API keys and providers without usage data rank after subscriptions.
   - It never refuses a pick that round-robin would have served.

Every pick is logged at info level as `quota-pacing: picked auth=… tier=…`, together with the
numbers behind it.

## Deploy or update

On the hub:

```bash
cd ~/Github/CLIProxyAPI
hub/deploy.sh            # build, smoke test on a spare port, swap in under an auto-rollback guard
tail -f ~/cliproxyapi/fork-builds/deploy.log
hub/rollback.sh          # back to the stock binary and round-robin
```

The build uses `CGO_ENABLED=0 GOAMD64=v1`. The hub's i5-2500K has no AVX2, and it has no C
toolchain, so plugins are not supported.

## Moving to a new upstream release

```bash
git fetch upstream --tags
git rebase --onto vX.Y.Z v8.0.44 feat/quota-pacing   # replace v8.0.44 with the current base tag
go test ./sdk/cliproxy/auth/ -run 'QuotaPacing|ParseQuotaWindows'
hub/deploy.sh
```

## Laptops

Laptops never run the proxy. Point a new machine at the hub with:

```bash
bash <(ssh home-server cat cliproxyapi/laptop-setup.sh)
```

## Simulation

`sdk/cliproxy/auth/quota_pacing_sim_test.go` replays 14 days of synthetic Claude Code traffic
against the real selectors and compares them with a theoretical best, which spreads each minute's
demand over accounts in order of weekly reset, with no session stickiness and no cache cost:

```bash
go test -tags quotasim -run TestQuotaPacingSimulation -v ./sdk/cliproxy/auth/
```

Results on 2026-10-27 for the deployed version. That's 80 scenarios: 4 account setups × 5 demand
patterns, each with and without 50 one-shot sessions an hour, × 2 week/5h ratios, × 8 seeds.

| Strategy | Share of best | Best or tied | Failovers |
|---|---|---|---|
| quota-pacing | 98.27% | 73/80 | 3471 |
| round-robin | 97.57% | 45/80 | 3946 |
| soonest-reset | 96.45% | 32/80 | 8368 |

Other rankings for healthy accounts scored 97.4–97.9%:
- greedy;
- earliest-deadline;
- cubed weights.

Admission windows of 15 minutes uncapped, 5 minutes uncapped, and none at all scored at or below
the shipped 5 minutes capped at 25%.
