package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path" // 远程路径必须用 unix 语义的 path 包，而非 filepath（跨平台正确性）
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

const (
	defaultReadMax = 512 * 1024
	hardReadMax    = 2 * 1024 * 1024
)

func openSFTP(s *sshSession) (*sftp.Client, error) {
	cli, err := sftp.NewClient(s.Client)
	if err != nil {
		return nil, fmt.Errorf("启动 SFTP 子系统失败（对端可能未安装 sftp-server）: %w", err)
	}
	return cli, nil
}

// ensureRemoteDir 逐级创建远程目录（等价 mkdir -p），已存在的层级自动跳过。
func ensureRemoteDir(cli *sftp.Client, dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	cur := ""
	for _, p := range strings.Split(strings.Trim(path.Clean(dir), "/"), "/") {
		cur += "/" + p
		if err := cli.Mkdir(cur); err != nil {
			if fi, serr := cli.Stat(cur); serr == nil && fi.IsDir() {
				continue // 已存在
			}
			return err
		}
	}
	return nil
}

// ---------- 上传 / 下载 ----------

func doUpload(s *sshSession, localPath, remotePath string) (map[string]any, error) {
	localPath = expandHome(localPath)
	if localPath == "" || remotePath == "" {
		return nil, errors.New("local_path 与 remote_path 必填")
	}
	lf, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("打开本地文件失败: %w", err)
	}
	defer lf.Close()

	cli, err := openSFTP(s)
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	if err := ensureRemoteDir(cli, path.Dir(remotePath)); err != nil {
		log.Printf("创建远程父目录 %s 失败（继续尝试写入）: %v", path.Dir(remotePath), err)
	}
	rf, err := cli.Create(remotePath)
	if err != nil {
		return nil, fmt.Errorf("创建远程文件 %s 失败: %w", remotePath, err)
	}
	defer rf.Close()

	start := time.Now()
	n, err := io.Copy(rf, lf)
	if err != nil {
		return nil, fmt.Errorf("传输失败: %w", err)
	}
	s.touch()
	return map[string]any{
		"ok": true, "local_path": localPath, "remote_path": remotePath,
		"bytes": n, "duration_ms": time.Since(start).Milliseconds(),
	}, nil
}

func doDownload(s *sshSession, remotePath, localPath string) (map[string]any, error) {
	localPath = expandHome(localPath)
	if localPath == "" || remotePath == "" {
		return nil, errors.New("local_path 与 remote_path 必填")
	}
	cli, err := openSFTP(s)
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	rf, err := cli.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("打开远程文件 %s 失败: %w", remotePath, err)
	}
	defer rf.Close()

	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return nil, fmt.Errorf("创建本地目录失败: %w", err)
	}
	lf, err := os.Create(localPath)
	if err != nil {
		return nil, fmt.Errorf("创建本地文件失败: %w", err)
	}
	defer lf.Close()

	start := time.Now()
	n, err := io.Copy(lf, rf)
	if err != nil {
		return nil, fmt.Errorf("传输失败: %w", err)
	}
	s.touch()
	return map[string]any{
		"ok": true, "remote_path": remotePath, "local_path": localPath,
		"bytes": n, "duration_ms": time.Since(start).Milliseconds(),
	}, nil
}

// ---------- 目录 / 文件管理 ----------

