package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gtun-lite/internal/common"
	"gtun-lite/schema"
	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
)

var (
	// ErrEmptyDatabase 表示数据库文件存在但没有任何表。
	ErrEmptyDatabase = errors.New("database contains no tables")
	// ErrSchemaMismatch 表示表集合与当前版本要求不符。
	ErrSchemaMismatch = errors.New("database schema does not match this version")
	// ErrNotFound 表示按 ID 查询的行不存在。
	ErrNotFound = errors.New("row not found")
)

// requiredTables 是 store 启动时无条件要求的全部表。数量固定为 4：
// 库里多一张或少一张都算 schema 不匹配，直接拒绝启动。
var requiredTables = []string{"devices", "networks", "network_members", "network_peerings"}

// Store 持有配置表的连接。
//
// 只有配置落库：设备、网络、成员、配对。链路状态不在这里，也不该在这里——
// 它是 hub 的纯内存状态（见 link.go）。状态变更是热路径，写库会给它加上
// 事务开销、保留期裁剪与 schema 演进成本，而这些数据的读取是低频的运维需求。
//
// 写入者有两类：hub 的 owner goroutine（注册、会话相关）与管理端点所在的
// HTTP handler goroutine（建网、加成员等）。二者不经我们串行化，正确性由
// SQLite 自身的写锁加 DSN 里的 busy_timeout 兜住；读取（管理页面的 GET）
// 在 WAL 下与写入并发。
type Store struct {
	db *sql.DB
}

// OpenStore 打开（或首次创建）数据库并校验 schema。
//
// 空库是首次部署：用内嵌的 schema/server.sql 建表后重新校验。
// 非空但表集合不对是版本或部署错误，拒绝启动——自动迁移会掩盖
// 「跑错了版本」这个真正的问题。
//
// foreign_keys 与 busy_timeout 通过 DSN 下发：database/sql 会池化连接，
// 逐连接的 PRAGMA 必须挂在驱动连接参数上，靠执行一次 PRAGMA 语句
// 只会影响当时的那一个连接。
func OpenStore(ctx context.Context, path string) (*Store, error) {
	// WAL 让读连接不被写事务阻塞；busy_timeout 由 DSN 换算成毫秒。
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	err = validateSchema(ctx, db)
	if errors.Is(err, ErrEmptyDatabase) {
		if _, execErr := db.ExecContext(ctx, schema.ServerSQL); execErr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("create schema: %w", execErr)
		}
		err = validateSchema(ctx, db)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库连接。
func (store *Store) Close() error {
	return store.db.Close()
}

// isUniqueViolation 按驱动的错误码判定 UNIQUE 约束冲突。错误可能被
// fmt.Errorf 包装过，用 errors.As 而不是匹配错误文本——文本会随驱动
// 版本与包装层数变化，错误码不会。
func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
	}
	return false
}

// validateSchema 要求库里的表集合与 requiredTables 完全一致，随后执行完整性检查。
// 启动即校验是刻意的：schema 不符属于部署错误，此时拒绝启动比运行到某个
// 具体查询才失败更容易定位。
func validateSchema(ctx context.Context, db *sql.DB) error {
	// 一次读出实际表集合，再与 requiredTables 比对。不写死表名列表在 SQL 里：
	// 表名只在 requiredTables 一处声明，避免两处不同步。
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return fmt.Errorf("list schema tables: %w", err)
	}
	defer rows.Close()
	actual := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		actual[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// 空库与「表集合不对」分开报：空库是首次部署，调用方据此决定是否建表；
	// 表集合不对是版本或部署错误，不能自动修。
	if len(actual) == 0 {
		return ErrEmptyDatabase
	}
	for _, name := range requiredTables {
		if _, ok := actual[name]; !ok {
			return fmt.Errorf("%w: missing table %s", ErrSchemaMismatch, name)
		}
	}
	if len(actual) != len(requiredTables) {
		return fmt.Errorf("%w: database has %d tables, want exactly %d", ErrSchemaMismatch, len(actual), len(requiredTables))
	}
	return validateIntegrity(ctx, db)
}

