CREATE TABLE blobs (
  blob_key VARCHAR(190) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
  digest BINARY(32) NOT NULL,
  bytes BIGINT UNSIGNED NOT NULL,
  state ENUM('ready','deleting','deleted') NOT NULL DEFAULT 'ready',
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  KEY (state, created_at)
) ENGINE=InnoDB;

CREATE TABLE input_fragments (
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  source ENUM('primary','backup') NOT NULL,
  generation BIGINT UNSIGNED NOT NULL,
  source_ms BIGINT NOT NULL,
  offset_ms BIGINT NOT NULL,
  duration_ms INT NOT NULL,
  blob_key VARCHAR(190) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  received_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (session_id, source, generation, source_ms),
  UNIQUE KEY (session_id, source, generation, offset_ms),
  FOREIGN KEY (session_id, source) REFERENCES source_inputs(session_id, source),
  FOREIGN KEY (blob_key) REFERENCES blobs(blob_key)
) ENGINE=InnoDB;

CREATE TABLE transcode_nodes (
  id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
  slots INT UNSIGNED NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT true,
  last_seen DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;

CREATE TABLE transcode_jobs (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  source ENUM('primary','backup') NOT NULL,
  source_generation BIGINT UNSIGNED NOT NULL,
  source_epoch BIGINT UNSIGNED NOT NULL,
  source_ms BIGINT NOT NULL,
  offset_ms BIGINT NOT NULL,
  duration_ms INT NOT NULL,
  seq BIGINT UNSIGNED NOT NULL,
  gap_ms BIGINT NOT NULL,
  input_blob VARCHAR(190) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  state ENUM('queued','running','done','failed','superseded') NOT NULL DEFAULT 'queued',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  node VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin,
  lease_until DATETIME(6),
  error VARCHAR(255),
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  pending_session CHAR(24) CHARACTER SET ascii COLLATE ascii_bin AS
    (CASE WHEN state IN ('queued','running') THEN session_id ELSE NULL END) STORED,
  UNIQUE KEY (pending_session),
  FOREIGN KEY (session_id) REFERENCES sessions(id),
  FOREIGN KEY (input_blob) REFERENCES blobs(blob_key),
  FOREIGN KEY (node) REFERENCES transcode_nodes(id),
  KEY (state, lease_until, id),
  KEY (node, state, lease_until),
  KEY (session_id, seq)
) ENGINE=InnoDB;

CREATE TABLE segments (
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  track VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  seq BIGINT UNSIGNED NOT NULL,
  source ENUM('primary','backup') NOT NULL,
  source_epoch BIGINT UNSIGNED NOT NULL,
  source_ms BIGINT NOT NULL,
  offset_ms BIGINT NOT NULL,
  media_ms BIGINT NOT NULL,
  duration_ms INT NOT NULL,
  gap_ms BIGINT NOT NULL,
  blob_key VARCHAR(190) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  published_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (session_id, track, seq),
  FOREIGN KEY (session_id) REFERENCES sessions(id),
  FOREIGN KEY (blob_key) REFERENCES blobs(blob_key)
) ENGINE=InnoDB;

CREATE TABLE recordings (
  id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
  state ENUM('published','hidden','empty','purged') NOT NULL,
  revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
  duration_ms BIGINT NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  visible_until DATETIME(6),
  hidden_at DATETIME(6),
  FOREIGN KEY (session_id) REFERENCES sessions(id),
  KEY (state, visible_until)
) ENGINE=InnoDB;
