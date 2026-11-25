CREATE TABLE captions (
  id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
  recording_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  language VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  revision BIGINT UNSIGNED NOT NULL,
  blob_key VARCHAR(190) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  FOREIGN KEY (recording_id) REFERENCES recordings(id),
  FOREIGN KEY (blob_key) REFERENCES blobs(blob_key),
  UNIQUE KEY (recording_id, language, revision)
) ENGINE=InnoDB;

CREATE TABLE edits (
  id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
  recording_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  recording_revision BIGINT UNSIGNED NOT NULL,
  caption_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin,
  from_ms BIGINT NOT NULL,
  to_ms BIGINT NOT NULL,
  actual_from_ms BIGINT,
  actual_to_ms BIGINT,
  state ENUM('queued','running','published','failed','hidden','purged') NOT NULL,
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  node VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin,
  lease_until DATETIME(6),
  output_blob VARCHAR(190) CHARACTER SET ascii COLLATE ascii_bin,
  caption_blob VARCHAR(190) CHARACTER SET ascii COLLATE ascii_bin,
  error VARCHAR(255),
  visible_until DATETIME(6),
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  FOREIGN KEY (recording_id) REFERENCES recordings(id),
  FOREIGN KEY (caption_id) REFERENCES captions(id),
  FOREIGN KEY (node) REFERENCES transcode_nodes(id),
  FOREIGN KEY (output_blob) REFERENCES blobs(blob_key),
  FOREIGN KEY (caption_blob) REFERENCES blobs(blob_key),
  KEY (state, lease_until, id),
  KEY (state, visible_until),
  KEY (node, state, lease_until)
) ENGINE=InnoDB;

CREATE TABLE edit_segments (
  edit_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  seq BIGINT UNSIGNED NOT NULL,
  track VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  blob_key VARCHAR(190) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  media_ms BIGINT NOT NULL,
  duration_ms INT NOT NULL,
  PRIMARY KEY (edit_id, seq, track),
  FOREIGN KEY (edit_id) REFERENCES edits(id),
  FOREIGN KEY (blob_key) REFERENCES blobs(blob_key)
) ENGINE=InnoDB;

CREATE TABLE qoe_events (
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  device VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  event_key VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  kind ENUM('startup','stall','ended','error','heartbeat') NOT NULL,
  duration_ms BIGINT NOT NULL,
  media_ms BIGINT NOT NULL,
  recorded_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (session_id, device, event_key),
  FOREIGN KEY (session_id) REFERENCES sessions(id),
  KEY (session_id, recorded_at)
) ENGINE=InnoDB;

CREATE TABLE usage_hourly (
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  hour_start DATETIME NOT NULL,
  ingress_bytes BIGINT UNSIGNED NOT NULL DEFAULT 0,
  egress_bytes BIGINT UNSIGNED NOT NULL DEFAULT 0,
  media_requests BIGINT UNSIGNED NOT NULL DEFAULT 0,
  egress_errors BIGINT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (session_id, hour_start),
  FOREIGN KEY (session_id) REFERENCES sessions(id),
  KEY (hour_start, session_id)
) ENGINE=InnoDB;