// validateIntegrity 同时检查 SQLite 页结构和所有外键引用。
func validateIntegrity(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		if result != "ok" {
			return fmt.Errorf("integrity_check failed: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	foreignRows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer foreignRows.Close()
	if foreignRows.Next() {
		return errors.New("foreign_key_check reported a violation")
	}
	return foreignRows.Err()
}

// DeviceRow 是 devices 表的一行。
type DeviceRow struct {
	ID        common.DeviceID `json:"device_id"`
	Name      string          `json:"name"`
	Platform  string          `json:"platform"`
	CreatedAt string          `json:"created_at"`
}

// NetworkRow 是 networks 表的一行。
type NetworkRow struct {
	ID        common.NetworkID `json:"id"`
	Name      string           `json:"name"`
	CIDR      common.IPv4CIDR  `json:"cidr"`
	CreatedAt string           `json:"created_at"`
}

// MemberRow 是 network_members 表的一行，附带设备的可读名（JOIN devices），
// 管理页面展示成员时需要名字而不只是 ID。
type MemberRow struct {
	NetworkID  common.NetworkID `json:"network_id"`
	DeviceID   common.DeviceID  `json:"device_id"`
	DeviceName string           `json:"device_name"`
	VirtualIP  common.IPv4      `json:"virtual_ip"`
	JoinedAt   string           `json:"joined_at"`
}

// PeeringRow 是 network_peerings 表的一行，附带两侧设备名。
type PeeringRow struct {
	NetworkID  common.NetworkID `json:"network_id"`
	DeviceA    common.DeviceID  `json:"device_a"`
	NameA      string           `json:"name_a"`
	DeviceB    common.DeviceID  `json:"device_b"`
	NameB      string           `json:"name_b"`
	PeeringID  common.PeeringID `json:"peering_id"`
	VirtualIPA common.IPv4      `json:"virtual_ip_a"`
	VirtualIPB common.IPv4      `json:"virtual_ip_b"`
	CreatedAt  string           `json:"created_at"`
}

// nowRFC3339 统一生成 created_at / joined_at 的文本时间。
// RFC3339 是 SQLite TEXT 时间戳的事实标准，字典序与时间序一致。
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// UpsertDevice 注册即 upsert：新设备插入，老设备刷新 name 与 platform。
// 设备身份由客户端持久持有，服务器不生成也不修改它。
// 单条消息上限是全局常量（见 common.MaxControlMessageBytes），不做每设备
// 配置位：上限挡的是错误对端与超长行，与设备是谁无关。
func (store *Store) UpsertDevice(ctx context.Context, id common.DeviceID, name, platform string) error {
	_, err := store.db.ExecContext(ctx, `
		INSERT INTO devices (device_id, name, platform, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET name = excluded.name, platform = excluded.platform`,
		string(id), name, platform, nowRFC3339())
	if err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	return nil
}

// DeleteDevice 删除设备行。调用方负责前置检查：设备必须已离开网络、
// 且控制连接不在线（在线客户端重连时会 upsert 复活该行）。
func (store *Store) DeleteDevice(ctx context.Context, id common.DeviceID) error {
	result, err := store.db.ExecContext(ctx, `DELETE FROM devices WHERE device_id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("delete device: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrNotFound
	}
	return nil
}

// Ping 验证数据库连通（/ready 探活用）。
func (store *Store) Ping(ctx context.Context) error {
	return store.db.PingContext(ctx)
}

// ListDevices 返回全部设备，按创建顺序。
func (store *Store) ListDevices(ctx context.Context) ([]DeviceRow, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT device_id, name, platform, created_at FROM devices ORDER BY created_at, device_id`)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()
	var devices []DeviceRow
	for rows.Next() {
		var row DeviceRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Platform, &row.CreatedAt); err != nil {
			return nil, err
		}
		devices = append(devices, row)
	}
	return devices, rows.Err()
}

// CreateNetwork 插入一个网络。CIDR 的 UNIQUE 约束替我们拒绝重复网段。
func (store *Store) CreateNetwork(ctx context.Context, id common.NetworkID, name string, cidr common.IPv4CIDR) error {
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO networks (network_id, name, cidr, created_at) VALUES (?, ?, ?, ?)`,
		string(id), name, string(cidr), nowRFC3339())
	if err != nil {
		return fmt.Errorf("create network: %w", err)
	}
	return nil
}

// ListNetworks 返回全部网络，按创建顺序。
func (store *Store) ListNetworks(ctx context.Context) ([]NetworkRow, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT network_id, name, cidr, created_at FROM networks ORDER BY created_at, network_id`)
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	defer rows.Close()
	var networks []NetworkRow
	for rows.Next() {
		var row NetworkRow
		if err := rows.Scan(&row.ID, &row.Name, &row.CIDR, &row.CreatedAt); err != nil {
			return nil, err
		}
		networks = append(networks, row)
	}
	return networks, rows.Err()
}

