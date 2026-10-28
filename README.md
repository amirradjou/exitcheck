# rugcheck

Crypto scam detector: a Telegram bot that gives a risk verdict for any token address, from contract checks, a buy-then-sell simulation on a forked chain and an ML classifier — plus a live watch of new token launches.

## Getting started

```sh
go mod download
make build && ./bin/rugcheck version
```

## Development

```sh
make test     # tests
make lint     # lint + format
```
