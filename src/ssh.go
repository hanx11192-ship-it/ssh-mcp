package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// ---------- 会话存储 ----------

type sshSession struct {
	ID           string
	Client       *gossh.Client
	Host         string
	Port         int
	Username     string
	Policy       string
	SudoPassword string // 可选：建连时保存，供 ssh_sudo_exec 免对话使用
	Created      time.Time

	mu       sync.Mutex
	lastUsed time.Time
}

func (s *sshSession) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *sshSession) idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastUsed)
}

var (
	sessMu   sync.Mutex
	sessions = map[string]*sshSession{}
)

const maxSessions = 32

func addSession(s *sshSession) error {
	sessMu.Lock()
	defer sessMu.Unlock()
	if len(sessions) >= maxSessions {
		return fmt.Errorf("已达最大会话数 %d，请先用 ssh_disconnect 释放空闲会话", maxSessions)
	}
	sessions[s.ID] = s
	return nil
}

func getSession(id string) (*sshSession, error) {
	sessMu.Lock()
	defer sessMu.Unlock()
	s, ok := sessions[id]
	if !ok {
		return nil, fmt.Errorf("会话 %q 不存在（可能已断开），可用 ssh_list_sessions 查看当前会话", id)
	}
	return s, nil
}

func removeSession(id string) (*sshSession, bool) {
	sessMu.Lock()
	defer sessMu.Unlock()
	s, ok := sessions[id]
	if ok {
		delete(sessions, id)
	}
	return s, ok
}

// startJanitor 空闲资源回收：默认 60 分钟（SSH_MCP_SESSION_TTL 分钟可调，设为 -1 关闭）。
func startJanitor() {
	ttlMin := 60
	if v := strings.TrimSpace(os.Getenv("SSH_MCP_SESSION_TTL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			ttlMin = n
		}
	}
	var ttl time.Duration
	if ttlMin >= 0 {
		ttl = time.Duration(ttlMin) * time.Minute
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for now := range t.C {
			if ttl > 0 {
				sessMu.Lock()
				for id, s := range sessions {
					if s.idle() > ttl {
						_ = s.Client.Close()
						delete(sessions, id)
						log.Printf("会话 %s 空闲超过 %d 分钟，已自动关闭", id, ttlMin)
					}
				}
				sessMu.Unlock()
			}
			cleanupShells(ttl, now)
			cleanupForwarders(ttl, now)
		}
	}()
}

// ---------- 主机密钥校验（TOFU：首次连接自动记录，之后校验一致性） ----------

func hostPattern(host string, port int) string {
	if port == 22 {
		return host
	}
	return fmt.Sprintf("[%s]:%d", host, port)
}

func hostKeyCallback(host string, port int, policy string, captured *gossh.PublicKey) gossh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		pattern := hostPattern(hostname, port)
		b64 := base64.StdEncoding.EncodeToString(key.Marshal())
		line := pattern + " " + key.Type() + " " + b64
		path := knownHostsPath()
		data, _ := os.ReadFile(path)
		for _, l := range strings.Split(string(data), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			fields := strings.Fields(l)
			if len(fields) < 3 || fields[0] != pattern {
				continue
			}
			if fields[1] == key.Type() && fields[2] == b64 {
				*captured = key // 与已记录密钥一致
				return nil
			}
			return fmt.Errorf(
				"主机 %s 的密钥与已记录的不一致，疑似中间人攻击，已拒绝连接。"+
					"如确认对端确实更换了密钥，请删除 %s 中该主机的记录后重试", pattern, path)
		}
		if policy == "strict" {
			return fmt.Errorf(
				"主机 %s 的密钥不在已知列表（host_key_policy=strict）。"+
					"可先用终端手动连接一次以记录密钥，或将 host_key_policy 改为 auto_accept", pattern)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.WriteString(line + "\n"); err != nil {
			return err
		}
		*captured = key
		log.Printf("已记录主机 %s 的密钥指纹 %s", pattern, gossh.FingerprintSHA256(key))
		return nil
	}
}

// ---------- 认证 ----------

