-- Per-alias model metadata overrides reported by the LM Studio compatible
-- model list (GET /api/v1/models).
-- {"context_length": <int>, "tools": <bool>, "vision": <bool>}; a missing key
-- means "detect from the upstream".
ALTER TABLE aliases ADD COLUMN model_info_json TEXT NOT NULL DEFAULT '{}';
