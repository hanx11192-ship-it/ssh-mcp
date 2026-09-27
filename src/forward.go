package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// ---------- 本地端口转发（ssh -L 语义） ----------

type forwarder struct {
	ID         string
	SSHID      string
	BindAddr   string
	TargetHost string
	TargetPort int

	ln         net.Listener
	mu         sync.Mutex
	channels   map[uint64]gossh.Channel
	nextChanID uint64
	active     int64
	total      int64
	stopped    bool
	lastUsed   time.Time
}

var (
	fwdMu      sync.Mutex
	forwarders = map[string]*forwarder{}
	fwdSeq     int
)

func (f *forwarder) touch() {
	f.mu.Lock()
	f.lastUsed = time.Now()
	f.mu.Unlock()
}

func (f *forwarder) snapshot() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{
		"forward_id": f.ID, "session_id": f.SSHID, "bind": f.BindAddr,
		"target": net.JoinHostPort(f.TargetHost, strconv.Itoa(f.TargetPort)),
		"active_conns": f.active, "total_conns": f.total, "stopped": f.stopped,
	}
}

// forwardPayload 对应 RFC 4254 §7.2 direct-tcpip 通道打开参数。
type forwardPayload struct {
	Host       string
	Port       uint32
	OriginIP   string
	OriginPort uint32
}

func doForwardStart(s *sshSession, targetHost string, targetPort, localPort int, localHost string) (map[string]any, error) {
	if targetHost == "" || targetPort <= 0 {
		return nil, fmt.Errorf("target_host 与 target_port 必填")
	}
	if localPort < 0 || localPort > 65535 {
		return nil, fmt.Errorf("local_port 取值 0-65535（0 表示自动分配）")
	}
	if localHost == "" {
		localHost = "127.0.0.1" // 默认仅监听本机，避免意外向局域网暴露
	}

	bind := net.JoinHostPort(localHost, strconv.Itoa(localPort))
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", bind, err)
	}

	fwdMu.Lock()
	if len(forwarders) >= maxSessions {
		fwdMu.Unlock()
		_ = ln.Close()
		return nil, fmt.Errorf("已达最大转发数 %d，请先 ssh_forward_stop 释放", maxSessions)
	}
	fwdSeq++
	fw := &forwarder{
		ID: fmt.Sprintf("fwd-%d", fwdSeq), SSHID: s.ID,
		TargetHost: targetHost, TargetPort: targetPort,
		ln: ln, channels: map[uint64]gossh.Channel{},
		lastUsed: time.Now(),
	}
	fw.BindAddr = ln.Addr().String()
	forwarders[fw.ID] = fw
	fwdMu.Unlock()

	go forwardAcceptLoop(fw, s)

	return map[string]any{
		"ok": true, "forward_id": fw.ID, "bind": fw.BindAddr,
		"target": net.JoinHostPort(targetHost, strconv.Itoa(targetPort)),
		"note":   "本地端口已开始转发；用 ssh_forward_list 查看，ssh_forward_stop 停止",
	}, nil
}

func forwardAcceptLoop(fw *forwarder, s *sshSession) {
	for {
		conn, err := fw.ln.Accept()
		if err != nil {
			fw.mu.Lock()
			stopped := fw.stopped
			fw.mu.Unlock()
			if !stopped {
				// listener 异常关闭（如所属会话断开被 janitor 清理）
				fw.stopAll()
			}
			return
		}
		fw.touch()
		go handleForwardConn(fw, s, conn)
	}
}

func handleForwardConn(fw *forwarder, s *sshSession, lc net.Conn) {
	atomic.AddInt64(&fw.active, 1)
	atomic.AddInt64(&fw.total, 1)
	defer func() {
		atomic.AddInt64(&fw.active, -1)
		_ = lc.Close()
	}()

	originIP, originPort := "127.0.0.1", uint32(0)
	if ta, ok := lc.RemoteAddr().(*net.TCPAddr); ok {
		originIP, originPort = ta.IP.String(), uint32(ta.Port)
	}
	payload := gossh.Marshal(forwardPayload{
		Host: fw.TargetHost, Port: uint32(fw.TargetPort),
		OriginIP: originIP, OriginPort: originPort,
	})
	ch, reqs, err := s.Client.OpenChannel("direct-tcpip", payload)
	if err != nil {
		return
	}
	go gossh.DiscardRequests(reqs)

	cid := atomic.AddUint64(&fw.nextChanID, 1)
	fw.mu.Lock()
	if fw.stopped {
		fw.mu.Unlock()
		_ = ch.Close()
		return
	}
	fw.channels[cid] = ch
	fw.mu.Unlock()
	defer func() {
		fw.mu.Lock()
		delete(fw.channels, cid)
		fw.mu.Unlock()
		_ = ch.Close()
	}()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(ch, lc); _ = ch.CloseWrite(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(lc, ch); done <- struct{}{} }()
	<-done
}

func (f *forwarder) stopAll() {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return
	}
	f.stopped = true
	_ = f.ln.Close()
	chs := make([]gossh.Channel, 0, len(f.channels))
	for _, ch := range f.channels {
		chs = append(chs, ch)
	}
	f.channels = map[uint64]gossh.Channel{}
	f.mu.Unlock()
	for _, ch := range chs {
		_ = ch.Close()
	}
}

func doForwardStop(id string) (map[string]any, error) {
	fwdMu.Lock()
	fw, ok := forwarders[id]
	if ok {
		delete(forwarders, id)
	}
	fwdMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("转发 %q 不存在，可用 ssh_forward_list 查看当前转发", id)
	}
	fw.stopAll()
	res := fw.snapshot()
	res["ok"] = true
	res["note"] = "已停止监听并关闭活动连接"
	return res, nil
}

func doForwardList() (map[string]any, error) {
	fwdMu.Lock()
	defer fwdMu.Unlock()
	out := make([]map[string]any, 0, len(forwarders))
	for _, fw := range forwarders {
		out = append(out, fw.snapshot())
	}
	return map[string]any{"count": len(out), "forwards": out}, nil
}

// cleanupForwarders 回收所属连接已断开/超时的转发（由 janitor 调用）。
func cleanupForwarders(maxIdle time.Duration, now time.Time) {
	fwdMu.Lock()
	items := make([]*forwarder, 0, len(forwarders))
	for _, fw := range forwarders {
		items = append(items, fw)
	}
	fwdMu.Unlock()

	for _, fw := range items {
		sessMu.Lock()
		_, alive := sessions[fw.SSHID]
		sessMu.Unlock()
		fw.mu.Lock()
		idle := now.Sub(fw.lastUsed)
		fw.mu.Unlock()
		if !alive || (maxIdle > 0 && idle > maxIdle) {
			fw.stopAll()
			fwdMu.Lock()
			delete(forwarders, fw.ID)
			fwdMu.Unlock()
			log.Printf("转发 %s 已回收（连接存活=%v，空闲 %s）", fw.ID, alive, idle)
		}
	}
}
