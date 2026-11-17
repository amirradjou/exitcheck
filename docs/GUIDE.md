# exitcheck guide

How each part works, every command, and the limits of each check. Measured results are in [RESULTS.md](RESULTS.md); the short version is the [README](../README.md).

## What it does

Most new tokens are traps: honeypots you can buy but never sell, hidden mint functions, 99% sell taxes switched on after
launch, liquidity the deployer can pull. The signs are on chain and in the contract, but they take an expert ten minutes to
check.

1. **Contract facts.** Verified source, proxy, owner. Every privileged function and who can use it right now, tested by
   simulating the call rather than trusting its name. Fees and limits as set now. Holder concentration, every pool, whether
   its liquidity is burned, locked or withdrawable, and the deployer's other tokens.
2. **Buy-then-sell simulation.** On an Anvil fork of the latest block: buy, wait, transfer, sell. Measures taxes, failed
   sales and transfer limits.
3. **Verdict.** Fixed rules give one of four levels and the reasons for it. A gradient-boosted classifier, trained on what past
   launches did next, is being evaluated against them. It joins the verdict only if it beats them, and [so far it has not
   shown that](RESULTS.md#m4-status).
4. **"Why" in plain words.** With a model configured, a language model explains the verdict through retrieval-augmented
   generation, citing the checks and source lines behind it. Any sentence it cannot ground is dropped.
5. **Live watch.** New pools on the Uniswap v2/v3 factories and Aerodrome stream through Kafka to analyzer workers, which
   check each launch and post the dangerous ones to an alerts channel.

Whatever could not be checked is listed under "Not checked" with the reason, and a check that did not finish is never read as
a pass. Known blind spot, stated in every answer: a contract can treat later buyers differently, so a clean result is not a
guarantee.

## Try it

You need [Go 1.26](https://go.dev/dl/) and, for the simulation, [Foundry](https://getfoundry.sh)'s `anvil`. No keys: it runs on
free public RPCs.

```sh
make build
./bin/exitcheck check 0xEF77fB35d6e0503faf3cbe9565193195Aa4c6eF8            # the verdict, as the bot gives it (ZiG, a Base honeypot)
./bin/exitcheck facts 0x6982508145454Ce325dDbE47a25d4ec3d2311933            # the full report (PEPE on Ethereum)
./bin/exitcheck facts -chain base 0x532f27101965dd16442E59d40670FaF5eBB142E4  # BRETT on Base
./bin/exitcheck facts -json <address>                                        # machine-readable
./bin/exitcheck sim 0x6982508145454Ce325dDbE47a25d4ec3d2311933              # buy then sell on a fork
./bin/exitcheck eval testdata/tokens.json                                    # the labelled set
make bot                                                                    # your own Telegram bot (TELEGRAM_BOT_TOKEN in .env)
docker compose up -d --build                                                # the whole live watch (see M3)
```

`ETHERSCAN_API_KEY` is optional, and every other setting is in [`.env.example`](../.env.example). `sim` looks for `anvil` on your
PATH and in `~/.foundry/bin`.

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

`exitcheck sim` starts Anvil on a fork of the latest block and trades the token the way a new buyer would:

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

`exitcheck check <address>` and the Telegram bot give the same answer. Facts and the simulation run side by side,
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
./bin/exitcheck report -since 24h  # lag, duplicates and verdicts so far
./bin/exitcheck audit -since 24h   # launches the watch missed, counted from the factories' logs
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
  and cap an address list at ten. And state can't be missed when a subscription drops. Balances are read at the
  processed block, not "latest": behind a load balancer, "latest" can come from a node a block behind. A funded pool
  is dated by the first Mint after the last block it was seen unfunded. So lag runs from when trading became
  possible, and a pool that first got only a quote-side position isn't dated from that. Pools never funded cost
  nothing downstream; they expire after a day.
- **Nothing is lost.** The watcher saves its block cursor and waiting pools to Postgres after each range, and
  publishes before moving the cursor. A crash re-publishes; it never skips. The analyzers commit Kafka offsets only
  after a launch is stored, so a crashed analyzer's partitions are replayed by the others. The newest 100 blocks of a
  range are read from the latest-state RPC alone, which errors for a block it does not have yet; a refused step is
  tried again a second later. An archive RPC can answer "no logs" for a block it has not reached, and that silently
  lost launches during M3's measured day (see [M3 results](RESULTS.md#m3-results)). A step has a time budget: a stalled RPC
  connection otherwise holds up every launch behind it. A step that overruns is tried again on a new connection, and
  the last attempt's budget is long enough for a catch-up that honestly needs the time. Slow steps and calls are logged by name.
  A late watcher goes straight to the newest head, reading the whole range in one step: taken one at a time, heads arrive
  faster than a slow step clears them, and the watcher never gets back to the present.
- **Nothing is doubled.**
  - The producer is idempotent.
  - A launch is one Postgres row per pool, so a redelivery only increments a counter.
  - An alert is one row per token, claimed before it is sent. If posting fails, the claim is released and the stream
    retries until the alert is posted.
  - Launches are keyed by token, so a token's pools stay on one partition, in order.
  - `exitcheck report` counts duplicates rather than assuming there are none.
- **Analyzers scale by partition.** The topic has six partitions; `--scale analyzer=N` adds workers up to that, and
  Kafka moves a stopped worker's partitions to the rest. Each check runs the bot's own rules through the same Redis
  cache, so a token someone asks the bot about right after launch is answered from it.
- **Alerts** go to the Telegram channel in `TELEGRAM_ALERTS_CHAT` (the bot must be an admin there): every launch
  judged high risk or scam, once per token. A launch more than 30 minutes old is stored but not posted: after an outage
  the watcher reads every block it missed, and a channel should not get a day of old launches at once.
- **A reboot brings it all back.** Every service restarts on its own, and the watcher resumes from its saved cursor.

Limits:
- Uniswap v4 pools are not watched.
- A v3 pool funded with the token alone counts as launched, though the simulation needs quote liquidity to trade.
- Reorgs are not undone; a launch in a dropped block keeps its row.

## How the dataset and model work (M4)

```sh
./bin/exitcheck backfill -watch -from 2026-10-16 -to 2026-10-29T23:20:00Z  # past launches, checked as they stood
./bin/exitcheck label -checked                                             # what happened in the day after each
./bin/exitcheck dataset                                                    # ml/data/dataset.csv
cd ml && uv run train.py                                                  # time split, model vs rules, MLflow
```

- **A label is what the chain did, not a list of scams.** `exitcheck label` reads each launch's pool logs, from the
  pool's creation to 24 h after its first liquidity.
  - A Uniswap v2 or Aerodrome pool is its own LP token. Its Transfer logs give the LP supply and the fee collector's
    share at every point, and the pool counts as pulled under the rule facts use: LP outside the fee collector and
    the permanent minimum is under 1% of supply.
  - A v3 pool's positions are NFTs, so its liquidity is summed from Mint and Burn instead.
  - Pulled means the liquidity fell under the threshold at any point within the day. A deployer who empties a pool
    and refills it later has still taken buyers' money once.
  - Buys and sells before the pull are counted. A pull before any trade is bait: nobody lost money.
- **Features are what the check saw at the launch's block.**
  - The live watch keeps each check's full report (`launches.report`).
  - Launches from before that, and older ones, are checked again by `exitcheck backfill`. The unchanged check runs
    against an Anvil fork of the launch's block, where that block is the latest state, so facts and simulation see
    it as it stood then. Ages are measured from the checked block's timestamp, and explorers are offline: they only
    know the present.
  - Re-checked this way, 331 of the 338 launches the live watch had already judged got the same verdict (97.9%).
    One of the seven others was a false honeypot that the live check got wrong (a simulation cut off by its budget).
- **The same features, live or backfilled.** `internal/features` turns a report into 65 numbers: contract and owner,
  privileged functions found in the bytecode and who can use them now, holdings, pools and LP shares, the deployer's
  history, the simulation's steps and taxes. Explorer answers are left out (verified source, powers found only in
  source, Blockscout's holder list): a backfill cannot have them, and a live launch is usually too new to be indexed.
  A value the check could not read stays missing.
- **Time split.** `ml/train.py` (XGBoost, tracked in a local MLflow store) trains on the oldest launches, picks its thresholds
  on the next ones and tests on the newest, so the model is judged on launches it could not have seen. A label looks a day past
  its launch, so each part stops a full day before the next begins: without that gap, the last day of training data holds what
  happened after the cut. The whole procedure is repeated with the cut moved earlier (walk-forward), since one split can be
  lucky, and differences from the rules come with a bootstrap interval over the same test launches. Results are also split by
  venue and chain: the rules catch nearly every v2 pull (the deployer holds the LP tokens) and miss most v3 ones (positions are
  NFTs, so who holds them is not checked).
- **The rules first, the model where they are silent.** That is the form it would run in, and the one that counts. It could raise
  a launch the rules let through to high risk and could never lower anything. Lowering would be wrong even when the model is
  right about the day: an owner who can mint and has not yet done so has not stopped being dangerous because the pool survived
  24 hours. So the only choice is how much precision to trade for catching more pulls. The threshold is the one with the best
  combined F1 on the threshold launches, among those whose added alerts are right more often than not.
- **Not in the live verdict.** Not until it clearly beats the rules: see [M4 status](RESULTS.md#m4-status).

Limits:
- The label is liquidity pulled within a day. A honeypot that keeps its liquidity, with buyers unable to sell, is not
  counted as one.
- Backfill takes launches that became tradeable in the block that created their pool, which is most of them (206 of 210 on
  Base and 42 of 55 on Ethereum on M3's measured day). Pools funded later are only in the live data.
- A few weeks of launches teach the model those weeks' scam templates, and who launches changes quickly (see below).

## How the explanations work (M5)

```sh
EXITCHECK_LLM_URL=https://api.groq.com/openai/v1 EXITCHECK_LLM_KEY=… EXITCHECK_LLM_MODEL=qwen/qwen3.8-27b ./bin/exitcheck explain <address>
./bin/exitcheck explain -review -out review.md   # the review set
```

The verdict is already decided when the model is called. The model explains it in plain words and can change nothing.
Without a model configured, the bot answers exactly as before. It works with any OpenAI-compatible endpoint: Ollama on the
same machine, or a free tier. exitcheck needs no paid key.

- **Retrieval.** The model gets three kinds of evidence, each with an ID it must cite:
  - `C`: the findings behind the verdict, in the rules' own words. Reassurances stay out: the rules have weighed them, and a
    model handed "ownership is renounced" next to a failed sale writes that the token looks fine.
  - `S`: lines of the verified source, found by BM25 over the contract's functions. The query is the privileged functions
    someone can use now and the path a failed sale takes, nothing else. Asked about a wallet holding 40% of the supply,
    source search returns whatever mentions balances, and a model cites it as if it mattered.
  - `P`: entries from a small [library of known scam patterns](../internal/explain/patterns.json), one per finding, which say
    why a finding matters to a buyer.
- **Grounding.** Nothing the model writes reaches a reader without passing [`Ground`](../internal/explain/ground.go), a
  deterministic check. A sentence is dropped if it:
  - cites nothing, cites an ID that does not exist, or cites only the pattern library, which says nothing about this token;
  - joins two findings. Every word of "the deployer holds 50% of the supply and could withdraw the liquidity" comes from one
    of two findings; the claim comes from neither;
  - has a number, link or address its citations do not have;
  - takes under two thirds of its content words from what it cites, or does not state the finding it cites;
  - leans on the sentence before it ("That can collapse the price"), which may have been dropped;
  - inflates its evidence ("unlimited", "highly likely") with words the evidence does not use;
  - reassures or advises ("safe", "no obvious issues", "should buy"), whatever it cites.

  A source citation that shares fewer than two words with its sentence is removed: it was added for show.
- **Hostile input.** A token's name, its revert messages and its source are written by the token's author, who may be writing
  for the model. Comments and strings are blanked before source is retrieved, square brackets are stripped so a revert message
  cannot forge an evidence line, a token's name cannot ground a claim, and the prompt says the evidence is data. The real defence is
  the verdict being out of the model's reach and every sentence passing `Ground`. Telegram turns "evil.xyz" in a token's name into a live
  link, and these answers go to a public channel, so anything author-written is defanged when rendered.
- **In the bot.** The verdict goes out as soon as it exists and the explanation is edited in afterwards, with a 30 s budget. A
  slow or failing model costs the explanation and nothing else. A clear verdict is not put to the model: a sentence about a
  token with no red flags reads as a recommendation.

Limits:
- `Ground` checks that a sentence stays close to what it cites. It does not understand the sentence: a claim made entirely of
  words from its citation can still be wrong. Hence the review below, read by hand.
- Most tokens are not verified when they launch, so most explanations of launches cite checks and patterns only.

## Stack

- Go: RPC and explorer clients, Anvil fork simulation, analyzer workers, Telegram bot, the model's scorer
- Foundry/Anvil for forks; Python (uv, XGBoost, MLflow) to train the model
- Kafka for the launch stream, Postgres for results, Redis for the verdict cache; `compose.yaml` runs all of it
- Sources: free public RPCs over HTTP and WebSocket, Sourcify and Blockscout (keyless), Etherscan (optional key)
