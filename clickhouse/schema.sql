-- ClickHouse schema for the opentela GPU performance pipeline.
--
-- Apply once (the api does not auto-migrate ClickHouse):
--   clickhouse-client --host <host> --database opentela --multiquery < clickhouse/schema.sql
-- or over HTTP:
--   curl -u default:<password> 'https://<host>:8443/?database=opentela' --data-binary @clickhouse/schema.sql
--
-- Two layers:
--   perf_samples — one row per proxied inference response, 30-day TTL. Raw
--     material for debugging and re-derivation; the public surface never
--     reads it.
--   perf_hourly(+ mv) — hourly rollup keyed by (gpu_model, model, service),
--     with TDigest aggregate states so the leaderboard can merge exact-ish
--     percentiles across arbitrary windows, plus a unique-state over the
--     anonymized provider fingerprint. 13-month retention.
--
-- Privacy: peer_fp is a 64-bit sha256 prefix of the serving peer's id — it
-- supports distinct-provider counting but does not identify an operator.
-- No API keys, prompts, or payloads are ever inserted.

CREATE TABLE IF NOT EXISTS perf_samples
(
    ts             DateTime64(3, 'UTC') DEFAULT now64(3),
    service        LowCardinality(String) DEFAULT '',
    route          LowCardinality(String) DEFAULT '',
    model          LowCardinality(String) DEFAULT '',
    peer_fp        FixedString(16) DEFAULT '',
    gpu_model      LowCardinality(String) DEFAULT '',
    gpu_count      UInt8 DEFAULT 0,
    status         UInt16 DEFAULT 0,
    client_abort   Bool DEFAULT false,
    ttft_ms        Float32 DEFAULT 0,
    first_token_ms Float32 DEFAULT 0,
    total_ms       Float32 DEFAULT 0,
    input_tokens   UInt32 DEFAULT 0,
    cached_input_tokens UInt32 DEFAULT 0,
    output_tokens  UInt32 DEFAULT 0,
    response_bytes UInt64 DEFAULT 0,
    gpu_ms         UInt64 DEFAULT 0
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (gpu_model, model, ts)
TTL toDate(ts) + INTERVAL 30 DAY;

CREATE TABLE IF NOT EXISTS perf_hourly
(
    hour          DateTime('UTC'),
    service       LowCardinality(String),
    model         LowCardinality(String),
    gpu_model     LowCardinality(String),
    requests      SimpleAggregateFunction(sum, UInt64),
    errors        SimpleAggregateFunction(sum, UInt64),
    client_aborts SimpleAggregateFunction(sum, UInt64),
    input_tokens  SimpleAggregateFunction(sum, UInt64),
    cached_input_tokens SimpleAggregateFunction(sum, UInt64),
    output_tokens SimpleAggregateFunction(sum, UInt64),
    gen_ms        SimpleAggregateFunction(sum, Float64),
    ttft_q        AggregateFunction(quantilesTDigest(0.5, 0.9, 0.99), Float32),
    tps_q         AggregateFunction(quantilesTDigest(0.5, 0.9, 0.99), Float32),
    provider_uniq AggregateFunction(uniq, FixedString(16))
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(hour)
ORDER BY (gpu_model, model, service, hour)
TTL toDateTime(hour) + INTERVAL 13 MONTH;

-- Per-request generation throughput: streaming requests spread token output
-- over (first_token → end); everything else (aborts, non-streaming) falls
-- back to total latency. Requests without token counts contribute 0 — they
-- pull percentiles down honestly rather than being silently excluded from
-- the denominator.
CREATE MATERIALIZED VIEW IF NOT EXISTS perf_hourly_mv TO perf_hourly AS
SELECT
    toStartOfHour(ts) AS hour,
    service,
    model,
    gpu_model,
    count() AS requests,
    countIf(status >= 500) AS errors,
    countIf(client_abort) AS client_aborts,
    sum(input_tokens) AS input_tokens,
    sum(cached_input_tokens) AS cached_input_tokens,
    sum(output_tokens) AS output_tokens,
    sum(if(first_token_ms > 0, greatest(total_ms - first_token_ms, 0), total_ms)) AS gen_ms,
    quantilesTDigestState(0.5, 0.9, 0.99)(ttft_ms) AS ttft_q,
    -- Qualify source columns: bare names would resolve to the sum() aliases
    -- above, which ClickHouse rejects as nested aggregation.
    quantilesTDigestState(0.5, 0.9, 0.99)(
        toFloat32(if(perf_samples.output_tokens > 0,
           perf_samples.output_tokens * 1000.0 / greatest(if(perf_samples.first_token_ms > 0, perf_samples.total_ms - perf_samples.first_token_ms, perf_samples.total_ms), 1),
           0))
    ) AS tps_q,
    uniqState(peer_fp) AS provider_uniq
FROM perf_samples
GROUP BY
    hour,
    service,
    model,
    gpu_model;

-- Daily token usage per model (GPUs and services collapsed): the reporting
-- window for usage dashboards, mirroring the Tinybird token_usage_daily
-- endpoint. Reads perf_hourly so retention follows the 13-month rollup
-- rather than the 30-day raw table; SimpleAggregateFunction(sum) columns
-- aggregate with plain sum().
-- NOTE: unlike the Tinybird pipe there is no success_only filter here — the
-- hourly rollup keeps only status *counts* (errors/client_aborts), not
-- per-row status, so row-level filtering must be done on perf_samples
-- directly (30-day window) using the settlement predicate:
--   status >= 200 AND status < 300 AND NOT client_abort
--   AND (input_tokens > 0 OR cached_input_tokens > 0 OR output_tokens > 0)
CREATE VIEW IF NOT EXISTS token_usage_daily AS
SELECT
    toDate(hour) AS day,
    model,
    sum(requests) AS requests,
    sum(input_tokens) AS input_tokens,
    sum(cached_input_tokens) AS cached_input_tokens,
    sum(output_tokens) AS output_tokens
FROM perf_hourly
GROUP BY day, model;
