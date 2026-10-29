# rugcheck

Paste a token address, get a straight answer: is this token built to take your money? A Telegram bot that gives a
risk verdict with the reasons behind it, plus a live watch of new token launches.

> **Status: M0 (contract facts) built.** This README is the spec. The milestones at the end track what is built.

## Try it

```sh
make build
./bin/rugcheck facts 0x6982508145454Ce325dDbE47a25d4ec3d2311933            # PEPE on Ethereum
./bin/rugcheck facts -chain base 0x532f27101965dd16442E59d40670FaF5eBB142E4  # BRETT on Base
./bin/rugcheck facts -json <address>                                        # machine-readable
./bin/rugcheck eval testdata/tokens.json                                    # the labelled set
```

No keys needed: it runs on free public RPCs. `ETHERSCAN_API_KEY` is optional (see `.env.example`). A report covers
the contract (verified, proxy, owner, deployment), every privileged function and who can use it right now, current
fee and limit settings, holder concentration, every pool and whether its liquidity is burned, locked or pulled, and
the deployer's other tokens. Whatever could not be checked is listed under NOT CHECKED with the reason.

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

An event-driven pipeline: new-pool events from the Uniswap v2/v3 factories (and Base's main DEXes) stream from a WebSocket RPC into Kafka;
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

## Stack

- Go: RPC and explorer clients, Anvil fork simulation, analyzer workers, Telegram bot, HTTP API
- Foundry/Anvil for forks; Python (uv, scikit-learn/XGBoost, MLflow) for the model, served as features → score
- Kafka for the launch stream, Postgres for results, Redis for the verdict cache; `compose.yaml` for local runs
- Sources: a WebSocket RPC (free tier), Etherscan/Basescan APIs (free key, rate-limited) — respect each one's limits

## Milestones

| | Milestone | Done when |
|---|---|---|
| M0 ✅ | `rugcheck facts <address>`: contract, owner powers, holders, LP state | correct on 20 hand-picked tokens (10 known scams, 10 legitimate) — **20/20, see [M0 results](#m0-results)** |
| M1 | Buy-then-sell simulation on an Anvil fork | every known honeypot in the set flagged; no legitimate token flagged |
| M2 | Telegram bot: verdict + reasons, cached in Redis | median answer under 10 s cold, instant when cached |
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

- GTA6 and FREEDOM250 look clean on paper (ownership renounced, LP burned) and are honeypots anyway. Facts cannot
  catch them; the buy-then-sell simulation (M1) has to.
- Uniswap v2's protocol fee switch is on: trading mints LP to the fee collector, so a pulled pool's LP supply stays
  above the 1000-wei minimum. Counted correctly, 3,511 of the 4,264 WETH pairs created in the 45 days before the
  run had their liquidity withdrawn after trading started.
- All ten scams are on Ethereum; Base scams join the set with M1.
