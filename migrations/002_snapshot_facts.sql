CREATE TABLE IF NOT EXISTS snapshot_services (
    host_id VARCHAR(128) NOT NULL,
    batch_id VARCHAR(191) NOT NULL,
    service_id VARCHAR(191) NOT NULL,
    service_type VARCHAR(32) NOT NULL,
    service_name TEXT NOT NULL,
    PRIMARY KEY (host_id, batch_id, service_id),
    KEY idx_service_identity (host_id, service_id, batch_id),
    CONSTRAINT fk_service_snapshot FOREIGN KEY (host_id, batch_id)
        REFERENCES snapshot_batches (host_id, batch_id)
        ON DELETE CASCADE ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS snapshot_processes (
    host_id VARCHAR(128) NOT NULL,
    batch_id VARCHAR(191) NOT NULL,
    pid INT UNSIGNED NOT NULL,
    ppid INT UNSIGNED NOT NULL DEFAULT 0,
    start_ticks BIGINT UNSIGNED NOT NULL,
    service_id VARCHAR(191) NULL,
    netns VARCHAR(64) NOT NULL DEFAULT '',
    cgroup TEXT NOT NULL,
    PRIMARY KEY (host_id, batch_id, pid, start_ticks),
    KEY idx_process_lifecycle (host_id, pid, start_ticks, batch_id),
    KEY idx_process_service (host_id, service_id, batch_id),
    CONSTRAINT fk_process_snapshot FOREIGN KEY (host_id, batch_id)
        REFERENCES snapshot_batches (host_id, batch_id)
        ON DELETE CASCADE ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS snapshot_listeners (
    host_id VARCHAR(128) NOT NULL,
    batch_id VARCHAR(191) NOT NULL,
    service_id VARCHAR(191) NOT NULL,
    pid INT UNSIGNED NOT NULL,
    ip VARBINARY(16) NOT NULL,
    port SMALLINT UNSIGNED NOT NULL,
    netns VARCHAR(64) NOT NULL DEFAULT '',
    PRIMARY KEY (host_id, batch_id, service_id, pid, ip, port, netns),
    KEY idx_listener_lookup (host_id, ip, port, netns, batch_id),
    CONSTRAINT fk_listener_service FOREIGN KEY (host_id, batch_id, service_id)
        REFERENCES snapshot_services (host_id, batch_id, service_id)
        ON DELETE CASCADE ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS snapshot_device_bindings (
    host_id VARCHAR(128) NOT NULL,
    batch_id VARCHAR(191) NOT NULL,
    device_key VARCHAR(191) NOT NULL,
    pid INT UNSIGNED NOT NULL,
    start_ticks BIGINT UNSIGNED NOT NULL,
    memory_bytes BIGINT UNSIGNED NULL,
    PRIMARY KEY (host_id, batch_id, device_key, pid, start_ticks),
    KEY idx_binding_process (host_id, pid, start_ticks, batch_id),
    KEY idx_binding_device (host_id, device_key, batch_id),
    CONSTRAINT fk_binding_process FOREIGN KEY (host_id, batch_id, pid, start_ticks)
        REFERENCES snapshot_processes (host_id, batch_id, pid, start_ticks)
        ON DELETE CASCADE ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
