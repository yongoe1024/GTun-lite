package client

import (
	"fmt"
	"os"

	"gtun-lite/internal/common"
)

// LoadIdentity 读取设备身份文件；文件不存在时生成新身份并写回。
//
// 身份由客户端自己持久持有，服务器只登记不生成——设备重装服务器后
// 身份不变，链路历史与配对关系才能延续。
//
// 写回经同目录临时文件 + rename 安装：截断式直写一旦在写到一半时崩溃，
// 会留下半截身份文件，此后每次启动都在解析处失败，只能人工删文件；
// rename 保证目标路径上任一时刻看到的都是完整内容。临时文件名带 pid，
// 避免并发首启的两个进程写同一个临时路径互相截断。已知边界：rename 会
// 覆盖，并发首启各持不同身份、以两台设备并存（duplicate_login 只拦同一
// 身份的双开）；接受，重启后收敛到文件里最终那份。
func LoadIdentity(path string) (common.DeviceID, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("read identity file: %w", err)
		}
		id := common.GenerateDeviceID()
		temporary := fmt.Sprintf("%s.tmp%d", path, os.Getpid())
		if err := os.WriteFile(temporary, []byte(id+"\n"), 0o600); err != nil {
			return "", fmt.Errorf("persist new identity: %w", err)
		}
		if err := os.Rename(temporary, path); err != nil {
			_ = os.Remove(temporary)
			return "", fmt.Errorf("install new identity: %w", err)
		}
		return id, nil
	}
	id, err := common.ParseDeviceID(string(trimSpaceNewline(raw)))
	if err != nil {
		return "", fmt.Errorf("identity file %s: %w", path, err)
	}
	return id, nil
}

// trimSpaceNewline 去掉身份文件末尾的换行。
func trimSpaceNewline(raw []byte) string {
	end := len(raw)
	for end > 0 && (raw[end-1] == '\n' || raw[end-1] == '\r' || raw[end-1] == ' ' || raw[end-1] == '\t') {
		end--
	}
	return string(raw[:end])
}
