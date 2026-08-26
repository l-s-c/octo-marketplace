-- +migrate Up
-- ============================================================================
-- unified-plugin-backend :: authoritative Plugin schema
-- ============================================================================
-- Creates only the seven confirmed unified Plugin tables. Historical data
-- backfill and retirement of legacy catalog tables are intentionally deferred.
-- JSON CHECK constraints require MySQL 8.0.16 or newer, where CHECK constraints
-- are enforced. All timestamps are application-stamped DATETIME(3), matching the
-- current marketplace conventions.
-- ============================================================================

CREATE TABLE `plugin_categories` (
  `category_id`      VARCHAR(64)  NOT NULL,
  `name`             VARCHAR(128) NOT NULL,
  `icon_key`         VARCHAR(128) NOT NULL DEFAULT '',
  `plugin_types_json` JSON         NOT NULL,
  `sort_order`       INT          NOT NULL DEFAULT 0,
  `status`           TINYINT      NOT NULL DEFAULT 1,
  `created_at`       DATETIME(3)  NOT NULL,
  `updated_at`       DATETIME(3)  NOT NULL,
  `deleted_at`       DATETIME(3)  NULL DEFAULT NULL,
  PRIMARY KEY (`category_id`),
  KEY `idx_plugin_categories_status_order` (`status`, `sort_order`),
  KEY `idx_plugin_categories_deleted_at` (`deleted_at`),
  CONSTRAINT `chk_plugin_categories_types_array`
    CHECK (JSON_TYPE(`plugin_types_json`) = 'ARRAY')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='Unified taxonomy for all Plugin types';

CREATE TABLE `plugins` (
  `plugin_id`           VARCHAR(64)  NOT NULL,
  `plugin_name`         VARCHAR(160) NOT NULL,
  `plugin_type`         ENUM('expert','expert_team','skill','connector') NOT NULL,
  `category_id`         VARCHAR(64)  NULL DEFAULT NULL,
  `tags_json`           JSON         NOT NULL,
  `publisher`           VARCHAR(128) NOT NULL DEFAULT '',
  `owner_uid`           VARCHAR(64)  NOT NULL,
  `space_id`            VARCHAR(64)  NULL DEFAULT NULL,
  `visibility`          ENUM('public','space','private','system') NOT NULL,
  `creator_name`        VARCHAR(128) NOT NULL DEFAULT '',
  `created_by_type`     ENUM('human','bot','import') NOT NULL DEFAULT 'human',
  `created_by_bot_uid`  VARCHAR(64)  NULL DEFAULT NULL,
  `created_by_bot_name` VARCHAR(128) NULL DEFAULT NULL,
  `manifest_json`       JSON         NOT NULL,
  `plugin_json`         JSON         NOT NULL,
  `manifest_hash`       CHAR(71)     NOT NULL,
  `plugin_hash`         CHAR(71)     NOT NULL,
  `current_version_id`  VARCHAR(64)  NULL DEFAULT NULL,
  `status`              TINYINT      NOT NULL DEFAULT 1,
  `created_at`          DATETIME(3)  NOT NULL,
  `updated_at`          DATETIME(3)  NOT NULL,
  `deleted_at`          DATETIME(3)  NULL DEFAULT NULL,
  PRIMARY KEY (`plugin_id`),
  KEY `idx_plugins_type_status_created` (`plugin_type`, `status`, `created_at`),
  KEY `idx_plugins_scope_category_created` (`visibility`, `space_id`, `category_id`, `created_at`),
  KEY `idx_plugins_owner_created` (`owner_uid`, `created_at`),
  KEY `idx_plugins_plugin_hash` (`plugin_hash`),
  KEY `idx_plugins_current_version` (`current_version_id`),
  KEY `idx_plugins_deleted_at` (`deleted_at`),
  CONSTRAINT `chk_plugins_tags_array` CHECK (JSON_TYPE(`tags_json`) = 'ARRAY'),
  CONSTRAINT `chk_plugins_manifest_object` CHECK (JSON_TYPE(`manifest_json`) = 'OBJECT'),
  CONSTRAINT `chk_plugins_package_object` CHECK (JSON_TYPE(`plugin_json`) = 'OBJECT')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='Authoritative current state for unified Plugins';

CREATE TABLE `plugin_relations` (
  `relation_id`     VARCHAR(64) NOT NULL,
  `source_plugin_id` VARCHAR(64) NOT NULL,
  `target_plugin_id` VARCHAR(64) NOT NULL,
  `relation_type`  VARCHAR(64) NOT NULL,
  `sort_order`     INT         NOT NULL DEFAULT 0,
  `relation_json`  JSON        NULL DEFAULT NULL,
  `status`         TINYINT     NOT NULL DEFAULT 1,
  `created_by`     VARCHAR(64) NOT NULL,
  `created_at`     DATETIME(3) NOT NULL,
  `updated_at`     DATETIME(3) NOT NULL,
  `deleted_at`     DATETIME(3) NULL DEFAULT NULL,
  PRIMARY KEY (`relation_id`),
  KEY `idx_plugin_relations_source_type_order` (`source_plugin_id`, `relation_type`, `sort_order`),
  KEY `idx_plugin_relations_target_type` (`target_plugin_id`, `relation_type`),
  KEY `idx_plugin_relations_deleted_at` (`deleted_at`),
  CONSTRAINT `chk_plugin_relations_distinct_plugins`
    CHECK (`source_plugin_id` <> `target_plugin_id`),
  CONSTRAINT `chk_plugin_relations_json_object`
    CHECK (`relation_json` IS NULL OR JSON_TYPE(`relation_json`) = 'OBJECT')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='Directed composition graph between Plugins';

CREATE TABLE `plugin_versions` (
  `version_id`     VARCHAR(64) NOT NULL,
  `plugin_id`      VARCHAR(64) NOT NULL,
  `version`        VARCHAR(64) NOT NULL,
  `manifest_json`  JSON        NOT NULL,
  `plugin_json`    JSON        NOT NULL,
  `manifest_hash`  CHAR(71)    NOT NULL,
  `plugin_hash`    CHAR(71)    NOT NULL,
  `relations_json` JSON        NOT NULL,
  `changelog`      TEXT        NULL,
  `created_by`     VARCHAR(64) NOT NULL,
  `created_at`     DATETIME(3) NOT NULL,
  PRIMARY KEY (`version_id`),
  UNIQUE KEY `uq_plugin_versions_plugin_version` (`plugin_id`, `version`),
  KEY `idx_plugin_versions_plugin_created` (`plugin_id`, `created_at`),
  KEY `idx_plugin_versions_plugin_hash` (`plugin_hash`),
  CONSTRAINT `chk_plugin_versions_manifest_object` CHECK (JSON_TYPE(`manifest_json`) = 'OBJECT'),
  CONSTRAINT `chk_plugin_versions_package_object` CHECK (JSON_TYPE(`plugin_json`) = 'OBJECT'),
  CONSTRAINT `chk_plugin_versions_relations_array` CHECK (JSON_TYPE(`relations_json`) = 'ARRAY')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='Immutable published snapshots for all Plugin types';

CREATE TABLE `plugin_audit_logs` (
  `audit_log_id`          VARCHAR(64)   NOT NULL,
  `plugin_id`             VARCHAR(64)   NOT NULL,
  `action`                VARCHAR(32)   NOT NULL,
  `operator_id`           VARCHAR(64)   NOT NULL,
  `operator_name`         VARCHAR(160)  NOT NULL,
  `request_id`            VARCHAR(80)   NOT NULL,
  `before_hash`           CHAR(71)      NULL DEFAULT NULL,
  `after_hash`            CHAR(71)      NULL DEFAULT NULL,
  `manifest_snapshot_json` JSON          NULL DEFAULT NULL,
  `plugin_snapshot_json`  JSON          NULL DEFAULT NULL,
  `remark`                VARCHAR(1024) NULL DEFAULT NULL,
  `created_at`            DATETIME(3)   NOT NULL,
  PRIMARY KEY (`audit_log_id`),
  KEY `idx_plugin_audit_plugin_created` (`plugin_id`, `created_at`),
  KEY `idx_plugin_audit_action_created` (`action`, `created_at`),
  KEY `idx_plugin_audit_operator_created` (`operator_id`, `created_at`),
  KEY `idx_plugin_audit_request` (`request_id`),
  KEY `idx_plugin_audit_created_at` (`created_at`),
  CONSTRAINT `chk_plugin_audit_manifest_object`
    CHECK (`manifest_snapshot_json` IS NULL OR JSON_TYPE(`manifest_snapshot_json`) = 'OBJECT'),
  CONSTRAINT `chk_plugin_audit_package_object`
    CHECK (`plugin_snapshot_json` IS NULL OR JSON_TYPE(`plugin_snapshot_json`) = 'OBJECT')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='Append-only Plugin operation audit history';

CREATE TABLE `plugin_category_placements` (
  `placement_id`   VARCHAR(64)  NOT NULL,
  `placement_code` VARCHAR(128) NOT NULL,
  `plugin_type`    VARCHAR(64)  NOT NULL,
  `category_id`    VARCHAR(64)  NOT NULL,
  `visible`        TINYINT(1)   NOT NULL DEFAULT 1,
  `sort_order`     INT          NOT NULL DEFAULT 0,
  `created_at`     DATETIME(3)  NOT NULL,
  `updated_at`     DATETIME(3)  NOT NULL,
  PRIMARY KEY (`placement_id`),
  UNIQUE KEY `uq_plugin_category_placement` (`placement_code`, `plugin_type`, `category_id`),
  KEY `idx_plugin_category_placement_list` (`placement_code`, `plugin_type`, `visible`, `sort_order`),
  KEY `idx_plugin_category_placement_category` (`category_id`),
  CONSTRAINT `chk_plugin_category_placement_visible` CHECK (`visible` IN (0, 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='Category visibility and ordering at a placement point';

CREATE TABLE `plugin_placements` (
  `placement_id`   VARCHAR(64)  NOT NULL,
  `placement_code` VARCHAR(128) NOT NULL,
  `plugin_id`      VARCHAR(64)  NOT NULL,
  `category_id`    VARCHAR(64)  NULL DEFAULT NULL,
  `category_key`   VARCHAR(64)
    GENERATED ALWAYS AS (IFNULL(`category_id`, '')) STORED,
  `visible`        TINYINT(1)   NOT NULL DEFAULT 1,
  `sort_order`     INT          NOT NULL DEFAULT 0,
  `created_at`     DATETIME(3)  NOT NULL,
  `updated_at`     DATETIME(3)  NOT NULL,
  PRIMARY KEY (`placement_id`),
  UNIQUE KEY `uq_plugin_placement` (`placement_code`, `plugin_id`, `category_key`),
  KEY `idx_plugin_placement_list` (`placement_code`, `category_key`, `visible`, `sort_order`),
  KEY `idx_plugin_placement_plugin` (`plugin_id`),
  KEY `idx_plugin_placement_category` (`category_id`),
  CONSTRAINT `chk_plugin_placement_visible` CHECK (`visible` IN (0, 1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='Plugin visibility, category, and ordering at a placement point';

-- +migrate Down

DROP TABLE IF EXISTS `plugin_placements`;
DROP TABLE IF EXISTS `plugin_category_placements`;
DROP TABLE IF EXISTS `plugin_audit_logs`;
DROP TABLE IF EXISTS `plugin_versions`;
DROP TABLE IF EXISTS `plugin_relations`;
DROP TABLE IF EXISTS `plugins`;
DROP TABLE IF EXISTS `plugin_categories`;
