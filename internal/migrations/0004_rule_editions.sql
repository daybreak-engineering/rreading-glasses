-- Default editions chosen by rules. A manual edition_id wins over them.

CREATE TABLE rule_editions (
  work_id     BIGINT PRIMARY KEY,
  edition_id  BIGINT NOT NULL,
  reason      TEXT NOT NULL,
  computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