func buildAuth(password, privateKeyPath, privateKey, passphrase string) ([]gossh.AuthMethod, error) {
	var methods []gossh.AuthMethod

	// 1) 显式私钥（内容优先，其次路径）
	var keyData []byte
	if privateKey != "" {
		keyData = []byte(privateKey)
	} else if privateKeyPath != "" {
		b, err := os.ReadFile(expandHome(privateKeyPath))
		if err != nil {
			return nil, fmt.Errorf("读取私钥 %s 失败: %w", privateKeyPath, err)
		}
		keyData = b
	}
	if keyData != nil {
		signer, err := parseKey(keyData, passphrase)
		if err != nil {
			return nil, err
		}
		methods = append(methods, gossh.PublicKeys(signer))
	}

	// 2) 密码 + keyboard-interactive（网络设备常用后者）
	if password != "" {
		methods = append(methods,
			gossh.Password(password),
			gossh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				ans := make([]string, len(questions))
				for i := range questions {
					ans[i] = password
				}
				return ans, nil
			}),
		)
	}

	// 3) 都没给则尝试 ~/.ssh 默认密钥
	if len(methods) == 0 {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			p := filepath.Join(homeDir(), ".ssh", name)
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			signer, err := gossh.ParsePrivateKey(b)
			if err != nil {
				continue
			}
			methods = append(methods, gossh.PublicKeys(signer))
		}
	}

	if len(methods) == 0 {
		return nil, errors.New("未提供任何认证方式：请传 password，或 private_key/private_key_path，或在 ~/.ssh 放置默认密钥")
	}
	return methods, nil
}

func parseKey(data []byte, passphrase string) (gossh.Signer, error) {
	if passphrase != "" {
		return gossh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
	}
	signer, err := gossh.ParsePrivateKey(data)
	if err != nil {
		var ppErr *gossh.PassphraseMissingError
		if errors.As(err, &ppErr) {
			return nil, errors.New("私钥已加密，请通过 passphrase 参数提供口令")
		}
		return nil, fmt.Errorf("解析私钥失败: %w", err)
	}
	return signer, nil
}

// ---------- 连接 ----------

func uniqueSessionID(base string) string {
	sessMu.Lock()
	defer sessMu.Unlock()
	if _, exists := sessions[base]; !exists {
		return base
	}
	for n := 2; ; n++ {
		id := fmt.Sprintf("%s-%d", base, n)
		if _, exists := sessions[id]; !exists {
			return id
		}
	}
}

func doConnect(host string, port int, username, password, privateKeyPath, privateKey,
	passphrase, policy, name, sudoPassword string) (map[string]any, error) {

	if host == "" || username == "" {
		return nil, errors.New("host 与 username 必填")
	}
	if port <= 0 {
		port = 22
	}
	if policy == "" {
		policy = defaultHostKeyPolicy()
	}
	if policy != "auto_accept" && policy != "strict" {
		return nil, fmt.Errorf("host_key_policy 只支持 auto_accept / strict，收到 %q", policy)
	}

	auths, err := buildAuth(password, privateKeyPath, privateKey, passphrase)
	if err != nil {
		return nil, err
	}

	var gotKey gossh.PublicKey
	cfg := &gossh.ClientConfig{
		User:            username,
		Auth:            auths,
		HostKeyCallback: hostKeyCallback(host, port, policy, &gotKey),
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := gossh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", addr, err)
	}

	id := strings.TrimSpace(name)
	if id == "" {
		id = fmt.Sprintf("%s:%d", host, port)
	}
	id = uniqueSessionID(id)

	s := &sshSession{
		ID: id, Client: client, Host: host, Port: port,
		Username: username, Policy: policy, SudoPassword: sudoPassword,
		Created: time.Now(), lastUsed: time.Now(),
	}
	if err := addSession(s); err != nil {
		_ = client.Close()
		return nil, err
	}

	res := map[string]any{
		"ok": true, "session_id": id,
		"host": host, "port": port, "username": username,
		"host_key_policy": policy,
	}
	if gotKey != nil {
		res["host_key_fingerprint"] = gossh.FingerprintSHA256(gotKey)
	}
	return res, nil
}

// ---------- 命令执行 ----------

func checkBlacklist(command string) error {
	for _, re := range cmdBlacklist {
		if re.MatchString(command) {
			return fmt.Errorf("命令被本地安全策略拦截（命中规则 %q）。"+
				"如确属误拦，可在服务端用 SSH_MCP_CMD_BLACKLIST=off 关闭或自定义规则", re.String())
		}
	}
	return nil
}

