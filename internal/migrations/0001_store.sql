-- Normalized metadata store. Sync never deletes rows; see docs/design.md.

CREATE TABLE authors (
  id         BIGINT PRIMARY KEY,
  name       TEXT NOT NULL DEFAULT '',
  data       JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE works (
  id           BIGINT PRIMARY KEY,
  title        TEXT NOT NULL DEFAULT '',
  full_title   TEXT NOT NULL DEFAULT '',
  short_title  TEXT NOT NULL DEFAULT '',
  release_date TEXT NOT NULL DEFAULT '',
  rating_count BIGINT NOT NULL DEFAULT 0,
  data         JSONB NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE editions (
  id         BIGINT PRIMARY KEY,
  work_id    BIGINT NOT NULL,
  title      TEXT NOT NULL DEFAULT '',
  language   TEXT NOT NULL DEFAULT '',
  format     TEXT NOT NULL DEFAULT '',
  isbn13     TEXT NOT NULL DEFAULT '',
  asin       TEXT NOT NULL DEFAULT '',
  is_ebook   BOOLEAN NOT NULL DEFAULT FALSE,
  listed     BOOLEAN NOT NULL DEFAULT FALSE, -- Included in the work's Books.
  data       JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX editions_work_idx ON editions (work_id);
CREATE INDEX editions_isbn13_idx ON editions (isbn13) WHERE isbn13 <> '';
CREATE INDEX editions_asin_idx ON editions (asin) WHERE asin <> '';

-- Credits in upstream order; replaced on each work update.
CREATE TABLE work_authors (
  work_id   BIGINT NOT NULL,
  author_id BIGINT NOT NULL,
  position  INT NOT NULL,
  PRIMARY KEY (work_id, author_id)
);
CREATE INDEX work_authors_author_idx ON work_authors (author_id);

-- The served author list. Only ever added to.
CREATE TABLE author_works (
  author_id  BIGINT NOT NULL,
  work_id    BIGINT NOT NULL,
  first_seen TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (author_id, work_id)
);
CREATE INDEX author_works_work_idx ON author_works (work_id);

CREATE TABLE series (
  id         BIGINT PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  data       JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE series_works (
  series_id  BIGINT NOT NULL,
  work_id    BIGINT NOT NULL,
  position   TEXT NOT NULL DEFAULT '',
  sort       INT NOT NULL DEFAULT 0,
  is_primary BOOLEAN NOT NULL DEFAULT FALSE,
  PRIMARY KEY (series_id, work_id)
);
CREATE INDEX series_works_work_idx ON series_works (work_id);

CREATE TABLE store_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
