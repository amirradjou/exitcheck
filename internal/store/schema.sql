-- The live watch's tables. Every statement is idempotent: Open runs this file on each start.

CREATE TABLE IF NOT EXISTS watch_cursors (
    chain      text PRIMARY KEY,
    block      bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Pools created against a quote token, waiting for their first liquidity.
CREATE TABLE IF NOT EXISTS pending_pools (
    chain         text NOT NULL,
    pool          text NOT NULL,
    dex           text NOT NULL,
    token         text NOT NULL,
    quote         text NOT NULL,
    created_block bigint NOT NULL,
    created_tx    text NOT NULL,
    seen_at       timestamptz NOT NULL,
    PRIMARY KEY (chain, pool)
);

-- One row per launch (pool). deliveries counts how often Kafka delivered it; above 1 is a redelivery that the key
-- turned into a count instead of a second result.
CREATE TABLE IF NOT EXISTS launches (
    id            text PRIMARY KEY,
    chain         text NOT NULL,
    dex           text NOT NULL,
    pool          text NOT NULL,
    token         text NOT NULL,
    quote         text NOT NULL,
    created_block bigint NOT NULL,
    created_tx    text NOT NULL,
    funded_block  bigint NOT NULL,
    funded_at     timestamptz NOT NULL,
    seen_at       timestamptz,
    detected_at   timestamptz NOT NULL,
    received_at   timestamptz NOT NULL DEFAULT now(),
    analyzed_at   timestamptz,
    level         text,
    headline      text,
    answer        text,
    complete      boolean,
    from_cache    boolean,
    check_seconds double precision,
    error         text,
    deliveries    integer NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS launches_funded_at ON launches (funded_at);
-- report is the check's full JSON report (facts, simulation, verdict): the model's features as they stood at the
-- checked block. NULL for launches analyzed before it was kept, and for answers served from the cache.
ALTER TABLE launches ADD COLUMN IF NOT EXISTS report jsonb;

-- One alert per token, claimed before it is sent.
CREATE TABLE IF NOT EXISTS alerts (
    chain      text NOT NULL,
    token      text NOT NULL,
    launch_id  text NOT NULL,
    level      text NOT NULL,
    claimed_at timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz,
    PRIMARY KEY (chain, token)
);
-- sent_at settles a claim; posted says whether it reached a channel (none is configured until TELEGRAM_ALERTS_CHAT).
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS posted boolean NOT NULL DEFAULT false;
