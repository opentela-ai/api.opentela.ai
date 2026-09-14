-- 0013_model_pricing.sql — per-model buyer caps + console-managed seller asks
-- (design §5/§6: the price a buyer accepts and the price a seller charges
-- are both per service+model, not three flat numbers; and both are editable
-- from the console without a node-side process).

-- account_model_caps: per-(service, model) buyer caps. A row overrides the
-- account's flat caps TIER BY TIER — a tier left NULL inherits the flat cap
-- (or unlimited when that is NULL too), so a model row can tighten just the
-- output rate without loosening anything else.
CREATE TABLE IF NOT EXISTS account_model_caps (
    account_id                   TEXT NOT NULL,
    service                      TEXT NOT NULL,
    model                        TEXT NOT NULL,
    max_input_per_million        BIGINT,
    max_cached_input_per_million BIGINT,
    max_output_per_million       BIGINT,
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, service, model),
    CONSTRAINT account_model_caps_nonneg_chk
        CHECK ((max_input_per_million IS NULL OR max_input_per_million >= 0)
           AND (max_cached_input_per_million IS NULL OR max_cached_input_per_million >= 0)
           AND (max_output_per_million IS NULL OR max_output_per_million >= 0)),
    -- Same 1e12 bound as peer_asks: ~1e6 OTELA per 1M tokens at 9 decimals
    -- is far past any sane rate, and keeps quote math inside int64.
    CONSTRAINT account_model_caps_bound_chk
        CHECK ((max_input_per_million IS NULL OR max_input_per_million < 1000000000000)
           AND (max_cached_input_per_million IS NULL OR max_cached_input_per_million < 1000000000000)
           AND (max_output_per_million IS NULL OR max_output_per_million < 1000000000000))
);

-- peer_ask_config: the seller OWNER's durable, console-editable asks per
-- peer. Unlike peer_asks (TTL'd market rows), this has no expiry — it is
-- what the seller WANTS to charge. The API's ask refresher republishes it
-- to peer_asks with a fresh TTL while the peer's live mesh observation
-- matches the owner wallet (the same liveness predicate the billing gate
-- routes on), so a console-configured seller needs no node-side publisher.
-- A node running `askpublish` writes peer_asks directly and remains
-- supported; the two sources must not both drive the same peer.
CREATE TABLE IF NOT EXISTS peer_ask_config (
    peer_id                   TEXT NOT NULL,
    service                   TEXT NOT NULL,
    model                     TEXT NOT NULL,
    input_per_million         BIGINT NOT NULL,
    cached_input_per_million  BIGINT NOT NULL,
    output_per_million        BIGINT NOT NULL,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (peer_id, service, model),
    CONSTRAINT peer_ask_config_rates_nonneg_chk
        CHECK (input_per_million >= 0
           AND cached_input_per_million >= 0
           AND output_per_million >= 0),
    CONSTRAINT peer_ask_config_rates_bound_chk
        CHECK (input_per_million    < 1000000000000
           AND cached_input_per_million < 1000000000000
           AND output_per_million   < 1000000000000)
);

CREATE INDEX IF NOT EXISTS idx_peer_ask_config_peer
    ON peer_ask_config (peer_id);
