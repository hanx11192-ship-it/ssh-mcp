package main

import (
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// ---------- 交互式 PTY 会话 ----------

type shellSession struct {
	ID       string
	SSHID    string // 所属 ssh 连接会话 id
	sess     *gossh.Session
	stdin    io.WriteCloser
	term     string
	cols     int
	rows     int

	mu       sync.Mutex
	raw      []byte // 原始输出缓冲
	readPos  int    // 已读取偏移
	overflow bool
	exited   bool
	lastUsed time.Time

	once sync.Once
	done chan struct{}
}

var (
	shellMu       sync.Mutex
	shellSessions = map[string]*shellSession{}
	shellSeq      int
)

const maxShellBuffer = 512 * 1024

// ansiRe 剥离 ANSI 转义序列（CSI / OSC / 单字符），对 agent 更友好。
var ansiRe = regexp.MustCompile(
	"\x1b\\[[0-9;?]*[ -/]*[@-~]" + // CSI
		"|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)?" + // OSC（标题等）
		"|\x1b[@-Z\\-_]") // 其余单字符转义

func normalizeTTY(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ToValidUTF8(s, "�")
	return s
}

func (sh *shellSession) touch() {
	sh.mu.Lock()
	sh.lastUsed = time.Now()
	sh.mu.Unlock()
}

func (sh *shellSession) idle() time.Duration {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return time.Since(sh.lastUsed)
}

// drain 取出上次读取之后的新输出（已剥离 ANSI、规范换行）。
func (sh *shellSession) drain() (string, bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fresh := string(sh.raw[sh.readPos:])
	sh.readPos = len(sh.raw)
	ov := sh.overflow
	sh.overflow = false
	out := normalizeTTY(fresh)
	if ov {
		out = "[缓冲溢出，较早的中间输出已被丢弃]\n" + out
	}
	return out, len(out) > 0
}

func shellUniqueID() string {
	shellMu.Lock()
	defer shellMu.Unlock()
	shellSeq++
	return fmt.Sprintf("sh-%d", shellSeq)
}

// doShellStart 启动 PTY 交互会话；command 为空时打开登录 shell。
func doShellStart(s *sshSession, command, termType string, cols, rows int) (map[string]any, error) {
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 32
	}
	if termType == "" {
		termType = "xterm-256color"
	}

	sess, err := s.Client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("创建 PTY 会话失败（连接可能已断开，请重新 ssh_connect）: %w", err)
	}
	modes := gossh.TerminalModes{
		gossh.ECHO:          1,
		gossh.TTY_OP_ISPEED: 115200,
		gossh.TTY_OP_OSPEED: 115200,
	}
	if err := sess.RequestPty(termType, rows, cols, modes); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("请求 PTY 失败（对端可能不支持交互终端）: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("获取标准输入失败: %w", err)
	}
	stdout, err := sess.StdoutPipe() // PTY 模式下 stderr 已并入 stdout
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("获取标准输出失败: %w", err)
	}

	sh := &shellSession{
		ID: shellUniqueID(), SSHID: s.ID, sess: sess, stdin: stdin,
		term: termType, cols: cols, rows: rows,
		lastUsed: time.Now(), done: make(chan struct{}),
	}

	// 收集输出
	go func() {
		buf := make([]byte, 8192)
		for {
			n, rerr := stdout.Read(buf)
			if n > 0 {
				sh.mu.Lock()
				sh.raw = append(sh.raw, buf[:n]...)
				if len(sh.raw) > maxShellBuffer {
					cut := len(sh.raw) - maxShellBuffer/2
					sh.raw = append([]byte(nil), sh.raw[cut:]...)
					if sh.readPos > cut {
						sh.readPos -= cut
					} else {
						sh.readPos = 0
					}
					sh.overflow = true
				}
				sh.mu.Unlock()
			}
			if rerr != nil {
				sh.once.Do(func() { close(sh.done) })
				return
			}
		}
	}()

	var serr error
	if command == "" {
		serr = sess.Shell()
	} else {
		serr = sess.Start(command)
	}
	if serr != nil {
		_ = sess.Close()
		if command == "" {
			return nil, fmt.Errorf("启动登录 shell 失败: %w", serr)
		}
		return nil, fmt.Errorf("启动命令失败: %w", serr)
	}

	// 必须在 Shell()/Start() 之后才能 Wait()：进程未启动时 Wait 会立即返回
	go func() {
		_ = sess.Wait()
		sh.mu.Lock()
		sh.exited = true
		sh.mu.Unlock()
		sh.once.Do(func() { close(sh.done) })
	}()

	shellMu.Lock()
	if len(shellSessions) >= maxSessions {
		shellMu.Unlock()
		_ = sess.Close()
		return nil, fmt.Errorf("已达最大会话数 %d，请先 ssh_shell_close 释放", maxSessions)
	}
	shellSessions[sh.ID] = sh
	shellMu.Unlock()

	return map[string]any{
		"ok": true, "shell_id": sh.ID, "session_id": s.ID,
		"command": command, "term": termType, "size": fmt.Sprintf("%dx%d", cols, rows),
		"note": "用 ssh_shell_send 发送输入并收取输出；ssh_shell_read 单独轮询输出；ssh_shell_close 关闭",
	}, nil
}

