-- Verdicts computed offline from stored data. Manual curation wins over them.

CREATE TABLE rule_verdicts (
  work_id     BIGINT NOT NULL,
  rule        TEXT NOT NULL CHECK (rule IN ('junk', 'foreign', 'sibling')),
  reason      TEXT NOT NULL,
  computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (work_id, rule)
);
