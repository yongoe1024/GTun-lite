package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"gtun-lite/internal/common"
)

// AdminAPI 承载管理端点：设备与网络的增删改查，以及三个链路操作
// （CONNECT / DISCONNECT / QUERY）。
//
// 用标准库 net/http 而非 Web 框架：全部负载是 JSON 序列化，没有模板渲染、
// 文件上传或正则路由的需求。Go 1.22 的 ServeMux 已支持「方法 + 路径参数」
// 模式匹配，够用；引框架换来的便利抵不上它带进来的间接依赖链。
//
// 刻意没有事件查询端点。状态变更只写结构化日志、不落库（见 Store 的注释），
// 因此不存在可供分页查询的事件表。需要事件时间线时正确做法是接日志检索，
// 而不是把表回来——表的成本落在「每次状态变更」这条热路径上，
// 而查询是低频的。
//
// 读端点（GET）直接读库；写端点先落库、再让 hub 重推受影响设备的全量配置。
// 配置组装在 hub 的 owner 命令内执行，两条写操作的「组装时刻」天然单调，
// 同一连接上不会出现旧配置后到（全量推送不变量的实现细节见 hub.pushConfig）。
type AdminAPI struct {
	hub    *Hub
	store  *Store
	config ServerConfig
	log    *slog.Logger
	retry  *AutoRetry
}

// NewAdminAPI 创建管理 API，并启动链路自动重试循环（见 AutoRetry：
// 开关默认关，循环空转的代价是每拍一次原子读）。
func NewAdminAPI(owner *Hub, store *Store, config ServerConfig, log *slog.Logger) *AdminAPI {
	api := &AdminAPI{hub: owner, store: store, config: config, log: log, retry: NewAutoRetry(owner, log)}
	return api
}

// Routes 注册全部管理端点。
func (api *AdminAPI) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	// GET / 是单文件管理页面（见 web.go 的内嵌说明）；/ready 供
	// systemd/容器探活；其余管理端点全部在 /api 下。
	mux.HandleFunc("GET /{$}", api.index)
	mux.HandleFunc("GET /ready", api.ready)
	mux.HandleFunc("GET /api/devices", api.listDevices)
	mux.HandleFunc("POST /api/devices/{device_id}/approve", api.approveDevice)
	mux.HandleFunc("DELETE /api/devices/{device_id}", api.deleteDevice)
	mux.HandleFunc("GET /api/networks", api.listNetworks)
	mux.HandleFunc("POST /api/networks", api.createNetwork)
	mux.HandleFunc("GET /api/networks/{network_id}", api.getNetwork)
	mux.HandleFunc("DELETE /api/networks/{network_id}", api.deleteNetwork)
	mux.HandleFunc("POST /api/networks/{network_id}/members", api.addMember)
	mux.HandleFunc("DELETE /api/networks/{network_id}/members/{device_id}", api.removeMember)
	mux.HandleFunc("POST /api/networks/{network_id}/peerings", api.createPeering)
	mux.HandleFunc("DELETE /api/networks/{network_id}/peerings/{peering_id}", api.deletePeering)
	mux.HandleFunc("GET /api/links", api.listLinks)
	mux.HandleFunc("POST /api/links/connect", api.connectLink)
	mux.HandleFunc("POST /api/links/disconnect", api.disconnectLink)
	mux.HandleFunc("POST /api/links/auto-retry", api.setAutoRetry)
	mux.HandleFunc("POST /api/devices/{device_id}/query", api.queryDevice)
	return mux
}

// writeJSON 统一输出 JSON 响应。
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// writeError 输出统一错误形状。code 是封闭集合，供脚本与页面分支。
func writeError(writer http.ResponseWriter, status int, code, detail string) {
	writeJSON(writer, status, map[string]string{"code": code, "message": detail})
}

// decodeBody 解析请求体到指定结构。字段类型错误、缺失必填都会在这里报出，
// 各 handler 拿到的是已通过 JSON 结构检查的值。
func decodeBody(writer http.ResponseWriter, request *http.Request, target any) bool {
	if err := json.NewDecoder(request.Body).Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_body", err.Error())
		return false
	}
	return true
}