func getShell(id string) (*shellSession, error) {
	shellMu.Lock()
	defer shellMu.Unlock()
	sh, ok := shellSessions[id]
	if !ok {
		return nil, fmt.Errorf("交互会话 %q 不存在（可能已退出或关闭），可用 ssh_shell_close 清理后重新 ssh_shell_start", id)
	}
	return sh, nil
}

func removeShell(id string) (*shellSession, bool) {
	shellMu.Lock()
	defer shellMu.Unlock()
	sh, ok := shellSessions[id]
	if ok {
		delete(shellSessions, id)
	}
	return sh, ok
}

func doShellSend(sh *shellSession, input string, waitMS int) (map[string]any, error) {
	sh.touch()
	if _, err := sh.stdin.Write([]byte(input)); err != nil {
		return nil, fmt.Errorf("写入失败（会话可能已退出）: %w", err)
	}
	if waitMS <= 0 {
		waitMS = 800
	}
	select {
	case <-sh.done:
	case <-time.After(time.Duration(waitMS) * time.Millisecond):
	}
	out, _ := sh.drain()
	sh.mu.Lock()
	exited := sh.exited
	sh.mu.Unlock()
	res := map[string]any{"shell_id": sh.ID, "output": out, "exited": exited}
	if exited {
		res["note"] = "进程已退出，可用 ssh_shell_close 释放"
	}
	return res, nil
}

func doShellRead(sh *shellSession, waitMS int) (map[string]any, error) {
	sh.touch()
	out, has := sh.drain()
	if !has && waitMS > 0 {
		select {
		case <-sh.done:
		case <-time.After(time.Duration(waitMS) * time.Millisecond):
		}
		out, _ = sh.drain()
	}
	sh.mu.Lock()
	exited := sh.exited
	sh.mu.Unlock()
	return map[string]any{"shell_id": sh.ID, "output": out, "exited": exited}, nil
}

func doShellClose(id string) (map[string]any, error) {
	sh, ok := removeShell(id)
	if !ok {
		return nil, fmt.Errorf("交互会话 %q 不存在", id)
	}
	_ = sh.stdin.Close()
	_ = sh.sess.Close()
	select {
	case <-sh.done:
	default:
	}
	return map[string]any{"ok": true, "closed": id}, nil
}

// cleanupShells 关闭已失效/超时的交互会话（由 janitor 调用）。
func cleanupShells(maxIdle time.Duration, now time.Time) {
	shellMu.Lock()
	defer shellMu.Unlock()
	for id, sh := range shellSessions {
		sessMu.Lock()
		_, alive := sessions[sh.SSHID]
		sessMu.Unlock()
		expired := maxIdle > 0 && sh.idle() > maxIdle
		if !alive || expired {
			_ = sh.stdin.Close()
			_ = sh.sess.Close()
			sh.once.Do(func() { close(sh.done) })
			delete(shellSessions, id)
			log.Printf("交互会话 %s 已回收（连接存活=%v，空闲超时=%v）", id, alive, expired)
		}
	}
}
