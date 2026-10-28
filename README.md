# rugcheck

Paste a token address, get a straight answer: is this token built to take your money? A Telegram bot that gives a
risk verdict with the reasons behind it, plus a live watch of new token launches.

> **Status: scaffold.** This README is the spec. The milestones at the end track what is built.

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

The LLM only explains: it gets the decisive checks plus the relevant source lines (retrieved from the verified source
and a small library of known scam patterns) and writes two or three sentences that cite them. If it cannot ground a
sentence in a check or a line of code, the sentence is dropped.

### 5. Live watch

New-pool events from the Uniswap v2/v3 factories (and Base's main DEXes) stream from a WebSocket RPC into Kafka;
analyzer workers check each new token and post high-risk launches to an alerts channel. Results land in Postgres;
Redis caches verdicts so a repeated address answers instantly.

### 6. Dataset and model

A labelled set: known honeypots and rugs from public scam reports, and tokens that are clearly legitimate (listed on
major exchanges, long-lived liquidity). Gradient-boosted model over the contract, holder and simulation features,
tracked in MLflow. Evaluated on a **time split** — trained on older launches, tested on newer ones — with precision and
recall at the alert threshold published in this README.

## Stack

- Go: RPC and explorer clients, Anvil fork simulation, analyzer workers, Telegram bot, HTTP API
- Foundry/Anvil for forks; Python (uv, scikit-learn/XGBoost, MLflow) for the model, served as features → score
- Kafka for the launch stream, Postgres for results, Redis for the verdict cache; `compose.yaml` for local runs
- Sources: a WebSocket RPC (free tier), Etherscan/Basescan APIs (free key, rate-limited) — respect each one's limits

## Milestones

| | Milestone | Done when |
|---|---|---|
| M0 | `rugcheck facts <address>`: contract, owner powers, holders, LP state | correct on 20 hand-picked tokens (10 known scams, 10 legitimate) |
| M1 | Buy-then-sell simulation on an Anvil fork | every known honeypot in the set flagged; no legitimate token flagged |
| M2 | Telegram bot: verdict + reasons, cached in Redis | median answer under 10 s cold, instant when cached |
| M3 | Live watch: factory events → Kafka → analyzers → alerts channel | a day of launches processed with lag under a minute and no duplicates |
| M4 | Labelled dataset, features, model, MLflow | time-split precision/recall published; beats the hard rules alone |
| M5 | Grounded "why" explanations | on a review set, every sentence cites a check or a source line |
| M6 | Public demo and results | README shows tokens analysed, precision at the alert threshold, median time to verdict |
