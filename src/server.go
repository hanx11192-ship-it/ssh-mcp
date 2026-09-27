package main

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newMCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "ssh-mcp", Version: version}, &mcp.ServerOptions{
		Instructions: "本服务把 SSH 封装为 MCP 工具，用于让 agent 管理局域网设备。" +
			"典型流程：先用 ssh_connect 或 ssh_connect_by_id 建立连接并拿到 session_id，" +
			"之后用 ssh_exec 执行命令、sftp_* 系列传输/读写文件，最后 ssh_disconnect 释放。" +
			"凭据只保存在本机（调用参数或 ~/.ssh-mcp/servers.json），不会发送给模型以外的任何一方。",
	})

	// ---- 连接管理 ----
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ssh_connect",
		Description: "建立到局域网/远程设备的 SSH 连接，返回 session_id 供后续工具使用。认证三选一：" +
			"password、private_key（PEM 内容）、private_key_path；都不传时回退到 ~/.ssh 默认密钥。" +
			"host_key_policy=auto_accept（默认，首次连接自动记录主机密钥，之后校验一致性）/strict（拒绝陌生主机）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		Host          string `json:"host" jsonschema:"目标主机 IP 或域名（必填）"`
		Username      string `json:"username" jsonschema:"SSH 用户名（必填）"`
		Port          int    `json:"port,omitempty" jsonschema:"SSH 端口，默认 22"`
		Password      string `json:"password,omitempty" jsonschema:"密码认证（可选）"`
		PrivateKeyPath string `json:"private_key_path,omitempty" jsonschema:"私钥文件路径，支持 ~（可选）"`
		PrivateKey    string `json:"private_key,omitempty" jsonschema:"私钥 PEM 内容（可选）"`
		Passphrase    string `json:"passphrase,omitempty" jsonschema:"私钥口令（可选）"`
		HostKeyPolicy string `json:"host_key_policy,omitempty" jsonschema:"auto_accept 或 strict（可选）"`
		Name          string `json:"name,omitempty" jsonschema:"自定义 session_id（可选，缺省为 host:port）"`
		SudoPassword  string `json:"sudo_password,omitempty" jsonschema:"保存到会话的 sudo 密码，供 ssh_sudo_exec 免对话使用（可选）"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		res, err := doConnect(a.Host, a.Port, a.Username, a.Password,
			a.PrivateKeyPath, a.PrivateKey, a.Passphrase, a.HostKeyPolicy, a.Name, a.SudoPassword)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "ssh_connect_by_id",
		Description: "按 id 连接 ~/.ssh-mcp/servers.json（或环境变量 SSH_MCP_SERVERS）中预配置的服务器。" +
			"适合把凭据留在本机、避免密码出现在对话中的场景。先调 ssh_list_servers 查看可用 id。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		ServerID string `json:"server_id" jsonschema:"预配置服务器的 id（必填）"`
		Name     string `json:"name,omitempty" jsonschema:"自定义 session_id（可选）"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		p, err := findPreset(a.ServerID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doConnect(p.Host, p.Port, p.Username, p.Password,
			p.PrivateKeyPath, p.PrivateKey, p.Passphrase, p.HostKeyPolicy, a.Name, p.SudoPassword)
		if err != nil {
			return nil, nil, err
		}
		res["preset_id"] = p.ID
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ssh_list_servers",
		Description: "列出 ~/.ssh-mcp/servers.json 或环境变量 SSH_MCP_SERVERS 中预配置的服务器（不含明文密码）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		ps := loadPresets()
		out := make([]map[string]any, 0, len(ps))
		for _, p := range ps {
			m := map[string]any{"id": p.ID, "host": p.Host, "username": p.Username}
			if p.Port > 0 {
				m["port"] = p.Port
			}
			if p.Password != "" || p.PrivateKey != "" || p.PrivateKeyPath != "" {
				m["auth"] = "已配置"
			}
			out = append(out, m)
		}
		return nil, map[string]any{"count": len(out), "servers": out,
			"hint": "用 ssh_connect_by_id 连接；配置文件: ~/.ssh-mcp/servers.json"}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ssh_list_sessions",
		Description: "列出当前所有活跃 SSH 会话及其空闲时长。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		sessMu.Lock()
		defer sessMu.Unlock()
		out := make([]map[string]any, 0, len(sessions))
		for _, s := range sessions {
			out = append(out, map[string]any{
				"session_id": s.ID, "host": s.Host, "port": s.Port,
				"username": s.Username, "idle_seconds": int(s.idle().Seconds()),
			})
		}
		return nil, map[string]any{"count": len(out), "sessions": out}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ssh_disconnect",
		Description: "断开并释放指定的 SSH 会话。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID string `json:"session_id" jsonschema:"要断开的会话 id（必填）"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, ok := removeSession(a.SessionID)
		if !ok {
			return nil, nil, fmt.Errorf("会话 %q 不存在", a.SessionID)
		}
		_ = s.Client.Close()
		return nil, map[string]any{"ok": true, "closed": a.SessionID}, nil
	})

	// ---- 命令执行 ----
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ssh_exec",
		Description: "在远程设备上执行 shell 命令（非交互式），返回 stdout/stderr/exit_code。" +
			"支持管道、重定向等常规 shell 语法；不要执行需要交互确认的命令（用 -y / DEBIAN_FRONTEND=noninteractive 等）。" +
			"cwd 参数可指定工作目录。默认 30 秒超时，超时会向进程发送 SIGKILL。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID  string  `json:"session_id" jsonschema:"目标会话 id（必填）"`
		Command    string  `json:"command" jsonschema:"要执行的命令（必填）"`
		Cwd        string  `json:"cwd,omitempty" jsonschema:"工作目录（可选）"`
		TimeoutSec float64 `json:"timeout_sec,omitempty" jsonschema:"超时秒数，默认 30，上限 3600"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		timeout := 30 * time.Second
		if a.TimeoutSec > 0 {
			timeout = time.Duration(a.TimeoutSec * float64(time.Second))
			if timeout > time.Hour {
				timeout = time.Hour
			}
		}
		res, err := doExec(ctx, s, a.Command, a.Cwd, timeout)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	// ---- SFTP 文件操作 ----
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sftp_upload",
		Description: "上传本地文件到远程设备（SFTP）。remote_path 为完整目标文件路径，父目录不存在会自动创建。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID  string `json:"session_id" jsonschema:"目标会话 id（必填）"`
		LocalPath  string `json:"local_path" jsonschema:"本地文件路径（必填）"`
		RemotePath string `json:"remote_path" jsonschema:"远程目标路径（必填）"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doUpload(s, a.LocalPath, a.RemotePath)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sftp_download",
		Description: "从远程设备下载文件到本地（SFTP）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID  string `json:"session_id" jsonschema:"目标会话 id（必填）"`
		RemotePath string `json:"remote_path" jsonschema:"远程文件路径（必填）"`
		LocalPath  string `json:"local_path" jsonschema:"本地目标路径（必填）"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doDownload(s, a.RemotePath, a.LocalPath)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sftp_list",
		Description: "列出远程目录内容（名称/大小/权限/修改时间）。默认不显示隐藏文件。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID  string `json:"session_id" jsonschema:"目标会话 id（必填）"`
		RemotePath string `json:"remote_path,omitempty" jsonschema:"远程目录，默认当前目录"`
		ShowHidden bool   `json:"show_hidden,omitempty" jsonschema:"是否显示隐藏文件"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doList(s, a.RemotePath, a.ShowHidden)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sftp_mkdir",
		Description: "在远程设备上创建目录（自动递归创建父目录）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID  string `json:"session_id" jsonschema:"目标会话 id（必填）"`
		RemotePath string `json:"remote_path" jsonschema:"要创建的目录路径（必填）"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doMkdir(s, a.RemotePath)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sftp_remove",
		Description: "删除远程文件或目录。目录需 recursive=true 递归删除。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID  string `json:"session_id" jsonschema:"目标会话 id（必填）"`
		RemotePath string `json:"remote_path" jsonschema:"要删除的路径（必填）"`
		Recursive  bool   `json:"recursive,omitempty" jsonschema:"目录删除时设为 true"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doRemove(s, a.RemotePath, a.Recursive)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sftp_read",
		Description: "读取远程文本文件内容（UTF-8），默认最多 512KB，适合读配置/日志片段。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID  string `json:"session_id" jsonschema:"目标会话 id（必填）"`
		RemotePath string `json:"remote_path" jsonschema:"远程文件路径（必填）"`
		MaxBytes   int64  `json:"max_bytes,omitempty" jsonschema:"最多读取字节数，默认 524288，上限 2097152"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doRead(s, a.RemotePath, a.MaxBytes)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sftp_write",
		Description: "把文本内容写入远程文件（UTF-8），默认覆盖，append=true 追加。父目录自动创建。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID  string `json:"session_id" jsonschema:"目标会话 id（必填）"`
		RemotePath string `json:"remote_path" jsonschema:"远程文件路径（必填）"`
		Content    string `json:"content" jsonschema:"要写入的文本内容（必填）"`
		Append     bool   `json:"append,omitempty" jsonschema:"true 为追加，默认覆盖"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doWrite(s, a.RemotePath, a.Content, a.Append)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	// ---- sudo 命令执行 ----
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ssh_sudo_exec",
		Description: "以 sudo 执行命令（解决非交互场景 sudo 卡在密码提示的问题）。" +
			"密码来源优先级：本次传入 sudo_password > 建连时保存的 sudo_password > 依赖远端 NOPASSWD 配置（sudo -n）。" +
			"root 登录时无需密码。密码通过 stdin 传给 sudo，不会出现在远端进程列表。命令黑名单同样生效。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID    string  `json:"session_id" jsonschema:"目标会话 id（必填）"`
		Command      string  `json:"command" jsonschema:"要执行的命令（必填，不需要写 sudo 前缀）"`
		SudoPassword string  `json:"sudo_password,omitempty" jsonschema:"sudo 密码（可选，未提供则用建连时保存的或 NOPASSWD）"`
		Cwd          string  `json:"cwd,omitempty" jsonschema:"工作目录（可选）"`
		TimeoutSec   float64 `json:"timeout_sec,omitempty" jsonschema:"超时秒数，默认 30，上限 3600"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		timeout := 30 * time.Second
		if a.TimeoutSec > 0 {
			timeout = time.Duration(a.TimeoutSec * float64(time.Second))
			if timeout > time.Hour {
				timeout = time.Hour
			}
		}
		res, err := doSudoExec(ctx, s, a.Command, a.Cwd, a.SudoPassword, timeout)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	// ---- 交互式 PTY 会话 ----
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ssh_shell_start",
		Description: "在远程设备上启动交互式 PTY 终端会话（支持 vim/top 等全屏程序与需要确认输入的命令）。" +
			"command 为空时打开登录 shell，否则以 PTY 执行该命令。返回 shell_id；" +
			"之后用 ssh_shell_send 发输入并收输出、ssh_shell_read 轮询输出、ssh_shell_close 关闭。" +
			"注意：交互会话中的输入不受命令黑名单限制。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID string `json:"session_id" jsonschema:"目标会话 id（必填）"`
		Command   string `json:"command,omitempty" jsonschema:"要执行的命令；留空打开登录 shell"`
		Term      string `json:"term,omitempty" jsonschema:"终端类型，默认 xterm-256color"`
		Cols      int    `json:"cols,omitempty" jsonschema:"终端宽度，默认 120"`
		Rows      int    `json:"rows,omitempty" jsonschema:"终端高度，默认 32"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doShellStart(s, a.Command, a.Term, a.Cols, a.Rows)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "ssh_shell_send",
		Description: "向交互式 PTY 会话发送输入（命令/按键/确认，换行用 \\n），等待 wait_ms 毫秒后返回新增输出。" +
			"输出已剥离 ANSI 转义序列并规范换行。若进程已退出，exited=true。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		ShellID string `json:"shell_id" jsonschema:"交互会话 id（必填）"`
		Input   string `json:"input" jsonschema:"要发送的内容（必填），如 ls -la\\n 或 y\\n 或 Ctrl+C 对应 \\u0003"`
		WaitMs  int    `json:"wait_ms,omitempty" jsonschema:"发送后等待毫秒数，默认 800"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		sh, err := getShell(a.ShellID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doShellSend(sh, a.Input, a.WaitMs)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ssh_shell_read",
		Description: "读取交互式 PTY 会话自上次读取以来的新输出；无新输出时可等待 wait_ms 毫秒（长刷新程序如 top 适合轮询此接口）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		ShellID string `json:"shell_id" jsonschema:"交互会话 id（必填）"`
		WaitMs  int    `json:"wait_ms,omitempty" jsonschema:"无新输出时最长等待毫秒数，默认 0（立即返回）"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		sh, err := getShell(a.ShellID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doShellRead(sh, a.WaitMs)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ssh_shell_close",
		Description: "关闭并释放交互式 PTY 会话。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		ShellID string `json:"shell_id" jsonschema:"交互会话 id（必填）"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		res, err := doShellClose(a.ShellID)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	// ---- 本地端口转发 ----
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ssh_forward_start",
		Description: "建立本地端口转发（等同 ssh -L）：把 local_host:local_port 的流量经 SSH 会话转发到 " +
			"远程视角的 target_host:target_port（可达设备内网里本机无法直连的服务）。" +
			"local_host 默认 127.0.0.1；local_port 传 0 自动分配。返回 forward_id 与实际监听地址。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		SessionID   string `json:"session_id" jsonschema:"目标会话 id（必填）"`
		TargetHost  string `json:"target_host" jsonschema:"从 SSH 服务器侧访问的目标主机（必填）"`
		TargetPort  int    `json:"target_port" jsonschema:"从 SSH 服务器侧访问的目标端口（必填）"`
		LocalPort   int    `json:"local_port,omitempty" jsonschema:"本地监听端口，0 为自动分配"`
		LocalHost   string `json:"local_host,omitempty" jsonschema:"本地监听地址，默认 127.0.0.1"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		s, err := getSession(a.SessionID)
		if err != nil {
			return nil, nil, err
		}
		res, err := doForwardStart(s, a.TargetHost, a.TargetPort, a.LocalPort, a.LocalHost)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ssh_forward_list",
		Description: "列出当前所有端口转发及其连接计数。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		res, err := doForwardList()
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ssh_forward_stop",
		Description: "停止端口转发：关闭本地监听并断开活动转发连接。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a struct {
		ForwardID string `json:"forward_id" jsonschema:"转发 id（必填）"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		res, err := doForwardStop(a.ForwardID)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})

	return srv
}

	// ---- SFTP 文件操作 ----