CREATE TABLE tenants (
  id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;

CREATE TABLE credentials (
  digest BINARY(32) PRIMARY KEY,
  tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  label VARCHAR(96) NOT NULL,
  role ENUM('owner','director','editor','viewer','operator','push') NOT NULL,
  channel VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  source ENUM('','primary','backup') NOT NULL DEFAULT '',
  source_generation BIGINT UNSIGNED NOT NULL DEFAULT 0,
  expires_at DATETIME(6) NOT NULL,
  revoked_at DATETIME(6),
  FOREIGN KEY (tenant) REFERENCES tenants(id),
  KEY (tenant, channel, session_id, role)
) ENGINE=InnoDB;

CREATE TABLE channels (
  tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  channel VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  owner VARCHAR(96) NOT NULL,
  title VARCHAR(160) NOT NULL,
  available_from DATETIME(6) NOT NULL,
  available_until DATETIME(6) NOT NULL,
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (tenant, channel),
  FOREIGN KEY (tenant) REFERENCES tenants(id),
  KEY (tenant, available_from, channel)
) ENGINE=InnoDB;

CREATE TABLE sessions (
  id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
  tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  channel VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  state ENUM('waiting','live','degraded','switching','ending','archived') NOT NULL,
  active_source ENUM('','primary','backup') NOT NULL DEFAULT '',
  source_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0,
  source_floor_ms BIGINT NOT NULL DEFAULT 0,
  next_offset_ms BIGINT NOT NULL DEFAULT 0,
  source_end_ms BIGINT NOT NULL DEFAULT 0,
  media_duration_ms BIGINT NOT NULL DEFAULT 0,
  next_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,
  revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
  segment_ms INT NOT NULL,
  preset VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  preferred_source ENUM('','primary','backup') NOT NULL DEFAULT '',
  started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  ended_at DATETIME(6),
  FOREIGN KEY (tenant, channel) REFERENCES channels(tenant, channel),
  KEY (state, id),
  KEY (tenant, channel, started_at, id)
) ENGINE=InnoDB;

CREATE TABLE source_inputs (
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  source ENUM('primary','backup') NOT NULL,
  generation BIGINT UNSIGNED NOT NULL,
  last_received_at DATETIME(6),
  reported_lost_at DATETIME(6),
  clock_origin_ms BIGINT NOT NULL,
  PRIMARY KEY (session_id, source),
  FOREIGN KEY (session_id) REFERENCES sessions(id)
) ENGINE=InnoDB;

CREATE TABLE operation_receipts (
  tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  operation VARCHAR(48) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  request_key VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  request_digest BINARY(32) NOT NULL,
  object_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (tenant, operation, request_key),
  FOREIGN KEY (tenant) REFERENCES tenants(id)
) ENGINE=InnoDB;

CREATE TABLE session_events (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  actor VARCHAR(96) NOT NULL,
  details JSON NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  FOREIGN KEY (session_id) REFERENCES sessions(id),
  KEY (session_id, id)
) ENGINE=InnoDB;
