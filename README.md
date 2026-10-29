# rugcheck

Paste a token address, get a straight answer: is this token built to take your money? A Telegram bot that gives a
risk verdict with the reasons behind it, plus a live watch of new token launches.

> **Status: M0 (contract facts), M1 (buy-then-sell simulation) and M2 (Telegram bot) built.** This README is the
> spec. The milestones at the end track what is built.

## Try it

```sh
make build
./bin/rugcheck facts 0x6982508145454Ce325dDbE47a25d4ec3d2311933            # PEPE on Ethereum
./bin/rugcheck facts -chain base 0x532f27101965dd16442E59d40670FaF5eBB142E4  # BRETT on Base
./bin/rugcheck facts -json <address>                                        # machine-readable
./bin/rugcheck sim 0x6982508145454Ce325dDbE47a25d4ec3d2311933              # buy then sell on a fork
./bin/rugcheck sim -chain base 0x940181a94A35A4569E4529A3CDfB74e38FD98631  # AERO, through Aerodrome
./bin/rugcheck check 0xEF77fB35d6e0503faf3cbe9565193195Aa4c6eF8            # the verdict, as the bot gives it
./bin/rugcheck eval testdata/tokens.json                                    # the labelled set
make bot                                                                    # the Telegram bot (TELEGRAM_BOT_TOKEN in .env)
```

No keys needed: it runs on free public RPCs. `ETHERSCAN_API_KEY` is optional (see `.env.example`). A report covers
the contract (verified, proxy, owner, deployment), every privileged function and who can use it right now, current
fee and limit settings, holder concentration, every pool and whether its liquidity is burned, locked or pulled, and
the deployer's other tokens. Whatever could not be checked is listed under NOT CHECKED with the reason.

`sim` needs Foundry's `anvil` (on PATH or in `~/.foundry/bin`; see [getfoundry.sh](https://getfoundry.sh)). It
reports the venue, each step's outcome and tax, and a verdict.

## Why

Most new tokens and "airdrops" are traps: honeypots you can buy but never sell, hidden mint functions, 99% sell
taxes switched on after launch, liquidity the deployer can pull. The signs are on chain and in the contract — they
just take an expert ten minutes to check. rugcheck does it in seconds and explains what it found in plain words.

Not financial advice, never holds funds, never trades. Every simulation runs on a local fork with throwaway accounts.

## What it needs to do

### 1. Contract facts (Ethereum mainnet and Base first)

- Verified source or not (block-explorer API), proxy / upgradeable, owner renounced or not.
- Owner powers: mint, pause, blacklist, max-transaction and fee setters, trading switches, and the current fee values.
- Holders: top-holder concentration, deployer share, LP tokens burned or locked (and until when), pool age and size.
- Deployer history: other tokens the same address launched, and what happened to their liquidity.

### 2. Buy-then-sell simulation

On an Anvil fork of the latest block: buy through the pool's router with a funded test account, then try to sell.
Measures buy and sell tax, whether the sell reverts, and transfer limits. Known blind spot, stated in every verdict:
a contract can behave differently for later buyers (time switches, allowlists), so a clean simulation is not a
guarantee.

### 3. Verdict

Hard rules first (sell reverts → honeypot; owner can mint without limit → high risk), then a classifier over the
features above for the grey zone. The verdict and its score come from the checks and the model — never from an LLM.

### 4. "Why" in plain words

The LLM only explains, through retrieval-augmented generation (RAG): it gets the decisive checks plus the relevant
source lines (retrieved from the verified source and a small library of known scam patterns) and writes two or three sentences that cite them. If it cannot ground a
sentence in a check or a line of code, the sentence is dropped.

### 5. Live watch (event-driven)