// execViaSession 公共执行通道：在指定 SSH 连接上运行命令，支持向 stdin 写入数据（如 sudo 密码）。
func execViaSession(s *sshSession, command string, stdinData string, timeout time.Duration) (stdout, stderr string, exit int, timedOut bool, runErr error) {
	sess, err := s.Client.NewSession()
	if err != nil {
		return "", "", -1, false, fmt.Errorf("创建执行通道失败（连接可能已断开，请重新 ssh_connect）: %w", err)
	}
	defer sess.Close()

	var outBuf, errBuf bytes.Buffer
	sess.Stdout = &outBuf
	sess.Stderr = &errBuf
	if stdinData != "" {
		sess.Stdin = strings.NewReader(stdinData)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()

	select {
	case runErr = <-done:
	case <-time.After(timeout):
		timedOut = true
		_ = sess.Signal(gossh.SIGKILL)
		_ = sess.Close()
	}

	s.touch()
	return truncateOut(toUTF8(outBuf.Bytes())), truncateOut(toUTF8(errBuf.Bytes())),
		exitCode(runErr), timedOut, runErr
}

func doExec(_ context.Context, s *sshSession, command, cwd string, timeout time.Duration) (map[string]any, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("command 不能为空")
	}
	if err := checkBlacklist(command); err != nil {
		return nil, err
	}
	if cwd != "" {
		command = "cd " + shellQuote(cwd) + " && (" + command + ")"
	}

	start := time.Now()
	stdout, stderr, code, timedOut, runErr := execViaSession(s, command, "", timeout)

	res := map[string]any{
		"session_id":  s.ID,
		"command":     command,
		"exit_code":   code,
		"stdout":      stdout,
		"stderr":      stderr,
		"duration_ms": time.Since(start).Milliseconds(),
		"timed_out":   timedOut,
	}
	if timedOut {
		res["note"] = fmt.Sprintf("命令超过 %.0fs 超时限制，已发送 SIGKILL", timeout.Seconds())
	} else if runErr != nil {
		var ee *gossh.ExitError
		if !errors.As(runErr, &ee) {
			res["note"] = "命令运行异常: " + runErr.Error()
		}
	}
	return res, nil
}

// doSudoExec 以 sudo 执行命令。密码优先级：本次传入 > 会话保存 > 依赖 NOPASSWD（sudo -n）。
func doSudoExec(_ context.Context, s *sshSession, command, cwd string, sudoPassword string, timeout time.Duration) (map[string]any, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("command 不能为空")
	}
	// root 权限更危险，黑名单同样生效
	if err := checkBlacklist(command); err != nil {
		return nil, err
	}
	if cwd != "" {
		command = "cd " + shellQuote(cwd) + " && (" + command + ")"
	}
	if sudoPassword == "" {
		sudoPassword = s.SudoPassword
	}

	var cmd, stdinData string
	if sudoPassword != "" {
		// -S 从 stdin 读密码；-p '' 抑制回显提示；密码不进入远端命令行/进程列表
		cmd = "sudo -S -p '' -- bash -c " + shellQuote(command)
		stdinData = sudoPassword + "\n"
	} else {
		// -n 非交互：要求远端已配置 NOPASSWD，或当前即为 root
		cmd = "sudo -n -- bash -c " + shellQuote(command)
	}

	start := time.Now()
	stdout, stderr, code, timedOut, _ := execViaSession(s, cmd, stdinData, timeout)

	res := map[string]any{
		"session_id":  s.ID,
		"command":     command,
		"exit_code":   code,
		"stdout":      stdout,
		"stderr":      stderr,
		"duration_ms": time.Since(start).Milliseconds(),
		"timed_out":   timedOut,
		"as":          "sudo",
	}
	if timedOut {
		res["note"] = fmt.Sprintf("命令超过 %.0fs 超时限制，已发送 SIGKILL", timeout.Seconds())
	}
	// sudo 常见失败诊断
	lower := strings.ToLower(stderr)
	switch {
	case code == 1 && strings.Contains(lower, "a password is required"):
		res["note"] = "sudo 需要密码但未提供：可在调用时传 sudo_password，或建连时保存，或给该用户配置 NOPASSWD"
	case code != 0 && strings.Contains(lower, "sorry, try again"):
		res["note"] = "sudo 密码不正确"
	case code != 0 && strings.Contains(lower, "not in the sudoers file"):
		res["note"] = "该用户不在 sudoers 中，需在远端以管理员执行：usermod -aG sudo " + s.Username
	}
	return res, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *gossh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus()
	}
	return -1
}
