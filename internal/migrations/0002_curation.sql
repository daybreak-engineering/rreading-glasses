-- Manual curation; wins over upstream. See docs/design.md phase 2.

CREATE TABLE author_curation (
  author_id   BIGINT PRIMARY KEY,
  mode        TEXT NOT NULL DEFAULT 'rules' CHECK (mode IN ('rules', 'allowlist')),
  merged_into BIGINT CHECK (merged_into <> author_id),
  note        TEXT NOT NULL DEFAULT '',
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE work_curation (
  work_id           BIGINT PRIMARY KEY,
  status            TEXT CHECK (status IN ('canon', 'hidden')),
  merged_into       BIGINT CHECK (merged_into <> work_id),
  title             TEXT,
  edition_id        BIGINT,
  primary_author_id BIGINT,
  note              TEXT NOT NULL DEFAULT '',
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX work_curation_merged_idx ON work_curation (merged_into) WHERE merged_into IS NOT NULL;
CREATE INDEX work_curation_primary_idx ON work_curation (primary_author_id) WHERE primary_author_id IS NOT NULL;

CREATE TABLE edition_curation (
  edition_id BIGINT PRIMARY KEY,
  language   TEXT,
  hidden     BOOLEAN NOT NULL DEFAULT FALSE,
  note       TEXT NOT NULL DEFAULT '',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE curation_log (
  id        BIGSERIAL PRIMARY KEY,
  at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  kind      TEXT NOT NULL, -- author, work, edition
  entity_id BIGINT NOT NULL,
  before    JSONB,
  after     JSONB NOT NULL,
  source    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX curation_log_entity_idx ON curation_log (kind, entity_id);