An event-driven pipeline: new-pool events from the Uniswap v2/v3 factories (and Base's main DEXes), followed over a WebSocket RPC until each pool's first liquidity, stream into Kafka;
analyzer workers check each new token and post high-risk launches to an alerts channel. Analyzers are independent consumers in one Kafka consumer group, so adding
workers scales the watch and a crashed worker's partitions move to the others; results land in Postgres. Caching:
Redis holds recent verdicts so a repeated address answers instantly.

### 6. Dataset and model

A labelled set: known honeypots and rugs from public scam reports, and tokens that are clearly legitimate (listed on
major exchanges, long-lived liquidity). A machine-learning classifier (gradient-boosted trees, scikit-learn/XGBoost) over the contract, holder and simulation
features, tracked in MLflow. Evaluated on a **time split** — trained on older launches, tested on newer ones — with precision and
recall at the alert threshold published in this README.

## How the facts are checked (M0)

- **Privileged functions** come from the bytecode, so unverified contracts are covered: the dispatcher pushes one
  4-byte selector per external function, matched against the names token templates use (mint, blacklist, fee and
  limit setters, trading switches, pause, upgrade). Verified source adds functions under non-standard names.
- **Who can use them** is simulated, not guessed from modifiers: each function is run twice in one
  `eth_simulateV1` block as the owner, the deployer and a random account, once with harmless arguments and once with
  switches flipped and a large amount. A call had an effect if it emitted an event, moved the supply or a balance,
  or if the identical repeat fails or costs different gas (two identical calls on unchanged state cost exactly the
  same). That catches contracts that accept a privileged call from anyone and silently ignore it.
- **Deployment**: the creation block is found by searching archive state for the contract's nonce (EIP-161 sets it
  to 1 at creation), then the block's receipts give the transaction and the deployer.
- **Deployer history** needs no indexer: a contract an account creates directly lives at an address derived from
  the account and its nonce, so checking those addresses for code lists its other launches. A Uniswap v2 pool counts
  as pulled when the LP outside the protocol's fee collector and the 1000-wei minimum locked at the first deposit
  is under 1% of supply.
- **Holders**: Blockscout's index where it answers, otherwise a replay of Transfer logs from the creation block;
  either way, balances are re-read with `balanceOf` (reflection tokens change balances without Transfer events).
- **Liquidity**: pools against WETH, USDC and USDT on Uniswap v2/v3 (and Aerodrome on Base), valued in dollars; for
  the main pool, LP holders split into burned, locked (UNCX, Team Finance, PinkLock, with unlock dates) and
  withdrawable.

Limits, stated in every report where they apply: Uniswap v4 pools are not found yet; v3 positions are NFTs, so their
lock state is not checked; deployer history misses tokens launched through factories; on Base, holder lists for tokens
older than a few days are skipped (Base's explorer blocks scripted clients and replaying years of logs through a
free RPC is too slow).

## How the simulation works (M1)

`rugcheck sim` starts Anvil on a fork of the latest block and trades the token the way a new buyer would:

1. **Pool**: the deepest pool against WETH, USDC or USDT. A Uniswap v2 or Aerodrome pool beats a v3 one if it holds
   at least a twentieth of the deepest pool's value. v3's router rejects tokens that take a fee as they move, which
   would read as a honeypot when it is only a tax. USDC and USDT pools are reached through WETH.
2. **Buy** from a fresh random wallet with 0.5% of the pool's reserve, at most 0.05 ETH. That is small enough to
   stay under per-wallet and per-transaction limits.
3. **Wait** one minute and three blocks, so cooldowns and same-block anti-bot rules pass as they would for a real
   buyer.
4. **Transfer** half to a second wallet, then undo it, to catch tokens that block wallet-to-wallet moves.
5. **Sell** everything back through the same router; if that reverts, sell half.

A step's tax is 1 − received / expected, where expected is the DEX's quote for a swap and the amount sent for a
transfer. Under 0.5% counts as rounding.

| verdict | when | flagged |
|---|---|---|
| honeypot | selling reverts, whole and half; a sale keeps less than half its value; or a buy delivers no tokens | yes |
| sell-limited | the whole position cannot be sold, half can | yes |
| transfer-blocked | tokens cannot be sent to another wallet | yes |
| high-tax | a buy, sell or transfer tax of 10% or more | yes |
| ok | none of the above | no |
| cannot-buy | buying reverts (trading not open yet, or new buyers blocked): nobody can be trapped right now | no |
| no-pool | no pool with liquidity, or the DEX cannot quote a buy | no |

