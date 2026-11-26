CREATE TABLE visibility_batches (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  tenant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  credential_digest BINARY(32) NOT NULL,
  request_key VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  request_digest BINARY(32) NOT NULL,
  actor VARCHAR(96) NOT NULL,
  hide BOOLEAN NOT NULL,
  visible_until DATETIME(6),
  item_count TINYINT UNSIGNED NOT NULL,
  processed TINYINT UNSIGNED NOT NULL DEFAULT 0,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  CONSTRAINT visibility_batch_size CHECK (item_count BETWEEN 1 AND 50 AND processed <= item_count),
  CONSTRAINT visibility_batch_action CHECK ((hide AND visible_until IS NULL) OR (NOT hide AND visible_until IS NOT NULL)),
  FOREIGN KEY (tenant) REFERENCES tenants(id),
  UNIQUE KEY (tenant, credential_digest, request_key),
  KEY (tenant, credential_digest, id)
) ENGINE=InnoDB;

CREATE TABLE visibility_batch_items (
  batch_id BIGINT UNSIGNED NOT NULL,
  position TINYINT UNSIGNED NOT NULL,
  recording_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  expected_revision BIGINT UNSIGNED NOT NULL,
  state ENUM('not_started','applied','rejected') NOT NULL DEFAULT 'not_started',
  applied_revision BIGINT UNSIGNED,
  reason VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin,
  PRIMARY KEY (batch_id, position),
  UNIQUE KEY (batch_id, recording_id),
  FOREIGN KEY (batch_id) REFERENCES visibility_batches(id)
) ENGINE=InnoDB;
