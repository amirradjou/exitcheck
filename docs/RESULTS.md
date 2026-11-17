# exitcheck results

What each milestone measured, when, and what it showed. How each part works is in [GUIDE.md](GUIDE.md).

## Milestones

| | Milestone | Done when |
|---|---|---|
| M0 ✅ | `exitcheck facts <address>`: contract, owner powers, holders, LP state | correct on 20 hand-picked tokens (10 known scams, 10 legitimate) — **20/20, see [M0 results](#m0-results)** |
| M1 ✅ | Buy-then-sell simulation on an Anvil fork | every known honeypot in the set flagged; no legitimate token flagged — **6/6 flagged, 0/10 legitimate flagged, see [M1 results](#m1-results)** |
| M2 ✅ | Telegram bot: verdict + reasons, cached in Redis | median answer under 10 s cold, instant when cached — **9.1 s cold, 0.5 ms cached, see [M2 results](#m2-results)** |
| M3 | Live watch: factory events → Kafka → analyzers → alerts channel | a day of launches processed with lag under a minute and no duplicates — **the measured day: 262 of 264 under a minute (median 8.9 s), 0 duplicates, but 36 of Ethereum's 91 launches never seen (since fixed); no full day has been measured since the fixes, see [M3 results](#m3-results)** |
| M4 | Labelled dataset, features, model, MLflow | time-split precision/recall published; beats the hard rules alone — **published, not yet beating them clearly, see [M4 status](#m4-status)** |
| M5 ✅ | Grounded "why" explanations | on a review set, every sentence cites a check or a source line — **58 of 58, read by hand, see [M5 results](#m5-results)** |
| M6 | Public demo and results | README shows tokens analysed, precision at the alert threshold, median time to verdict; a hosted demo is postponed |

## M0 results

`exitcheck eval testdata/tokens.json`, run 2026-10-29 on the default public RPCs: **20/20 tokens correct, 150/150
checks passed**, median 7.4 s per token.

Each token in [`testdata/tokens.json`](../testdata/tokens.json) carries the evidence for its label and only the facts
that were established without exitcheck: deployer and creation transaction from Blockscout and `cast`, verification
status from the explorers, owners and LP supplies read with `cast`, privileged functions from the verified source's
modifiers, and, for unverified contracts, by calling them as the owner on an Anvil fork.

- **Scams** are labelled by what happened on chain, not by a third-party detector. Honeypots: many wallets bought
  and only an insider ever sold (buyers and sellers counted by transaction sender). Rugs: buyers paid in, then the
  deployer removed the liquidity. honeypot.is was tried as ground truth and dropped: it flagged tokens whose buyers
  kept selling.
- **Legitimate** tokens have traded on major exchanges for years. Several hold real powers (USDT's owner can mint,
  blacklist and pause), which is why the verdict will not rest on powers alone.

| label | chain | token | address | facts | time |
|---|---|---|---|---|---|
| legit | ethereum | USDT | `0xdAC17F958D2ee523a2206206994597C13D831ec7` | correct | 7.4 s |
| legit | ethereum | USDC | `0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48` | correct | 63.0 s |
| legit | ethereum | DAI | `0x6B175474E89094C44Da98b954EedeAC495271d0F` | correct | 8.1 s |
| legit | ethereum | PEPE | `0x6982508145454Ce325dDbE47a25d4ec3d2311933` | correct | 5.4 s |
| legit | ethereum | UNI | `0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984` | correct | 4.7 s |
| legit | ethereum | LINK | `0x514910771AF9Ca656af840dff83E8264EcF986CA` | correct | 5.4 s |
| legit | ethereum | SHIB | `0x95aD61b0a150d79219dCF64E1E6Cc01f0B64C4cE` | correct | 5.2 s |
| legit | base | BRETT | `0x532f27101965dd16442E59d40670FaF5eBB142E4` | correct | 7.5 s |
| legit | base | AERO | `0x940181a94A35A4569E4529A3CDfB74e38FD98631` | correct | 7.4 s |
| legit | base | DEGEN | `0x4ed4E862860beD51a9570b96d89aF5E1B0Efefed` | correct | 23.5 s |
| scam | ethereum | ASN | `0xBe2ab177599C6381C70725FEEE6663F9bCfF2197` | correct | 5.3 s |
| scam | ethereum | QTC | `0x541E9505Aaf0a7D2d26fe0123d0f2419cDbb3ad7` | correct | 5.1 s |
| scam | ethereum | GTA6 | `0x619Ef2228603f5b5613C9aa955B0F0b84bfDcd60` | correct | 8.2 s |
| scam | ethereum | FREEDOM250 | `0xD2Ab2Bf8C6e9e8070C8D9C24001d9a579620CaB2` | correct | 9.8 s |
| scam | ethereum | AMERICA250 | `0x5e82442d47b2e14b6F621a256DfB115DBd9f83ee` | correct | 6.2 s |
| scam | ethereum | DREAM | `0x809010Db55F12eF7E082d6e58461C58e27F1Aa9B` | correct | 7.0 s |
| scam | ethereum | ARC | `0x42Bf9A7D31534096CA80f68D811C83c603C3d850` | correct | 8.9 s |
| scam | ethereum | EWL | `0x752b6569279C353766EB7CB3058dcaCC3032c565` | correct | 7.7 s |
| scam | ethereum | Indy | `0x8F3bD752439caB48A383BFD284Cf165A019e5285` | correct | 9.0 s |
| scam | ethereum | USA | `0x1761177dfE78996D81aa0B1b0004F3E06Ce7170d` | correct | 7.0 s |

What this set showed:

- FREEDOM250 looks clean on paper (ownership renounced, LP burned) and is a honeypot anyway. Facts cannot catch it;
  the buy-then-sell simulation (M1) does. GTA6 was dropped from the set in M1: a fresh buyer can sell it (see
  [M1 results](#m1-results)).
- Uniswap v2's protocol fee switch is on: trading mints LP to the fee collector, so a pulled pool's LP supply stays
  above the 1000-wei minimum. Counted correctly, 3,511 of the 4,264 WETH pairs created in the 45 days before the
  run had their liquidity withdrawn after trading started.
- All ten scams in this run were on Ethereum; M1 added a Base honeypot (ZiG).

## M1 results

`exitcheck eval testdata/tokens.json`, run 2026-10-29 on the default public RPCs with Anvil 1.8.6: **6/6 honeypots
flagged, 0/10 legitimate tokens flagged**. The facts still hold: 20/20 tokens, 148/148 checks. A simulation took a
median of 5.0 s.

Every honeypot label now rests on a test that does not use exitcheck: `cast`, on an Anvil fork of the current chain,
buys with a fresh wallet, waits, and sells. Adding that test to the labels changed the set:

- **GTA6 was dropped.** A fresh buyer sells it without trouble. The buyers who never sold were launch-block sniper
  bots, so "only the deployer ever sold" did not prove a trap.
- **DREAM and USA are rugs, not honeypots.** On forks of blocks from their trading days, a buyer could sell. Buyers
  lost their money when the deployer removed the liquidity. Rugs carry no simulation expectation: once the pool is
  pulled there is nothing to trade, and M0 already reports the pull.
- **ZiG is the first Base scam.** It came from a scan of 30 days of new Base pools against WETH (Uniswap v2 and
  Aerodrome). Of the 34 pools that still held liquidity, the simulation called 2 honeypots. The other one (DEPH) stays
  out: its nine buyers all came within seven minutes of launch, more likely the deployer's own wallets than victims.
  The same scan over 30 days of Ethereum pairs found no honeypot whose pool still held 0.05 WETH or more.

| kind | chain | token | address | facts | facts time | simulation | sim time |
|---|---|---|---|---|---|---|---|
| legit | ethereum | USDT | `0xdAC17F958D2ee523a2206206994597C13D831ec7` | correct | 12.5 s | ok | 4.5 s |
| legit | ethereum | USDC | `0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48` | correct | 17.4 s | ok | 4.2 s |
| legit | ethereum | DAI | `0x6B175474E89094C44Da98b954EedeAC495271d0F` | correct | 6.1 s | ok | 5.7 s |
| legit | ethereum | PEPE | `0x6982508145454Ce325dDbE47a25d4ec3d2311933` | correct | 69.7 s | ok | 5.1 s |
| legit | ethereum | UNI | `0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984` | correct | 4.7 s | ok | 5.0 s |
| legit | ethereum | LINK | `0x514910771AF9Ca656af840dff83E8264EcF986CA` | correct | 8.1 s | ok | 5.0 s |
| legit | ethereum | SHIB | `0x95aD61b0a150d79219dCF64E1E6Cc01f0B64C4cE` | correct | 6.0 s | ok | 5.3 s |
| legit | base | BRETT | `0x532f27101965dd16442E59d40670FaF5eBB142E4` | correct | 7.5 s | ok | 7.1 s |
| legit | base | AERO | `0x940181a94A35A4569E4529A3CDfB74e38FD98631` | correct | 7.6 s | ok | 7.3 s |
| legit | base | DEGEN | `0x4ed4E862860beD51a9570b96d89aF5E1B0Efefed` | correct | 16.3 s | ok | 12.4 s |
| honeypot | ethereum | ASN | `0xBe2ab177599C6381C70725FEEE6663F9bCfF2197` | correct | 66.8 s | honeypot | 3.9 s |
| honeypot | ethereum | QTC | `0x541E9505Aaf0a7D2d26fe0123d0f2419cDbb3ad7` | correct | 8.0 s | honeypot | 3.6 s |
| honeypot | ethereum | FREEDOM250 | `0xD2Ab2Bf8C6e9e8070C8D9C24001d9a579620CaB2` | correct | 7.1 s | honeypot | 3.8 s |
| honeypot | ethereum | AMERICA250 | `0x5e82442d47b2e14b6F621a256DfB115DBd9f83ee` | correct | 8.1 s | honeypot | 4.8 s |
| rug | ethereum | DREAM | `0x809010Db55F12eF7E082d6e58461C58e27F1Aa9B` | correct | 6.1 s | - | - |
| rug | ethereum | ARC | `0x42Bf9A7D31534096CA80f68D811C83c603C3d850` | correct | 8.2 s | - | - |
| rug | ethereum | EWL | `0x752b6569279C353766EB7CB3058dcaCC3032c565` | correct | 6.5 s | - | - |
| honeypot | ethereum | Indy | `0x8F3bD752439caB48A383BFD284Cf165A019e5285` | correct | 8.7 s | honeypot | 5.0 s |
| honeypot | base | ZiG | `0xEF77fB35d6e0503faf3cbe9565193195Aa4c6eF8` | correct | 7.7 s | honeypot | 9.7 s |
| rug | ethereum | USA | `0x1761177dfE78996D81aa0B1b0004F3E06Ce7170d` | correct | 5.1 s | - | - |

What the honeypots show:

- **The traps differ.**
  - ASN's `sellTax()` reads 100. Nothing reaches the pool, so the router reverts.
  - QTC and Indy revert inside the token's transfer.
  - FREEDOM250 and AMERICA250 pay their fee wallet on every sale. That wallet is the deployer's own account,
    delegated by EIP-7702 to code that reverts on payment. The token's code, its renounced ownership and its burned
    LP all look clean: the trap lives in another account.
  - ZiG's verified source lets only seven hard-coded wallets sell. The three wallets that ever sold are on that list.
- **Names and owners can be faked; a sale cannot.** The function that arms ZiG's trap, `actionPair`, has a
  non-standard name and checks a hard-coded address instead of the owner. So M0 reports no privileged functions and a
  renounced owner. The simulation sees the effect however the cause is hidden.
- **Sniper bots look like victims.** Most buyers of FREEDOM250, AMERICA250 and Indy bought within two blocks of
  launch. The buys of ASN, QTC and ZiG were spread over 52, 90 and 109 blocks.

## M2 results

`exitcheck bench testdata/tokens.json`, run 2026-10-29 on the default public RPCs with Redis from `compose.yaml`.
Results: **median answer 9.1 s cold and 0.5 ms cached; 20/20 verdicts in their label's range**. Every scam comes
out at least high risk, and no legitimate token worse than caution.

The bench drives the bot's own message handler, with real RPCs, Anvil and Redis, and stands in only for Telegram. So
the times run from the message arriving to the answer being handed to Telegram; Telegram's delivery adds its own
fraction of a second. Each token is asked twice: first with its cache entries deleted (cold), then again (cached).

| label | chain | token | verdict | cold | cached |
|---|---|---|---|---|---|
| legit | ethereum | USDT | caution | 15.1 s | 0.2 ms |
| legit | ethereum | USDC | caution | 10.0 s | 0.5 ms |
| legit | ethereum | DAI | clear | 9.8 s | 0.5 ms |
| legit | ethereum | PEPE | clear | 8.6 s | 0.2 ms |
| legit | ethereum | UNI | caution | 9.3 s | 0.2 ms |
| legit | ethereum | LINK | clear | 9.4 s | 0.2 ms |
| legit | ethereum | SHIB | caution | 20.0 s | 0.5 ms |
| legit | base | BRETT | clear | 9.1 s | 0.5 ms |
| legit | base | AERO | clear | 8.1 s | 0.5 ms |
| legit | base | DEGEN | caution | 20.1 s | 0.5 ms |
| scam | ethereum | ASN | scam | 7.4 s | 0.2 ms |
| scam | ethereum | QTC | scam | 9.0 s | 0.5 ms |
| scam | ethereum | FREEDOM250 | scam | 10.2 s | 0.5 ms |
| scam | ethereum | AMERICA250 | scam | 8.7 s | 0.3 ms |
| scam | ethereum | DREAM | scam | 8.9 s | 0.5 ms |
| scam | ethereum | ARC | scam | 6.4 s | 0.5 ms |
| scam | ethereum | EWL | scam | 5.1 s | 0.5 ms |
| scam | ethereum | Indy | scam | 10.3 s | 0.5 ms |
| scam | base | ZiG | scam | 9.2 s | 0.4 ms |
| scam | ethereum | USA | scam | 8.6 s | 0.2 ms |

What the numbers show:

- **The tail comes from the free RPCs, not the work.**
  - Checked alone, SHIB takes about 5 s. In an earlier bench run it took 74 s, waiting out mevblocker's Cloudflare
    rate-limit cooldowns.
  - Facts now get a 20 s budget and the simulation 30 s. A stage still running at its deadline becomes a "not checked"
    line, so a slow answer is bounded instead of hanging. p90 was 15.1 s, max 20.1 s.
  - DEGEN hits the budget in every run. Its deployer has 1,320 transactions, and listing the deployer's other tokens
    checks each one's address through Base's RPC. That is not a decisive check, so DEGEN's answer stays complete.
- **A cut-short check is never called clear.** The verdict is at least caution, and says which check is missing, if
  any of these did not finish:
  - the simulation;
  - the power probes;
  - pool discovery or the LP supply;
  - the proxy check;
  - the owner read, or finding who deployed the token (both are needed to probe their powers);
  - for a token without a year-long record, who holds the LP.
- **Caution on legitimate tokens:**
  - USDT and USDC: owner powers (and USDC's upgradeable proxy), marked as a known trade-off on an established token.
  - UNI: a treasury holding 26%.
  - SHIB: its 41% "top holder" is SHIB's own burn address, which facts does not yet count as burned.
  - DEGEN: an owner contract that can pause.
- **Live in Telegram** (2026-10-29), with the bot's log timing each message up to Telegram accepting the reply:
  - ZiG: "Honeypot", 8.7 s cold, with its chain (Base) found by the bot;
  - PEPE: "No red flags found", 7.5 s cold;
  - PEPE again: 128 ms cached. That is mostly one round trip to Telegram's API, which the bench stubs out.
- **The verdict rules are also tested offline.** `internal/verdict/testdata` holds the recorded facts and
  simulations of all 20 tokens, so CI checks every rule change against real tokens without a network.

## M3 results

`docker compose up -d --build` ran the whole live watch on a laptop, on the default public RPCs: the watcher, two
analyzers, the bot, Kafka, Postgres and Redis. The measured day ran from 2026-10-30 01:31 to 2026-10-31 01:31 UTC,
and `exitcheck report -since 24h` read it at the end. Results: **264 launches, all analyzed; median lag 8.9 s, p95
13.3 s, 2 over a minute; 0 duplicate results and 0 duplicate alerts; 199 alerts, all posted** to the public channel
[@rugcheckApp](https://t.me/rugcheckApp). **But the watch did not see every launch:** `exitcheck audit` counts 300
launches that day from the factories' logs, and the watch missed 36 of Ethereum's 91 (none of Base's 209). The cause
is fixed ([PR #9](https://github.com/amirradjou/exitcheck/pull/9)), and a second fix keeps the watcher from falling behind Base's two-second blocks ([PR #11](https://github.com/amirradjou/exitcheck/pull/11)). **No full day
has been measured since those fixes**, so M3 stays open: what it needs is one more day read by `exitcheck report` and
`exitcheck audit`, with completeness counted.

| | |
|---|---|
| window | 2026-10-30 01:31 to 2026-10-31 01:31 UTC (24h0m0s) |
| launches | 264 (base 209, ethereum 55) |
| analyzed / failed / waiting | 264 / 0 / 0 |
| lag, liquidity → verdict | median 8.9 s, p95 13.3 s, max 70.9 s; 2 over a minute |
| of which detection | median 0.2 s, p95 3.5 s |
| check | median 8.5 s |
| redelivered by Kafka | 1 launches, 1 extra deliveries (counted, not re-analyzed) |
| duplicate results / alerts | 0 / 0 |
| verdicts | high-risk 198, caution 63, scam 2, clear 1 |
| alerts | 199 (199 posted) |

Lag runs from the block timestamp of a pool's first liquidity to its verdict stored in Postgres. Detection is the
watcher's share: up to the launch being published to Kafka. The day started after the last watcher fix (dating a
launch by the Mint that made it tradeable). The two watcher fixes above came after it.

What the numbers show:

- **The watch missed 36 of Ethereum's 91 launches, and none of Base's 209.** Counted afterwards by
  `exitcheck audit -until 2026-10-31T01:31:00Z`: every pool created that day against a quote token that became
  tradeable by the watch's own rule (it held the token, and the quote too for a v2 pair), set against the watch's
  launches. The missed ones were 22 of 59 Uniswap v2 launches and 14 of 32 v3 ones; every missed v2 pool still held
  its liquidity at the end of the block that created it. (A first count by hand said 43 of 97: it also took in seven
  v3 positions of the quote token alone, which nobody could buy from and the watch rightly left pending.)
  - The cause: for the newest block, the watcher asked the latest-state RPC for the factories' creation logs and, when
    that node was a moment behind the head (it says so with an error), fell back to the archive RPC. On Ethereum
    that is mevblocker, which answers an empty list, not an error, for a block it has not reached. The cursor then
    moved past the block, and its launches were gone. Base's archive RPC (Tenderly) errors instead, so Base lost none.
  - The fix: the newest 100 blocks of a range come from the latest-state RPC alone, and a step it refuses is tried
    again a second later. A test reproduces the loss against the old code.
  - The lag and duplicate figures below hold for the launches the watch saw.
- **The lag is the check.** The check's median, 8.5 s, is the bot's cold answer from M2. Base launches were detected
  within 0.3 s of their block at p95. Ethereum's took a median 2.7 s and p95 4.0 s, as Ethereum blocks are announced
  a few seconds after their slot's timestamp. Kafka, Postgres and the alert add a fraction of a second.
- **Two launches went over a minute, both waiting on a free RPC:**
  - 70.9 s (Base, 03:48 UTC). The first check attempt failed on a timeout from Tenderly's Base gateway. The retry,
    5 s later, finished within its budgets but without the simulation, which the verdict (high risk) lists as not
    checked.
  - 69.4 s (Ethereum, 23:43 UTC). The check took 5 s, but the watcher saw the pool 64 s after its block. The pool
    was created and funded in that same block, so the step that read the block finished a minute late. Nothing was
    logged, and steps are not timed, so which call was slow is not known. The watcher's RPC calls retry transient
    errors silently, and log reads fall back to the archive RPC's windowed scan when the latest-state RPC refuses.
    One other Ethereum launch came close the same way: 52.5 s to detection, 57.4 s of lag (08:28 UTC).
  - One more check failed its first attempt (a mevblocker timeout) and finished in 32.9 s.
- **The day's faults lost and doubled nothing:**
  - Docker's DNS stopped answering for about five minutes (03:41–03:47 UTC). The watcher could not reach Postgres to
    save its cursor, so 11 steps failed; each changed nothing and was run again at the next block. One pool was
    created during the outage (Ethereum v3, block 26302968). It was read from the backlog when the database came
    back, and its launch was detected 3 s after it was funded. Factory logs for those blocks show no other pool
    against a quote token.
  - The WebSocket subscriptions dropped 21 times (11 on Base, 10 on Ethereum), eight of them between 17:35 and
    17:46 UTC. Each was resubscribed, and the blocks in between were read by range from the saved cursor.
  - The one redelivery: Telegram reset the connection while an alert was being posted (00:15 UTC). The alert's claim
    was released and the stream handed the launch over again a second later. The analyzer found it already analyzed,
    did not check it again, and the alert was posted once.
- **The verdicts follow the venue.**
  - A deployer still holding the pool's LP tokens, and so able to withdraw the liquidity, accounts for 179 of the
    198 high-risk verdicts: 177 of the 184 Uniswap v2 launches, and 2 on Aerodrome. The other 19 rest on the deployer
    holding a large share of the supply (13), an owner who can mint (12), an upgradeable proxy (4) or a tax in the
    test trade (3), several with more than one.
  - Uniswap v3 had 76 launches, 57 judged caution. Of those, 51 were seeded with the token alone, so there was
    nothing to sell into yet and the simulation could not run (the limit noted above). An unfinished decisive check
    is never called clear.
  - 2 scams: one pool whose liquidity was already gone when the check ran, and one false honeypot. Sentient (SENT)
    on Ethereum, 03:47 UTC, right after the DNS outage: the simulation's 30 s budget ran out while it approved the
    sale, and the timeout read as a sale that reverts. Checked again on a fork of the same block (M4's backfill), the
    buy, transfer and sale all work. Its alert was posted. A simulation cut off by its budget is now reported as not
    checked, never as a verdict. 1 clear.
- **What happened next, a first look.** This is not M3's measure; M4's labelled set is. Right after the day, each
  Uniswap v2 pool was read again: its LP supply, the fee collector's share and its Burn and Swap logs.
  - Of the 177 v2 launches flagged because the deployer held the LP, 168 had their liquidity pulled by then.
  - 167 of those traded first. The median pool lasted 13.3 minutes from first liquidity to first removal (p25 10.1,
    p75 33.1), after a median 46 swaps.
  - All 167 alerts were posted before the removal, a median 13.1 minutes ahead.
  - Of the 7 other v2 launches, 3 were pulled.
  - Limits: v2 pools only, since v3 positions are NFTs and were not read. The swaps before a pull may be the
    deployer's own wallets, so they show that the pool traded, not how many people lost money.
- **Small footprint.** Idle at the end of the day, the seven containers used 814 MiB, 738 MiB of it Kafka. Each
  check's Anvil fork starts and stops inside an analyzer.

## M4 status

**Not done.** The evaluation is in place, and what it says so far is promising but not stable, so the model is not in the live
verdict. These are the numbers as of 2026-11-05. `cd ml && uv run train.py` reproduces them from
[`ml/dataset.csv.gz`](../ml/dataset.csv.gz): 1,144 launches, 72% of them pulled within a day.

| 228 test launches, 2026-10-30 to 10-31 | alerts | precision | recall | F1 |
|---|---|---|---|---|
| rules alone | 180 | 87.8% | 88.3% | 0.88 |
| model alone | 191 | 90.6% | 96.6% | 0.94 |
| rules, with the model on what they let through | 196 | 86.7% | 95.0% | 0.91 |

Against the rules alone on the same launches, the model alone gains 0.055 of F1 (95% interval +0.022 to +0.090), and the form
that would run gains 0.026 (+0.005 to +0.049). It would add 16 alerts, 12 of them right, and catch 12 of the 21 pulls the rules
missed.

Why that is not a result yet:

- **It moves too much.** A run a few hours earlier, with 95 fewer launches, gave the model alone a gain of 0.015 with an interval
  spanning zero (-0.016 to +0.045). A number that swings that far on 95 launches says the sample is too small to claim anything.
- **The folds disagree.** Moving the cut earlier, the form that would run gains nothing, then 0.03, then nothing:

  | trained on | tested on | rules: precision / recall / F1 | rules and model | alerts added |
  |---|---|---|---|---|
  | 90 | 190 | 89.6% / 94.9% / 0.92 | 89.6% / 94.9% / 0.92 | 0 |
  | 141 | 190 | 91.2% / 86.0% / 0.89 | 86.8% / 97.5% / 0.92 | 22 |
  | 282 | 190 | 85.6% / 89.7% / 0.88 | 83.4% / 93.2% / 0.88 | 10 |
- **It trained on 179 launches, all on Ethereum.** The day-long gaps use up two days, and Base's backfill so far only reaches back to
  October 29. Base's labels are also incomplete, so its 105 test launches are not all of them.
- **One rule was chosen after looking.** The threshold for what the rules let through replaced one that added no alerts at all, and
  was picked after seeing a test table. The next evaluation is what tests it.

Where it looks most useful is Uniswap v3, whose positions the rules cannot read: on 61 test launches the rules catch 20% of the pulls
at 33% precision, the model alone 80% at 69%. That is also where the data shifts most. Base v3 pools were pulled at 14% from October 16
to 19 and at 0% (0 of 46) from October 29. Read straight from the pools' logs, that drop is real, not a labelling error.

What is left: finish the two-week backfill (Ethereum back to October 16, Base from October 29 back to October 20), label it all, and
evaluate once more, with launches from after the training data as the test. M4 is done if the model then beats the rules across the
folds, and this section will say so either way.

## M5 results

`exitcheck explain -review` on 38 reports, 2026-11-05: the 20 labelled tokens and 18 launches the live watch saw, a spread of chains, venues
and verdicts. With `qwen/qwen3.8-27b` on Groq's free tier: **all 58 sentences that reached a reader cite a check or a source line**, and
reading them found none the checks contradict. All of them, with what each cites and what was dropped, are in
[docs/m5-review.md](m5-review.md).

For a honeypot on Base, the answer ends like this:

```
Why, in plain words:
A test buy succeeded but the sale failed, meaning buyers cannot get their money out. [1][2]
Some decisive checks did not finish, so the token cannot be called free of red flags. [3][4]

[1] check: A test buy worked but the sale failed (selling reverts: TransferHelper: TRANSFER_FROM_FAI…
[2] known pattern: Honeypot
[3] check: Some decisive checks did not finish (LP holders), so this is not a clean bill of health.
[4] known pattern: Unfinished checks

Written by a language model from the checks above. It did not decide the verdict.
```

How the same evidence and the same check treat different models. A model that stays close to its evidence keeps more; one that rewrites freely
loses more, which is the check doing its job.

| model | sentences written | kept | citing source lines | reports left without an explanation |
|---|---|---|---|---|
| `qwen/qwen3.8-27b` (Groq free tier) | 76 | 58 | 1 | 5 of 33 |
| Claude Haiku 5.5 | 85 | 79 | 6 | 3 of 33 |
| `openai/gpt-oss-120b` (Groq free tier) | 88 | 25 | 2 | 15 of 33 |
| `qwen2.5:7b` (local, Ollama) | 100 | 15 | 1 | 19 of 33 |

What the numbers show:

- **Citing is enforced, so it proves little by itself.** `Ground` keeps a sentence only if it cites a check or a source line, which makes "every
  sentence cites" true by construction. What tests it is reading them, and the tests below came from doing that. Of the 58 sentences, none says
  something the checks contradict, and two overstate: "faces a high chance of having its liquidity pulled" where the pattern says "many launches",
  and "would collapse the price" where it says "can".
- **Reading found what the counts hid**, on earlier runs:
  - A denylist entry matched "no real market", a phrase from the pattern library itself, and silently removed the most serious finding from 16 reports.
  - A realistic example sentence in the prompt was copied onto every token. The format is now described, not shown.
  - "The deployer holds 50% of the supply and could withdraw all the liquidity" has every word from one of two findings and the claim from neither.
    A sentence now rests on one finding.
  - "The owner can create unlimited new tokens" shares nearly every word with its finding. Words that inflate a claim must now be in the evidence.
  - "The owner holds 99.9% of the LP" was wrong on two tokens, because the verdict said "the deployer or owner". The fault was the verdict's: it now
    says who.
- **Five reports get no explanation, and that is the designed outcome.** USDT, USDC and DEGEN are among them: their powers are a known trade-off, so
  no warning is offered, and what the model adds beyond that does not pass. The verdict goes out alone.
- **Few sentences cite source lines.** Most launches are not verified when they launch, and this model prefers the checks. A stronger model cites more.
