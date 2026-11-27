CREATE TABLE productions (
  id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
  session_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
  title VARCHAR(160) NOT NULL,
  timezone VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  state ENUM('draft','on_air','ending','sealed') NOT NULL DEFAULT 'draft',
  rundown_rev BIGINT UNSIGNED NOT NULL DEFAULT 1,
  asrun_rev BIGINT UNSIGNED NOT NULL DEFAULT 0,
  control_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0,
  control_credential BINARY(32),
  controller VARCHAR(96) NOT NULL DEFAULT '',
  control_until DATETIME(6),
  program_slot VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  preview_slot VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  fallback_slot VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  program_frame BIGINT NOT NULL DEFAULT 0,
  started_at DATETIME(6),
  sealed_at DATETIME(6),
  FOREIGN KEY (session_id) REFERENCES sessions(id),
  KEY (state,id)
) ENGINE=InnoDB;

CREATE TABLE production_slots (
  production_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  slot_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  title VARCHAR(160) NOT NULL,
  kind ENUM('live','clip','slate') NOT NULL,
  audio_channel ENUM('stereo','left','right','mute') NOT NULL,
  timecode_zero_ms BIGINT,
  alignment_ms INT NOT NULL DEFAULT 0,
  observed_at DATETIME(6),
  last_source_frame BIGINT,
  lost_at DATETIME(6),
  PRIMARY KEY (production_id,slot_id),
  FOREIGN KEY (production_id) REFERENCES productions(id)
) ENGINE=InnoDB;

CREATE TABLE production_inputs (
  production_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  slot_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  source_frame BIGINT NOT NULL,
  frame_count INT NOT NULL,
  blob_key VARCHAR(190) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  start_frame BIGINT NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (production_id,slot_id,source_frame),
  FOREIGN KEY (production_id,slot_id) REFERENCES production_slots(production_id,slot_id),
  FOREIGN KEY (blob_key) REFERENCES blobs(blob_key),
  KEY (production_id,slot_id,start_frame),
  KEY (blob_key)
) ENGINE=InnoDB;

CREATE TABLE production_rundowns (
  production_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  revision BIGINT UNSIGNED NOT NULL,
  items JSON NOT NULL,
  actor VARCHAR(96) NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (production_id,revision),
  FOREIGN KEY (production_id) REFERENCES productions(id)
) ENGINE=InnoDB;

CREATE TABLE production_takes (
  id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
  production_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  control_epoch BIGINT UNSIGNED NOT NULL,
  command_id VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  request_key VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  request_digest BINARY(32) NOT NULL,
  slot_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  transition ENUM('cut','dissolve') NOT NULL,
  transition_frames INT NOT NULL,
  state ENUM('queued','published','fenced','failed') NOT NULL,
  planned_frame BIGINT NOT NULL,
  actual_frame BIGINT,
  actor VARCHAR(96) NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY (production_id,request_key),
  UNIQUE KEY (production_id,control_epoch,command_id),
  KEY (production_id,state,planned_frame),
  FOREIGN KEY (production_id,slot_id) REFERENCES production_slots(production_id,slot_id)
) ENGINE=InnoDB;

CREATE TABLE production_overlays (
  id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
  production_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  template ENUM('lower_third','logo','timer') NOT NULL,
  content VARCHAR(320) NOT NULL,
  font_digest BINARY(32) NOT NULL,
  font_size INT NOT NULL,
  on_frame BIGINT NOT NULL,
  off_frame BIGINT NOT NULL,
  expected_off_frame BIGINT NOT NULL,
  actor VARCHAR(96) NOT NULL,
  cleared_by VARCHAR(96),
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  FOREIGN KEY (production_id) REFERENCES productions(id),
  KEY (production_id,on_frame,off_frame)
) ENGINE=InnoDB;

CREATE TABLE production_frames (
  production_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  start_frame BIGINT NOT NULL,
  frame_count INT NOT NULL,
  source_plan JSON NOT NULL,
  take_id CHAR(24) CHARACTER SET ascii COLLATE ascii_bin,
  claim CHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  lease_until DATETIME(6) NOT NULL,
  attempts INT NOT NULL DEFAULT 1,
  last_error VARCHAR(255),
  sequence_no BIGINT UNSIGNED,
  evidence JSON,
  published_at DATETIME(6),
  PRIMARY KEY (production_id,start_frame),
  UNIQUE KEY (production_id,sequence_no),
  FOREIGN KEY (production_id) REFERENCES productions(id),
  FOREIGN KEY (take_id) REFERENCES production_takes(id)
) ENGINE=InnoDB;
