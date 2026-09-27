package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------- 数据目录 ----------

func dataDir() string {
	if d := os.Getenv("SSH_MCP_HOME"); d != "" {
		_ = os.MkdirAll(d, 0o700)
		return d
	}
	home := homeDir()
	d := filepath.Join(home, ".ssh-mcp")
	_ = os.MkdirAll(d, 0o700)
	return d
}

func knownHostsPath() string { return filepath.Join(dataDir(), "known_hosts") }

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir(), strings.TrimPrefix(p, "~"))
	}
	return p
}

// ---------- 预配置服务器（凭据留在本机，不进入对话） ----------

type ServerPreset struct {
	ID             string `json:"id"`
	Host           string `json:"host"`
	Port           int    `json:"port,omitempty"`
	Username       string `json:"username"`
	Password       string `json:"password,omitempty"`
	PrivateKeyPath string `json:"private_key_path,omitempty"`
	PrivateKey     string `json:"private_key,omitempty"`
	Passphrase     string `json:"passphrase,omitempty"`
	HostKeyPolicy  string `json:"host_key_policy,omitempty"`
	SudoPassword   string `json:"sudo_password,omitempty"` // 供 ssh_sudo_exec 免对话使用
}

func loadPresets() []ServerPreset {
	if raw := strings.TrimSpace(os.Getenv("SSH_MCP_SERVERS")); raw != "" {
		ps, err := parsePresets([]byte(raw))
		if err != nil {
			log.Printf("SSH_MCP_SERVERS 解析失败: %v", err)
			return nil
		}
		return ps
	}
	path := filepath.Join(dataDir(), "servers.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	ps, err := parsePresets(b)
	if err != nil {
		log.Printf("解析 %s 失败: %v", path, err)
		return nil
	}
	return ps
}

func parsePresets(b []byte) ([]ServerPreset, error) {
	var arr []ServerPreset
	if err := json.Unmarshal(b, &arr); err == nil {
		return arr, nil
	}
	var obj struct {
		Servers []ServerPreset `json:"servers"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("既不是数组也不是 {\"servers\":[...]} 格式")
	}
	return obj.Servers, nil
}

func findPreset(id string) (*ServerPreset, error) {
	for _, p := range loadPresets() {
		if p.ID == id {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("找不到 id 为 %q 的预配置服务器（配置来源：~/.ssh-mcp/servers.json 或环境变量 SSH_MCP_SERVERS）", id)
}

// ---------- 命令黑名单 ----------

var (
	cmdBlacklist      []*regexp.Regexp
	defaultBlacklist  = []string{
		// rm -rf /  之类的根目录灭世操作
		`rm\s+(-[a-zA-Z]*[rf][a-zA-Z]*\s+)+/(\s|$)`,
		// 格式化文件系统
		`\bmkfs(\.\w+)?\b`,
		// dd 直接写整块磁盘
		`\bdd\b[^|;]*\bof=/dev/(?:[sh]d[a-z]|nvme|mmcblk|vd[a-z]|disk|mapper/)`,
		// fork 炸弹
		`:\s*\(\s*\)\s*\{[^}]*\}\s*;\s*:`,
		// 全盘去权限
		`\bchmod\s+-R\s+0*0\s+/\s*$`,
	}
)

func initBlacklist() {
	raw := strings.TrimSpace(os.Getenv("SSH_MCP_CMD_BLACKLIST"))
	var pats []string
	switch {
	case strings.EqualFold(raw, "off"):
		return
	case raw != "":
		pats = strings.Split(raw, ",")
	default:
		pats = defaultBlacklist
	}
	for _, p := range pats {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			log.Printf("黑名单正则无效，已忽略 %q: %v", p, err)
			continue
		}
		cmdBlacklist = append(cmdBlacklist, re)
	}
}

// ---------- 通用小工具 ----------

const maxOutBytes = 128 * 1024

// truncateOut 截断超长输出，保留头尾，避免撑爆模型上下文。
func truncateOut(s string) string {
	if len(s) <= maxOutBytes {
		return s
	}
	head := strings.ToValidUTF8(s[:96*1024], "�")
	tail := strings.ToValidUTF8(s[len(s)-32*1024:], "�")
	return head + fmt.Sprintf("\n…[输出过长已截断，原始共 %d 字节]…\n", len(s)) + tail
}

func toUTF8(b []byte) string { return strings.ToValidUTF8(string(b), "�") }

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func defaultHostKeyPolicy() string {
	p := strings.TrimSpace(os.Getenv("SSH_MCP_HOST_KEY_POLICY"))
	if p == "strict" {
		return "strict"
	}
	return "auto_accept"
}
