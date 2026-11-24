CREATE TABLE IF NOT EXISTS observer_bindings (
    deepflow_instance_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
    observer_agent_id VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
    host_id VARCHAR(128) NOT NULL,
    valid_from DATETIME(6) NOT NULL,
    valid_to DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (deepflow_instance_id, observer_agent_id, host_id, valid_from),
    KEY idx_observer_time (deepflow_instance_id, observer_agent_id, valid_from, valid_to),
    CONSTRAINT fk_observer_host FOREIGN KEY (host_id)
        REFERENCES hosts (host_id)
        ON DELETE RESTRICT ON UPDATE RESTRICT,
    CONSTRAINT chk_observer_valid_range CHECK (valid_to IS NULL OR valid_to > valid_from)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS audit_event_attributions (
    event_id BIGINT UNSIGNED NOT NULL,
    host_id VARCHAR(128) NULL,
    snapshot_batch_id VARCHAR(191) NULL,
    snapshot_observed_at DATETIME(6) NULL,
    process_start_ticks BIGINT UNSIGNED NULL,
    service_id VARCHAR(191) NULL,
    status VARCHAR(48) NOT NULL,
    confidence VARCHAR(32) NOT NULL DEFAULT 'unknown',
    reason VARCHAR(191) NOT NULL DEFAULT '',
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
        ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (event_id),
    KEY idx_attribution_host_time (host_id, snapshot_observed_at),
    KEY idx_attribution_status (status, event_id),
    CONSTRAINT fk_attribution_event FOREIGN KEY (event_id)
        REFERENCES audit_events (event_id)
        ON DELETE CASCADE ON UPDATE RESTRICT,
    CONSTRAINT fk_attribution_host FOREIGN KEY (host_id)
        REFERENCES hosts (host_id)
        ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS audit_event_devices (
    event_id BIGINT UNSIGNED NOT NULL,
    device_key VARCHAR(191) NOT NULL,
    evidence VARCHAR(48) NOT NULL,
    collector_state VARCHAR(32) NOT NULL,
    PRIMARY KEY (event_id, device_key),
    KEY idx_event_device (device_key, event_id),
    CONSTRAINT fk_event_device_attribution FOREIGN KEY (event_id)
        REFERENCES audit_event_attributions (event_id)
        ON DELETE CASCADE ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