// networkDetail 是 GET /api/networks/{id} 的响应体。
type networkDetail struct {
	Network  NetworkRow   `json:"network"`
	Members  []MemberRow  `json:"members"`
	Peerings []PeeringRow `json:"peerings"`
}

// listDevices 返回设备与实时在线性。在线性来自会话表快照，
// 与链路视图取自同一瞬间。
func (api *AdminAPI) listDevices(writer http.ResponseWriter, request *http.Request) {
	devices, _, err := api.hub.Snapshot(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"devices": devices})
}

// listNetworks 返回全部网络与成员数。
func (api *AdminAPI) listNetworks(writer http.ResponseWriter, request *http.Request) {
	networks, err := api.store.ListNetworks(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	type networkSummary struct {
		NetworkRow
		MemberCount int `json:"member_count"`
	}
	summaries := make([]networkSummary, 0, len(networks))
	for _, network := range networks {
		count, err := api.store.CountMembers(request.Context(), network.ID)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		summaries = append(summaries, networkSummary{NetworkRow: network, MemberCount: count})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"networks": summaries})
}

// createNetworkInput 是建网请求体。
type createNetworkInput struct {
	Name string `json:"name"`
	CIDR string `json:"cidr"`
}

// createNetwork 建网。ID 由服务器生成（UUID 截短，碰撞由主键约束兜底），
// CIDR 必须是 RFC1918 网络地址且前缀长度在配置范围内。
func (api *AdminAPI) createNetwork(writer http.ResponseWriter, request *http.Request) {
	var input createNetworkInput
	if !decodeBody(writer, request, &input) {
		return
	}
	if input.Name == "" || len(input.Name) > 128 {
		writeError(writer, http.StatusBadRequest, "invalid_name", "name must contain 1 to 128 bytes")
		return
	}
	cidr := common.IPv4CIDR(input.CIDR)
	if err := cidr.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_cidr", err.Error())
		return
	}
	prefix, _ := netip.ParsePrefix(input.CIDR)
	if prefix.Bits() < api.config.Limits.MinCIDRPrefix || prefix.Bits() > api.config.Limits.MaxCIDRPrefix {
		writeError(writer, http.StatusBadRequest, "invalid_cidr",
			fmt.Sprintf("cidr prefix must be between %d and %d", api.config.Limits.MinCIDRPrefix, api.config.Limits.MaxCIDRPrefix))
		return
	}
	id := common.GenerateNetworkID()
	if err := api.store.CreateNetwork(request.Context(), id, input.Name, cidr); err != nil {
		if isUniqueViolation(err) {
			writeError(writer, http.StatusConflict, "cidr_taken", "another network already uses this cidr")
			return
		}
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	network, err := api.store.GetNetwork(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, network)
}

