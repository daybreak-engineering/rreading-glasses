-- Upstream requests per Unix hour, so a restart doesn't reset the daily budget.

CREATE TABLE upstream_budget (
  hour  BIGINT PRIMARY KEY,
  count BIGINT NOT NULL
);
