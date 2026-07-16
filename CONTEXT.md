# Domain context

- **Ledger** — Process-local, token-only aggregation of every LLM request attempt. Records successes, failures, reported or estimated token usage; resets on restart.
- **Stats bucket** — Ledger row keyed by UTC day × requested alias (or raw model string) × served provider/model.
