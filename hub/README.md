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

   New sessions go to the most urgent account. Accounts within 10% of the top urgency share new
   sessions.
2. **5-hour window: avoid mass failovers.** Each account's 5-hour use at its next reset is
   projected from its current burn rate, plus 5% for each session admitted in the last 15 minutes.
   An account projected to run out, or already 95% used, only gets new sessions when every account
   is in that state.
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