If the fork itself fails (the RPC it forks from refuses or times out), the run ends with an error, not a verdict.
Otherwise a failed state fetch would look like a reverting sell. On Base, forks come from Tenderly's gateway at a
capped rate, because publicnode's Base endpoint refuses the historical-state requests a fork makes. On the eval set a
run took 3.6–12.4 s (median 5.0 s).

**Blind spot**, printed with every result: a contract can treat later buyers differently. It may flip trading
switches after launch, use allowlists or time- and block-gated rules, or change its limits. A clean run is not a
guarantee: it answers "could a buyer sell right now". The contract facts cover what can still change (who can set
fees, blacklist or pause).

## How the verdict and the bot work (M2)

`rugcheck check <address>` and the Telegram bot give the same answer. Facts and the simulation run side by side,
because an answer has to come back in seconds; one after the other they take about 13 s at the median. Fixed rules
then turn the results into one of four levels. The level never comes from a language model.

| level | when |
|---|---|
| 🔴 scam | the test sale fails or the token keeps most of it (honeypot), or the liquidity was pulled |
| 🟠 high risk | a tax of 10% or more; sales capped or transfers blocked; a live power that can trap holders (mint, blacklist, pause, fees, trading switch, seize, upgrade, reclaim ownership) held by the owner, the deployer or anyone; the deployer or owner holds half the LP; the deployer pulled liquidity before |
| 🟡 caution | a tax under 10%; unpublished source; a pool younger than three days; under $10k of liquidity; one wallet with 20% of the supply; buying blocked; no pool; the high-risk powers above on an *established* token; a decisive check that did not finish |
| 🟢 no red flags | none of the above |

An established token has been on chain for a year and keeps at least $100k in its pools. USDT's owner can mint,
blacklist and pause, and has been able to for years. On USDT that is a known trade-off. On a two-day-old token it is
a trap waiting to be sprung. Each answer lists its reasons from most to least severe, then what could not be checked.

The bot:

- **Finding the chain.** Send it an address, adding "base" to pick the chain. Otherwise it uses the chain where the
  address is a contract. It replies "Checking…" and edits the answer in.
- **Caching.**
  - Answers are cached in Redis for 10 minutes, and so is the chain lookup, so a repeated address needs no RPC call.
  - An answer whose decisive checks did not all finish is kept for one minute only, so a retry soon gets a full
    check.
  - Concurrent requests for one token share one check.
  - If Redis is down, the bot answers without it. It leaves Redis alone for 30 s after a failure, so a hung Redis
    costs one answer a quarter of a second at most.
- **Rationing.** Each check starts an Anvil fork and makes dozens of calls to free RPCs, so checks are rationed:
  - two at a time across all chats (the rest queue, and are told so);
  - one at a time per chat;
  - ten uncached checks per chat per ten minutes.
- **Safe output.**
  - Answers are plain text. Names and revert reasons come from the token's author, so they lose control characters
    and can't carry formatting.
  - The bot token is part of every Telegram API URL, so no error message carries a URL.

## How the live watch works (M3)

```sh
docker compose up -d --build      # Kafka, Postgres, Redis, the watcher, two analyzers and the bot
./bin/rugcheck report -since 24h  # lag, duplicates and verdicts so far
```

```
WebSocket newHeads ─▶ watcher ──(launches, keyed by token)──▶ Kafka ──▶ analyzers (consumer group) ──▶ Postgres
                        │  cursor + pending pools in Postgres            │  same check and Redis cache as the bot
                        └─ per block: creation logs + one Multicall3      └─▶ alerts channel (one per token)
```

- **A launch is a pool's first liquidity.** The watcher follows new blocks over a WebSocket subscription. Per block
  range it makes two calls, however many pools are waiting:
  - the factories' creation logs (Uniswap v2 and v3, and Aerodrome on Base) for pools against WETH, USDC or USDT;
  - one Multicall3 of the waiting pools' balances.

  Funding is read from state, not from Mint logs, for two reasons. Free RPCs refuse log queries without an address
  and cap an address list at ten. And state can't be missed when a subscription drops. A funded pool is dated by its
  first Mint log, so lag is measured from when trading became possible. Pools never funded cost nothing downstream;
  they expire after a day.
