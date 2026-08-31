-- GTun-Lite 服务端 schema。
--
-- 全部 4 张表，只存配置：设备、网络、成员、配对。启动时校验表集合必须与此
-- 完全一致（见 store.go 的 validateSchema），多一张少一张都拒绝启动。
--
-- 这里没有的两样东西，缺席是设计结论而非遗漏：
--
--   1. 没有状态变更事件表。状态变更只写结构化日志。事件表的成本（写事务、
--      保留期裁剪、schema 演进）全部落在「每次状态变更」这条热路径上，
--      而它的读取只是低频的运维视图，日志能提供同样的信息。
--   2. 没有配置代次列（generation / revision）。配置推送始终全量且幂等，
--      因此不需要版本号来判断新旧。前提写在客户端 manager.go 与
--      common.NetworkConfig 的注释里：改成增量推送必须先把代次加回来。
--
-- 链路状态不在这里，也不该加进来：它是服务端的纯内存状态，重启必然全丢，
-- 由客户端重连时全量上报重建。落库只会得到一份可能已经过期的记录，
-- 而客户端手里那份永远是准的。

PRAGMA foreign_keys = ON;

BEGIN;

CREATE TABLE IF NOT EXISTS devices (
    -- device_id 是 UUID 的规范文本形式：36 字符、小写、连字符在固定位置。
    -- GLOB 逐段限定十六进制字符，把连字符位置也钉死，避免同一身份以不同
    -- 写法存成两行主键。
    device_id TEXT PRIMARY KEY
        CHECK (
            device_id GLOB '[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
        ),
    name TEXT NOT NULL,
    platform TEXT NOT NULL
        CHECK (platform IN ('linux', 'darwin', 'windows', 'android')),
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS networks (
    network_id TEXT PRIMARY KEY
        CHECK (
            length(network_id) = 8
            AND network_id NOT GLOB '*[^0-9a-f]*'
        ),
    name TEXT NOT NULL,
    cidr TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS network_members (
    network_id TEXT NOT NULL,
    device_id TEXT NOT NULL UNIQUE,
    virtual_ip TEXT NOT NULL,
    joined_at TEXT NOT NULL,
    PRIMARY KEY (network_id, device_id),
    UNIQUE (network_id, virtual_ip),
    FOREIGN KEY (network_id) REFERENCES networks (network_id) ON DELETE CASCADE,
    FOREIGN KEY (device_id) REFERENCES devices (device_id) ON DELETE CASCADE
);

-- network_peerings 只存配对身份，不存任何与「某次建链尝试」相关的列。
-- 一次尝试由内存里的 LinkToken 标识，随失败或拆链丢弃，没有跨重启保留的必要；
-- 而尝试之间不存在新旧序关系，也就没有需要落库的编号或水位。
--
-- CHECK (device_a < device_b) 与 Link 的字典序规范化对应：同一对设备在库里
-- 只有一行，不会出现 (a,b) 和 (b,a) 两条记录。
CREATE TABLE IF NOT EXISTS network_peerings (
    network_id TEXT NOT NULL,
    device_a TEXT NOT NULL,
    device_b TEXT NOT NULL,
    peering_id TEXT NOT NULL UNIQUE
        CHECK (
            length(peering_id) = 32
            AND peering_id NOT GLOB '*[^0-9a-f]*'
        ),
    created_at TEXT NOT NULL,
    PRIMARY KEY (network_id, device_a, device_b),
    CHECK (device_a < device_b),
    FOREIGN KEY (network_id, device_a)
        REFERENCES network_members (network_id, device_id) ON DELETE CASCADE,
    FOREIGN KEY (network_id, device_b)
        REFERENCES network_members (network_id, device_id) ON DELETE CASCADE
);

COMMIT;
