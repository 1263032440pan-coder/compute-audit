CREATE TABLE hosts (
    host_id VARCHAR(128) NOT NULL,
    hostname VARCHAR(255) NOT NULL DEFAULT '',
    management_ip VARBINARY(16) NULL,
    arch VARCHAR(32) NOT NULL DEFAULT '',
    vendor VARCHAR(32) NOT NULL DEFAULT '',
    last_seen DATETIME(6) NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'unknown',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
        ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (host_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE snapshot_batches (
    host_id VARCHAR(128) NOT NULL,
    batch_id VARCHAR(191) NOT NULL,
    boot_id VARCHAR(128) NOT NULL,
    sequence BIGINT UNSIGNED NOT NULL,
    observed_at DATETIME(6) NOT NULL,
    received_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    schema_version SMALLINT UNSIGNED NOT NULL,
    status JSON NOT NULL,
    payload_sha256 BINARY(32) NOT NULL,
    PRIMARY KEY (host_id, batch_id),
    UNIQUE KEY uq_snapshot_sequence (host_id, boot_id, sequence),
    KEY idx_snapshot_observed_at (observed_at),
    CONSTRAINT fk_snapshot_host FOREIGN KEY (host_id)
        REFERENCES hosts (host_id) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
