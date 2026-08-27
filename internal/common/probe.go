package common

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// ProbePortCount 是一次画像采样使用的连续 UDP 端口数量。判定 NAT 类型
	// 需要多个样本：映射端口全部相同为 stable，否则 variable；5 是「判定
	// 置信度」与「画像耗时」的取舍，同时决定了服务器连续占用 5 个 UDP 端口。
	ProbePortCount = 5
	// MaxProbeDatagram 是 PROBE/PORT 单个 UDP datagram 的统一大小上限。
	MaxProbeDatagram = 256
)

// ProbeRequest 是客户端发往单个探测端口的无状态请求。
type ProbeRequest struct {
	Nonce   string
	ProbeID uint8
}

// ProbeResponse 是服务器回显 nonce、序号和观察到的源地址。
type ProbeResponse struct {
	Nonce      string
	ProbeID    uint8
	PublicIP   IPv4
	MappedPort Port
}

// EncodeProbeRequest 按规范生成不带 LF 的 PROBE datagram。
func EncodeProbeRequest(request ProbeRequest) ([]byte, error) {
	if err := validateProbeNonce(request.Nonce); err != nil {
		return nil, err
	}
	if request.ProbeID < 1 || request.ProbeID > ProbePortCount {
		return nil, errors.New("probe_id must be between 1 and 5")
	}
	return []byte(fmt.Sprintf("PROBE %s %d", request.Nonce, request.ProbeID)), nil
}

// ParseProbeRequest 严格解析一个完整 PROBE datagram；任何格式错误都应由端点静默丢弃。
func ParseProbeRequest(data []byte) (ProbeRequest, error) {
	if len(data) == 0 || len(data) > MaxProbeDatagram {
		return ProbeRequest{}, errors.New("PROBE datagram length is outside 1..256")
	}
	if !printableASCII(data) {
		return ProbeRequest{}, errors.New("PROBE datagram must contain printable ASCII")
	}
	fields := strings.Split(string(data), " ")
	if len(fields) != 3 || fields[0] != "PROBE" || fields[1] == "" || fields[2] == "" {
		return ProbeRequest{}, errors.New("PROBE requires exactly 3 fields")
	}
	if err := validateProbeNonce(fields[1]); err != nil {
		return ProbeRequest{}, err
	}
	probeID, err := parseProbeID(fields[2])
	if err != nil {
		return ProbeRequest{}, err
	}
	return ProbeRequest{Nonce: fields[1], ProbeID: uint8(probeID)}, nil
}

// EncodeProbeResponse 按规范生成不带 LF 的 PORT datagram。
func EncodeProbeResponse(response ProbeResponse) ([]byte, error) {
	if err := validateProbeNonce(response.Nonce); err != nil {
		return nil, err
	}
	if response.ProbeID < 1 || response.ProbeID > ProbePortCount {
		return nil, errors.New("probe_id must be between 1 and 5")
	}
	if err := response.PublicIP.Validate(); err != nil {
		return nil, err
	}
	if err := response.MappedPort.Validate(); err != nil {
		return nil, err
	}
	encoded := []byte(fmt.Sprintf("PORT %s %d %s %d", response.Nonce, response.ProbeID, response.PublicIP, response.MappedPort))
	if len(encoded) > MaxProbeDatagram {
		return nil, errors.New("PORT datagram exceeds 256 bytes")
	}
	return encoded, nil
}

// ParseProbeResponse 严格解析一个完整 PORT datagram。
func ParseProbeResponse(data []byte) (ProbeResponse, error) {
	if len(data) == 0 || len(data) > MaxProbeDatagram {
		return ProbeResponse{}, errors.New("PORT datagram length is outside 1..256")
	}
	if !printableASCII(data) {
		return ProbeResponse{}, errors.New("PORT datagram must contain printable ASCII")
	}
	fields := strings.Split(string(data), " ")
	if len(fields) != 5 || fields[0] != "PORT" {
		return ProbeResponse{}, errors.New("PORT requires exactly 5 fields")
	}
	if err := validateProbeNonce(fields[1]); err != nil {
		return ProbeResponse{}, err
	}
	probeID, err := parseProbeID(fields[2])
	if err != nil {
		return ProbeResponse{}, err
	}
	publicIP := IPv4(fields[3])
	if err := publicIP.Validate(); err != nil {
		return ProbeResponse{}, err
	}
	if !decimalDigits(fields[4]) {
		return ProbeResponse{}, errors.New("mapped_port must be unsigned decimal")
	}
	value, err := strconv.ParseUint(fields[4], 10, 16)
	if err != nil || value == 0 {
		return ProbeResponse{}, errors.New("mapped_port must be between 1 and 65535")
	}
	return ProbeResponse{Nonce: fields[1], ProbeID: uint8(probeID), PublicIP: publicIP, MappedPort: Port(value)}, nil
}

// validateProbeNonce 校验探测 nonce：恰好 16 个小写十六进制字符。
// nonce 由客户端随机生成、原样回显，长度与字符集的双重校验把回显
// 伪装（拿别人的探测响应冒充自己的）挡在解析层。
func validateProbeNonce(value string) error {
	if len(value) != 16 {
		return errors.New("probe nonce must contain exactly 16 lowercase hexadecimal characters")
	}
	for _, character := range []byte(value) {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return errors.New("probe nonce must contain lowercase hexadecimal characters")
		}
	}
	return nil
}

// parseProbeID 解析探测端口序号：无前导零的规范十进制，取值 1..5
// （与五个探测端口一一对应）。
func parseProbeID(value string) (int, error) {
	if !decimalDigits(value) || (len(value) > 1 && value[0] == '0') {
		return 0, errors.New("probe_id must be unsigned canonical decimal")
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > ProbePortCount {
		return 0, errors.New("probe_id must be between 1 and 5")
	}
	return parsed, nil
}

// printableASCII 判定报文是否全部为可打印 ASCII。PROBE/PORT 报文是单空格
// 分隔的明文协议，非可打印字节意味着这不是本协议或已被截断。
func printableASCII(value []byte) bool {
	for _, character := range value {
		if character < 0x20 || character > 0x7e {
			return false
		}
	}
	return true
}