// GetNetwork 按 ID 返回网络行。
func (store *Store) GetNetwork(ctx context.Context, id common.NetworkID) (NetworkRow, error) {
	var row NetworkRow
	err := store.db.QueryRowContext(ctx,
		`SELECT network_id, name, cidr, created_at FROM networks WHERE network_id = ?`, string(id)).
		Scan(&row.ID, &row.Name, &row.CIDR, &row.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return NetworkRow{}, ErrNotFound
	}
	if err != nil {
		return NetworkRow{}, fmt.Errorf("get network: %w", err)
	}
	return row, nil
}

// DeleteNetwork 删除网络，成员与配对经外键级联删除。
// 调用方（hub）负责先通知在线成员重推空配置。
func (store *Store) DeleteNetwork(ctx context.Context, id common.NetworkID) error {
	result, err := store.db.ExecContext(ctx, `DELETE FROM networks WHERE network_id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("delete network: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrNotFound
	}
	return nil
}

// ListMembers 返回网络全部成员，附带设备名，按加入顺序。
func (store *Store) ListMembers(ctx context.Context, network common.NetworkID) ([]MemberRow, error) {
	rows, err := store.db.QueryContext(ctx, `
		SELECT m.network_id, m.device_id, d.name, m.virtual_ip, m.joined_at
		FROM network_members m JOIN devices d ON d.device_id = m.device_id
		WHERE m.network_id = ? ORDER BY m.joined_at, m.device_id`, string(network))
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	var members []MemberRow
	for rows.Next() {
		var row MemberRow
		if err := rows.Scan(&row.NetworkID, &row.DeviceID, &row.DeviceName, &row.VirtualIP, &row.JoinedAt); err != nil {
			return nil, err
		}
		members = append(members, row)
	}
	return members, rows.Err()
}

// CountMembers 返回网络当前成员数，供容量上限检查。
func (store *Store) CountMembers(ctx context.Context, network common.NetworkID) (int, error) {
	var count int
	err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM network_members WHERE network_id = ?`, string(network)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count members: %w", err)
	}
	return count, nil
}

// Membership 返回设备的成员关系。device_id 在 network_members 上有 UNIQUE
// 约束，一台设备同一时刻最多属于一个网络——这是 schema 层表达的架构决定：
// 下发配置的形状是单个 Network（见 common.NetworkConfig），而不是网络列表。
func (store *Store) Membership(ctx context.Context, device common.DeviceID) (MemberRow, error) {
	var row MemberRow
	err := store.db.QueryRowContext(ctx, `
		SELECT m.network_id, m.device_id, d.name, m.virtual_ip, m.joined_at
		FROM network_members m JOIN devices d ON d.device_id = m.device_id
		WHERE m.device_id = ?`, string(device)).
		Scan(&row.NetworkID, &row.DeviceID, &row.DeviceName, &row.VirtualIP, &row.JoinedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MemberRow{}, ErrNotFound
	}
	if err != nil {
		return MemberRow{}, fmt.Errorf("membership: %w", err)
	}
	return row, nil
}