- **Nothing is lost.** The watcher saves its block cursor and waiting pools to Postgres after each range, and
  publishes before moving the cursor. A crash re-publishes; it never skips. The analyzers commit Kafka offsets only
  after a launch is stored, so a crashed analyzer's partitions are replayed by the others.
- **Nothing is doubled.**
  - The producer is idempotent.
  - A launch is one Postgres row per pool, so a redelivery only increments a counter.
  - An alert is one row per token, claimed before it is sent. If posting fails, the claim is released and the stream
    retries until the alert is posted.
  - Launches are keyed by token, so a token's pools stay on one partition, in order.
  - `rugcheck report` counts duplicates rather than assuming there are none.
- **Analyzers scale by partition.** The topic has six partitions; `--scale analyzer=N` adds workers up to that, and
  Kafka moves a stopped worker's partitions to the rest. Each check runs the bot's own rules through the same Redis
  cache, so a token someone asks the bot about right after launch is answered from it.
- **Alerts** go to the Telegram channel in `TELEGRAM_ALERTS_CHAT` (the bot must be an admin there): every launch
  judged high risk or scam, once per token.

Limits:
- Uniswap v4 pools are not watched.
- A v3 pool funded with the token alone counts as launched, though the simulation needs quote liquidity to trade.
- Reorgs are not undone; a launch in a dropped block keeps its row.

## Stack

- Go: RPC and explorer clients, Anvil fork simulation, analyzer workers, Telegram bot, HTTP API
- Foundry/Anvil for forks; Python (uv, scikit-learn/XGBoost, MLflow) for the model, served as features → score
- Kafka for the launch stream, Postgres for results, Redis for the verdict cache; `compose.yaml` for local runs
- Sources: a WebSocket RPC (free tier), Etherscan/Basescan APIs (free key, rate-limited) — respect each one's limits

## Milestones

| | Milestone | Done when |
|---|---|---|
| M0 ✅ | `rugcheck facts <address>`: contract, owner powers, holders, LP state | correct on 20 hand-picked tokens (10 known scams, 10 legitimate) — **20/20, see [M0 results](#m0-results)** |
| M1 ✅ | Buy-then-sell simulation on an Anvil fork | every known honeypot in the set flagged; no legitimate token flagged — **6/6 flagged, 0/10 legitimate flagged, see [M1 results](#m1-results)** |
| M2 ✅ | Telegram bot: verdict + reasons, cached in Redis | median answer under 10 s cold, instant when cached — **9.1 s cold, 0.5 ms cached, see [M2 results](#m2-results)** |
| M3 | Live watch: factory events → Kafka → analyzers → alerts channel | a day of launches processed with lag under a minute and no duplicates |
| M4 | Labelled dataset, features, model, MLflow | time-split precision/recall published; beats the hard rules alone |
| M5 | Grounded "why" explanations | on a review set, every sentence cites a check or a source line |
| M6 | Public demo and results | README shows tokens analysed, precision at the alert threshold, median time to verdict |

## M0 results

`rugcheck eval testdata/tokens.json`, run 2026-10-29 on the default public RPCs: **20/20 tokens correct, 150/150
checks passed**, median 7.4 s per token.

Each token in [`testdata/tokens.json`](testdata/tokens.json) carries the evidence for its label and only the facts
that were established without rugcheck: deployer and creation transaction from Blockscout and `cast`, verification
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

`rugcheck eval testdata/tokens.json`, run 2026-10-29 on the default public RPCs with Anvil 1.8.6: **6/6 honeypots
flagged, 0/10 legitimate tokens flagged**. The facts still hold: 20/20 tokens, 148/148 checks. A simulation took a
median of 5.0 s.

Every honeypot label now rests on a test that does not use rugcheck: `cast`, on an Anvil fork of the current chain,
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

`rugcheck bench testdata/tokens.json`, run 2026-10-29 on the default public RPCs with Redis from `compose.yaml`.
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