// getNetwork 返回网络详情（成员与配对）。
func (api *AdminAPI) getNetwork(writer http.ResponseWriter, request *http.Request) {
	id := common.NetworkID(request.PathValue("network_id"))
	if !id.Valid() {
		writeError(writer, http.StatusBadRequest, "invalid_network_id", "network id must be 8 lowercase hex characters")
		return
	}
	network, err := api.store.GetNetwork(request.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusNotFound, "not_found", "no such network")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	members, err := api.store.ListMembers(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	peerings, err := api.store.ListPeerings(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, networkDetail{Network: network, Members: members, Peerings: peerings})
}

// deleteNetwork 删网。成员与配对级联删除；在线成员随后收到
// 「不属于任何网络」的空配置，内存里的相关链路条目一并清掉。
func (api *AdminAPI) deleteNetwork(writer http.ResponseWriter, request *http.Request) {
	id := common.NetworkID(request.PathValue("network_id"))
	if !id.Valid() {
		writeError(writer, http.StatusBadRequest, "invalid_network_id", "network id must be 8 lowercase hex characters")
		return
	}
	members, err := api.store.ListMembers(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// 配对清单必须在删除前取：级联删除之后就查不到了。
	peerings, err := api.store.ListPeerings(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	err = api.store.DeleteNetwork(request.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusNotFound, "not_found", "no such network")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	for _, member := range members {
		api.hub.PushConfig(request.Context(), member.DeviceID)
	}
	pruned := make([]common.Link, 0, len(peerings))
	for _, peering := range peerings {
		if pair, err := common.NewLink(peering.DeviceA, peering.DeviceB); err == nil {
			pruned = append(pruned, pair)
		}
	}
	api.hub.PruneLinks(request.Context(), pruned)
	writeJSON(writer, http.StatusOK, map[string]string{"status": "deleted"})
}

// addMemberInput 是入网请求体。
type addMemberInput struct {
	DeviceID string `json:"device_id"`
}

// addMember 把设备加入网络并分配虚拟 IP（取 CIDR 内第一个空闲主机地址）。
// 设备必须先注册过（devices 行存在）；一台设备同时只能属于一个网络，
// 这由 network_members.device_id 的 UNIQUE 约束表达。
func (api *AdminAPI) addMember(writer http.ResponseWriter, request *http.Request) {
	id := common.NetworkID(request.PathValue("network_id"))
	if !id.Valid() {
		writeError(writer, http.StatusBadRequest, "invalid_network_id", "network id must be 8 lowercase hex characters")
		return
	}
	var input addMemberInput
	if !decodeBody(writer, request, &input) {
		return
	}
	device, err := common.ParseDeviceID(input.DeviceID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_device_id", err.Error())
		return
	}
	network, err := api.store.GetNetwork(request.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusNotFound, "not_found", "no such network")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if _, err := api.store.Membership(request.Context(), device); err == nil {
		writeError(writer, http.StatusConflict, "already_member", "device already belongs to a network")
		return
	} else if !errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// 注册审批制：未批准的设备只存在于内存待审批表，库里没有行，
	// 不能入网。显式拒绝而不是等外键违约报 500。
	approved, err := api.store.HasDevice(request.Context(), device)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if !approved {
		writeError(writer, http.StatusConflict, "not_approved", "device has not been approved; approve its registration first")
		return
	}
	count, err := api.store.CountMembers(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if count >= api.config.Limits.MaxDevicesPerNetwork {
		writeError(writer, http.StatusConflict, "network_full",
			fmt.Sprintf("network already has %d of %d devices", count, api.config.Limits.MaxDevicesPerNetwork))
		return
	}
	members, err := api.store.ListMembers(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	address, err := allocateAddress(network.CIDR, members)
	if err != nil {
		writeError(writer, http.StatusConflict, "address_exhausted", err.Error())
		return
	}
	if err := api.store.AddMember(request.Context(), id, device, address); err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// 新成员改变了全部老成员的 peers.online 视图，整网重推。
	for _, member := range members {
		api.hub.PushConfig(request.Context(), member.DeviceID)
	}
	api.hub.PushConfig(request.Context(), device)
	created, err := api.store.Membership(request.Context(), device)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, created)
}

// removeMember 把设备移出网络，其配对级联删除，相关链路条目清掉，
// 剩余成员与离开者都收到新配置。
func (api *AdminAPI) removeMember(writer http.ResponseWriter, request *http.Request) {
	id := common.NetworkID(request.PathValue("network_id"))
	if !id.Valid() {
		writeError(writer, http.StatusBadRequest, "invalid_network_id", "network id must be 8 lowercase hex characters")
		return
	}
	device, err := common.ParseDeviceID(request.PathValue("device_id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_device_id", err.Error())
		return
	}
	// 先取成员与配对清单，删除之后就查不全了。移除成员经外键级联只删
	// 它本人的配对，因此 PruneLinks 同样只限涉及它的对——把全网络的对
	// 都清掉会白白丢弃无关链路的最后已知状态。
	members, err := api.store.ListMembers(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	peerings, err := api.store.ListPeerings(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	var pruned []common.Link
	for _, peering := range peerings {
		if peering.DeviceA != device && peering.DeviceB != device {
			continue
		}
		if link, err := common.NewLink(peering.DeviceA, peering.DeviceB); err == nil {
			pruned = append(pruned, link)
		}
	}
	err = api.store.RemoveMember(request.Context(), id, device)
	if errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusNotFound, "not_found", "device is not a member of this network")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	for _, member := range members {
		if member.DeviceID != device {
			api.hub.PushConfig(request.Context(), member.DeviceID)
		}
	}
	api.hub.PushConfig(request.Context(), device)
	api.hub.PruneLinks(request.Context(), pruned)
	writeJSON(writer, http.StatusOK, map[string]string{"status": "removed"})
}

// createPeeringInput 是建配对请求体。
type createPeeringInput struct {
	DeviceA string `json:"device_a"`
	DeviceB string `json:"device_b"`
}

// createPeering 在同网络的两台成员设备间建配对。两侧都收到含新对端的全量配置。
func (api *AdminAPI) createPeering(writer http.ResponseWriter, request *http.Request) {
	id := common.NetworkID(request.PathValue("network_id"))
	if !id.Valid() {
		writeError(writer, http.StatusBadRequest, "invalid_network_id", "network id must be 8 lowercase hex characters")
		return
	}
	var input createPeeringInput
	if !decodeBody(writer, request, &input) {
		return
	}
	deviceA, err := common.ParseDeviceID(input.DeviceA)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_device_id", "device_a: "+err.Error())
		return
	}
	deviceB, err := common.ParseDeviceID(input.DeviceB)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_device_id", "device_b: "+err.Error())
		return
	}
	link, err := common.NewLink(deviceA, deviceB)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_pair", err.Error())
		return
	}
	// 两台都必须已是本网络成员；配对的 FK 指向 network_members。
	members, err := api.store.ListMembers(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	inNetwork := map[common.DeviceID]bool{}
	for _, member := range members {
		inNetwork[member.DeviceID] = true
	}
	if !inNetwork[link[0]] || !inNetwork[link[1]] {
		writeError(writer, http.StatusConflict, "not_members", "both devices must be members of this network")
		return
	}
	peering := common.GeneratePeeringID()
	if err := api.store.CreatePeering(request.Context(), id, link[0], link[1], peering); err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	api.hub.PushConfig(request.Context(), link[0])
	api.hub.PushConfig(request.Context(), link[1])
	writeJSON(writer, http.StatusCreated, map[string]any{"peering_id": peering})
}

// deletePeering 删配对。两侧收到不再含对方的新配置；内存链路条目清除。
// 打洞中的尝试由客户端随新配置自行停止（尝试只存在于配置之内）。
func (api *AdminAPI) deletePeering(writer http.ResponseWriter, request *http.Request) {
	id := common.NetworkID(request.PathValue("network_id"))
	if !id.Valid() {
		writeError(writer, http.StatusBadRequest, "invalid_network_id", "network id must be 8 lowercase hex characters")
		return
	}
	peeringID := common.PeeringID(request.PathValue("peering_id"))
	if !peeringID.Valid() {
		writeError(writer, http.StatusBadRequest, "invalid_peering_id", "peering id must be 32 lowercase hex characters")
		return
	}
	peerings, err := api.store.ListPeerings(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	var pair common.Link
	found := false
	for _, peering := range peerings {
		if peering.PeeringID == peeringID {
			if pair, err = common.NewLink(peering.DeviceA, peering.DeviceB); err == nil {
				found = true
			}
			break
		}
	}
	if !found {
		writeError(writer, http.StatusNotFound, "not_found", "no such peering in this network")
		return
	}
	err = api.store.RemovePeering(request.Context(), id, peeringID)
	if errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusNotFound, "not_found", "no such peering in this network")
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	api.hub.PushConfig(request.Context(), pair[0])
	api.hub.PushConfig(request.Context(), pair[1])
	api.hub.PruneLinks(request.Context(), []common.Link{pair})
	writeJSON(writer, http.StatusOK, map[string]string{"status": "deleted"})
}

// listLinks 返回全部链路视图：在线性 + 最后已知状态 + 采集时刻。
// auto_retry 一并带回，管理页刷新后开关状态不漂移。
func (api *AdminAPI) listLinks(writer http.ResponseWriter, request *http.Request) {
	_, links, err := api.hub.Snapshot(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"links": links, "auto_retry": api.retry.Enabled()})
}

// setAutoRetry 开关「链路失败自动重试」全局循环（见 AutoRetry）。纯开关：
// 开 = 每 AutoRetryInterval 遍历一次内存链路表，断开（IDLE）的重发
// CONNECT；关 = 循环空转。语义与边界（手动断链会被扫回、重启归零）
// 见 autoretry.go 顶部注释与管理页提示语。
func (api *AdminAPI) setAutoRetry(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Enable bool `json:"enable"`
	}
	if !decodeBody(writer, request, &input) {
		return
	}
	api.retry.Set(input.Enable)
	status := "auto_retry_disabled"
	if input.Enable {
		status = "auto_retry_enabled"
	}
	writeJSON(writer, http.StatusAccepted, map[string]string{"status": status})
}

// linkPairInput 是链路操作请求体：两台设备即一条链路（设备对在库内
// 唯一确定一条配对，不需要网络与配对 ID）。
type linkPairInput struct {
	DeviceA string `json:"device_a"`
	DeviceB string `json:"device_b"`
}

// parseLinkInput 解析并规范化设备对，失败时已写好错误响应。
func parseLinkInput(writer http.ResponseWriter, request *http.Request) (common.Link, bool) {
	var input linkPairInput
	if !decodeBody(writer, request, &input) {
		return common.Link{}, false
	}
	deviceA, err := common.ParseDeviceID(input.DeviceA)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_device_id", "device_a: "+err.Error())
		return common.Link{}, false
	}
	deviceB, err := common.ParseDeviceID(input.DeviceB)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_device_id", "device_b: "+err.Error())
		return common.Link{}, false
	}
	link, err := common.NewLink(deviceA, deviceB)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_pair", err.Error())
		return common.Link{}, false
	}
	return link, true
}