func doList(s *sshSession, remotePath string, showHidden bool) (map[string]any, error) {
	if remotePath == "" {
		remotePath = "."
	}
	cli, err := openSFTP(s)
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	infos, err := cli.ReadDir(remotePath)
	if err != nil {
		return nil, fmt.Errorf("读取目录 %s 失败: %w", remotePath, err)
	}
	s.touch()
	entries := make([]map[string]any, 0, len(infos))
	for _, fi := range infos {
		if !showHidden && stringsBeginWithDot(fi.Name()) {
			continue
		}
		entries = append(entries, map[string]any{
			"name":     fi.Name(),
			"is_dir":   fi.IsDir(),
			"size":     fi.Size(),
			"mode":     fi.Mode().String(),
			"modified": fi.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	return map[string]any{"path": remotePath, "count": len(entries), "entries": entries}, nil
}

func stringsBeginWithDot(name string) bool {
	return len(name) > 0 && name[0] == '.'
}

func doMkdir(s *sshSession, remotePath string) (map[string]any, error) {
	if remotePath == "" {
		return nil, errors.New("remote_path 必填")
	}
	cli, err := openSFTP(s)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	if err := cli.MkdirAll(remotePath); err != nil {
		// MkdirAll 在部分设备上不可靠，逐级兜底
		if err2 := ensureRemoteDir(cli, remotePath); err2 != nil {
			return nil, fmt.Errorf("创建目录 %s 失败: %w", remotePath, err)
		}
	}
	s.touch()
	return map[string]any{"ok": true, "path": remotePath}, nil
}

func doRemove(s *sshSession, remotePath string, recursive bool) (map[string]any, error) {
	if remotePath == "" || remotePath == "/" {
		return nil, errors.New("remote_path 必填，且不允许删除根目录")
	}
	cli, err := openSFTP(s)
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	fi, err := cli.Stat(remotePath)
	if err != nil {
		return nil, fmt.Errorf("路径 %s 不存在: %w", remotePath, err)
	}
	if fi.IsDir() && !recursive {
		return nil, fmt.Errorf("%s 是目录，如需递归删除请设置 recursive=true", remotePath)
	}
	if err := sftpRemoveAll(cli, remotePath, fi.IsDir()); err != nil {
		return nil, fmt.Errorf("删除失败: %w", err)
	}
	s.touch()
	return map[string]any{"ok": true, "removed": remotePath}, nil
}

func sftpRemoveAll(cli *sftp.Client, path string, isDir bool) error {
	if !isDir {
		return cli.Remove(path)
	}
	entries, err := cli.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		child := path + "/" + e.Name()
		if e.IsDir() {
			if err := sftpRemoveAll(cli, child, true); err != nil {
				return err
			}
		} else if err := cli.Remove(child); err != nil {
			return err
		}
	}
	return cli.RemoveDirectory(path)
}

// ---------- 文本读写 ----------

func doRead(s *sshSession, remotePath string, maxBytes int64) (map[string]any, error) {
	if remotePath == "" {
		return nil, errors.New("remote_path 必填")
	}
	if maxBytes <= 0 {
		maxBytes = defaultReadMax
	}
	if maxBytes > hardReadMax {
		maxBytes = hardReadMax
	}
	cli, err := openSFTP(s)
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	rf, err := cli.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("打开远程文件 %s 失败: %w", remotePath, err)
	}
	defer rf.Close()

	fi, err := rf.Stat()
	if err != nil {
		return nil, fmt.Errorf("获取文件信息失败: %w", err)
	}

	buf := make([]byte, maxBytes)
	n, err := io.ReadFull(rf, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("读取失败: %w", err)
	}
	s.touch()
	res := map[string]any{
		"path":    remotePath,
		"size":    fi.Size(),
		"content": toUTF8(buf[:n]),
	}
	if fi.Size() > maxBytes {
		res["truncated"] = true
		res["note"] = fmt.Sprintf("文件共 %d 字节，仅读取前 %d 字节；可用 sftp_download 下载完整文件", fi.Size(), maxBytes)
	}
	return res, nil
}

func doWrite(s *sshSession, remotePath, content string, appendMode bool) (map[string]any, error) {
	if remotePath == "" {
		return nil, errors.New("remote_path 必填")
	}
	cli, err := openSFTP(s)
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	if err := ensureRemoteDir(cli, path.Dir(remotePath)); err != nil {
		log.Printf("创建远程父目录失败（继续尝试写入）: %v", err)
	}

	// pkg/sftp 的 OpenFile 使用 Go 标准 os flag（非 SFTP 协议标志）
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if appendMode {
		flags = os.O_WRONLY | os.O_CREATE
	}
	rf, err := cli.OpenFile(remotePath, flags)
	if err != nil {
		return nil, fmt.Errorf("打开远程文件 %s 失败: %w", remotePath, err)
	}
	defer rf.Close()

	data := []byte(content)
	var n int
	if appendMode {
		// 用 WriteAt 追加到文件尾，不依赖对端对 SSH_FXF_APPEND 的支持
		var offset int64
		if fi, err := rf.Stat(); err == nil {
			offset = fi.Size()
		}
		n, err = rf.WriteAt(data, offset)
	} else {
		n, err = rf.Write(data)
	}
	if err != nil {
		return nil, fmt.Errorf("写入失败: %w", err)
	}
	s.touch()
	return map[string]any{
		"ok": true, "path": remotePath, "bytes": n, "append": appendMode,
	}, nil
}