// AddMember 把设备加入网络并占用给定虚拟 IP。
// 分配算法（挑第一个空闲地址）在 hub 里做，store 只负责写入与约束兜底。
func (store *Store) AddMember(ctx context.Context, network common.NetworkID, device common.DeviceID, ip common.IPv4) error {
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO network_members (network_id, device_id, virtual_ip, joined_at) VALUES (?, ?, ?, ?)`,
		string(network), string(device), string(ip), nowRFC3339())
	if err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	return nil
}

// RemoveMember 把设备移出网络。其配对经外键级联删除。
func (store *Store) RemoveMember(ctx context.Context, network common.NetworkID, device common.DeviceID) error {
	result, err := store.db.ExecContext(ctx,
		`DELETE FROM network_members WHERE network_id = ? AND device_id = ?`, string(network), string(device))
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrNotFound
	}
	return nil
}

// CreatePeering 建立一对成员的配对。
func (store *Store) CreatePeering(ctx context.Context, network common.NetworkID, a, b common.DeviceID, peering common.PeeringID) error {
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO network_peerings (network_id, device_a, device_b, peering_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		string(network), string(a), string(b), string(peering), nowRFC3339())
	if err != nil {
		return fmt.Errorf("create peering: %w", err)
	}
	return nil
}

// ListPeerings 返回网络全部配对，附带两侧设备名与虚拟 IP，按创建顺序。
// device_a < device_b 由 schema 的 CHECK 维持，与 common.Link 的字典序一致。
func (store *Store) ListPeerings(ctx context.Context, network common.NetworkID) ([]PeeringRow, error) {
	rows, err := store.db.QueryContext(ctx, `
		SELECT p.network_id, p.device_a, da.name, ma.virtual_ip, p.device_b, db.name, mb.virtual_ip, p.peering_id, p.created_at
		FROM network_peerings p
		JOIN devices da ON da.device_id = p.device_a
		JOIN devices db ON db.device_id = p.device_b
		JOIN network_members ma ON ma.device_id = p.device_a
		JOIN network_members mb ON mb.device_id = p.device_b
		WHERE p.network_id = ? ORDER BY p.created_at, p.peering_id`, string(network))
	if err != nil {
		return nil, fmt.Errorf("list peerings: %w", err)
	}
	defer rows.Close()
	var peerings []PeeringRow
	for rows.Next() {
		var row PeeringRow
		if err := rows.Scan(&row.NetworkID, &row.DeviceA, &row.NameA, &row.VirtualIPA, &row.DeviceB, &row.NameB, &row.VirtualIPB, &row.PeeringID, &row.CreatedAt); err != nil {
			return nil, err
		}
		peerings = append(peerings, row)
	}
	return peerings, rows.Err()
}

// RemovePeering 删除配对。
func (store *Store) RemovePeering(ctx context.Context, network common.NetworkID, peering common.PeeringID) error {
	result, err := store.db.ExecContext(ctx,
		`DELETE FROM network_peerings WHERE network_id = ? AND peering_id = ?`, string(network), string(peering))
	if err != nil {
		return fmt.Errorf("remove peering: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrNotFound
	}
	return nil
}

// PeeringByID 按配对 ID 查找配对行。状态上报按 peering_id 索引，
// 服务器由此把它换算回 Link 的两台设备。
func (store *Store) PeeringByID(ctx context.Context, peering common.PeeringID) (PeeringRow, error) {
	var row PeeringRow
	err := store.db.QueryRowContext(ctx, `
		SELECT p.network_id, p.device_a, da.name, ma.virtual_ip, p.device_b, db.name, mb.virtual_ip, p.peering_id, p.created_at
		FROM network_peerings p
		JOIN devices da ON da.device_id = p.device_a
		JOIN devices db ON db.device_id = p.device_b
		JOIN network_members ma ON ma.device_id = p.device_a
		JOIN network_members mb ON mb.device_id = p.device_b
		WHERE p.peering_id = ?`, string(peering)).
		Scan(&row.NetworkID, &row.DeviceA, &row.NameA, &row.VirtualIPA, &row.DeviceB, &row.NameB, &row.VirtualIPB, &row.PeeringID, &row.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PeeringRow{}, ErrNotFound
	}
	if err != nil {
		return PeeringRow{}, fmt.Errorf("peering by id: %w", err)
	}
	return row, nil
}

// PeeringForDevicePair 按字典序规范化的设备对查找配对。
// 一台设备最多属于一个网络，设备对在库内唯一确定一条配对。
// CONNECT/DISCONNECT 下发前用它换取配对身份与两端的名称、虚拟 IP。
func (store *Store) PeeringForDevicePair(ctx context.Context, pair common.Link) (PeeringRow, error) {
	var row PeeringRow
	err := store.db.QueryRowContext(ctx, `
		SELECT p.network_id, p.device_a, da.name, ma.virtual_ip, p.device_b, db.name, mb.virtual_ip, p.peering_id, p.created_at
		FROM network_peerings p
		JOIN devices da ON da.device_id = p.device_a
		JOIN devices db ON db.device_id = p.device_b
		JOIN network_members ma ON ma.device_id = p.device_a
		JOIN network_members mb ON mb.device_id = p.device_b
		WHERE p.device_a = ? AND p.device_b = ?`, string(pair[0]), string(pair[1])).
		Scan(&row.NetworkID, &row.DeviceA, &row.NameA, &row.VirtualIPA, &row.DeviceB, &row.NameB, &row.VirtualIPB, &row.PeeringID, &row.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PeeringRow{}, ErrNotFound
	}
	if err != nil {
		return PeeringRow{}, fmt.Errorf("peering for device pair: %w", err)
	}
	return row, nil
}