// connectLink 下发 CONNECT。单侧离线返回 409 PEER_OFFLINE 且状态不变
// （不变量 2）；无配对返回 404。
func (api *AdminAPI) connectLink(writer http.ResponseWriter, request *http.Request) {
	link, ok := parseLinkInput(writer, request)
	if !ok {
		return
	}
	err := api.hub.IssueConnect(request.Context(), link)
	switch {
	case errors.Is(err, ErrPeerOffline):
		writeError(writer, http.StatusConflict, "peer_offline", "both devices must be online to issue commands")
	case errors.Is(err, ErrNotFound):
		writeError(writer, http.StatusNotFound, "not_found", "no peering exists for this device pair")
	case err != nil:
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
	default:
		writeJSON(writer, http.StatusAccepted, map[string]string{"status": "connect_issued"})
	}
}

// disconnectLink 下发 DISCONNECT，在线性要求与 CONNECT 相同。
func (api *AdminAPI) disconnectLink(writer http.ResponseWriter, request *http.Request) {
	link, ok := parseLinkInput(writer, request)
	if !ok {
		return
	}
	err := api.hub.IssueDisconnect(request.Context(), link)
	switch {
	case errors.Is(err, ErrPeerOffline):
		writeError(writer, http.StatusConflict, "peer_offline", "both devices must be online to issue commands")
	case errors.Is(err, ErrNotFound):
		writeError(writer, http.StatusNotFound, "not_found", "no peering exists for this device pair")
	case err != nil:
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
	default:
		writeJSON(writer, http.StatusAccepted, map[string]string{"status": "disconnect_issued"})
	}
}

