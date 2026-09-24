---
trial_id: "v000"
status: "COMPLETED"
outcome: "KEEP"
---

# Trial Summary: Baseline v000

## [TRIAL_OUTCOME]

### Performance Metrics

- throughput_qps: 397.1
- latency_p99_ms: 16.62
- latency_p50_ms: 6.19
- error_rate: 0.0
- memory_rss_mb: 128.0
- cache_hit_rate: 0.995

### Subsystem Health & Bottleneck Matrix

| Active Subsystem | Declaring Trait Provider | Primary Telemetry Sources | Key Observed Metrics | Health Status | Subsystem Tuning State | Bottleneck / Headroom Assessment |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Ingress / Router** | `apo-provider-router` | `monitor/router_metrics.csv` | Queue depth 0, drops 0, batch size 8 | `HEALTHY` | `TUNING` | Router queue clear with zero overflow drops |
| **Execution Engine** | `apo-provider-engine` | `monitor/engine_metrics.csv` | Contention penalty 1.05, active workers 4 | `HEALTHY` | `TUNING` | Workers stable with headroom for concurrency |
| **Substrate / Cache** | `apo-provider-substrate` | `monitor/substrate_metrics.csv` | Cache hit ratio 99.5%, memory 128MB | `HEALTHY` | `TUNING` | Cache hot path handling majority of requests |
