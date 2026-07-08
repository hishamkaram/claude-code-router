# Router Product Invariants

- Claude Code must see one stable Anthropic-compatible base URL for a session.
- The gateway routes by session and model alias, not one global process model.
- New work after `/model <alias>` should use that alias where safely possible.
- Existing workers may stay on their spawn-time model; report this instead of hiding it.
- Compatibility degradation is allowed only when safe and visible.
- Silent fallback to Claude is never allowed.
- Rejections must explain the missing capability or unsafe translation.