// queryDevice 向单台在线设备下发 QUERY。响应是异步的：设备随后的全量
// state_report 会刷新链路视图，管理页面通过 GET /api/links 看到。
// 目标设备离线时如实拒绝——查询一个不在线的事实来源没有意义，
// 展示层应显示最后已知状态而非发起注定失败的询问。
func (api *AdminAPI) queryDevice(writer http.ResponseWriter, request *http.Request) {
	device, err := common.ParseDeviceID(request.PathValue("device_id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_device_id", err.Error())
		return
	}
	err = api.hub.IssueQuery(request.Context(), device)
	switch {
	case errors.Is(err, ErrDeviceOffline):
		writeError(writer, http.StatusConflict, "peer_offline", "device is not online")
	case errors.Is(err, ErrSessionNotCurrent):
		writeError(writer, http.StatusConflict, "peer_offline", "device connection is being replaced")
	case err != nil:
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
	default:
		writeJSON(writer, http.StatusAccepted, map[string]string{"status": "query_issued"})
	}
}

// approveDevice 把待审批设备落库为正式设备（注册审批制）。
// 幂等语义由 hub 保证：不在待审批表（未注册、已断开、已批准）→ 404。
func (api *AdminAPI) approveDevice(writer http.ResponseWriter, request *http.Request) {
	device, err := common.ParseDeviceID(request.PathValue("device_id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_device_id", err.Error())
		return
	}
	if err := api.hub.ApproveDevice(request.Context(), device); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(writer, http.StatusNotFound, "not_found", "no pending registration for this device")
			return
		}
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "approved"})
}

// deleteDevice 删除设备并级联清退：外键级联清掉网络成员关系与配对，
// 内存链路状态同步清除，受影响对端重推新配置，在线设备踢下线。
// 未审批设备不在库里 → 404（待加入列表没有删除操作，断开即消失）。
func (api *AdminAPI) deleteDevice(writer http.ResponseWriter, request *http.Request) {
	device, err := common.ParseDeviceID(request.PathValue("device_id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_device_id", err.Error())
		return
	}
	// 删除前收集清退上下文：设备至多属于一个网络（device_id UNIQUE），
	// 其配对与受影响对端在删除之后就查不全了。
	member, err := api.store.Membership(request.Context(), device)
	if err != nil && !errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	var peers []common.DeviceID
	var pruned []common.Link
	if err == nil {
		peerings, err := api.store.ListPeerings(request.Context(), member.NetworkID)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		for _, peering := range peerings {
			if peering.DeviceA != device && peering.DeviceB != device {
				continue
			}
			if link, err := common.NewLink(peering.DeviceA, peering.DeviceB); err == nil {
				pruned = append(pruned, link)
			}
			other := peering.DeviceB
			if peering.DeviceB == device {
				other = peering.DeviceA
			}
			peers = append(peers, other)
		}
	}
	if err := api.store.DeleteDevice(request.Context(), device); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(writer, http.StatusNotFound, "not_found", "no such device")
			return
		}
		writeError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if len(pruned) > 0 {
		api.hub.PruneLinks(request.Context(), pruned)
	}
	for _, peer := range peers {
		api.hub.PushConfig(request.Context(), peer)
	}
	if api.hub.IsOnline(request.Context(), device) {
		_ = api.hub.Kick(request.Context(), device)
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "deleted"})
}

// ready 返回进程与数据库的健康状态（探活端点，无业务语义）。
func (api *AdminAPI) ready(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := api.store.Ping(ctx); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "db_unreachable"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

// allocateAddress 取 CIDR 内第一个未占用的主机地址，跳过网络地址与
// 广播地址。地址从网段低位往上找，分配结果稳定可预测。
func allocateAddress(cidr common.IPv4CIDR, members []MemberRow) (common.IPv4, error) {
	prefix, err := netip.ParsePrefix(string(cidr))
	if err != nil {
		return "", err
	}
	taken := make(map[netip.Addr]struct{}, len(members))
	for _, member := range members {
		taken[netip.MustParseAddr(string(member.VirtualIP))] = struct{}{}
	}
	address := prefix.Addr().Next()
	for address.IsValid() && prefix.Contains(address) {
		if address.Is4() {
			if _, occupied := taken[address]; !occupied {
				// 跳过广播地址：它不是可用主机地址。
				if address != common.LastIPv4Address(prefix) {
					return common.IPv4(address.String()), nil
				}
			}
		}
		address = address.Next()
	}
	return "", fmt.Errorf("no free host address in %s", cidr)
}
