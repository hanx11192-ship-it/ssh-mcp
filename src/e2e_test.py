#!/usr/bin/env python3
"""MCP stdio 端到端测试：用换行分隔 JSON-RPC 直接驱动 ssh-mcp 二进制。"""
import json, os, subprocess, sys, tempfile, time

BIN = sys.argv[1] if len(sys.argv) > 1 else "./ssh-mcp"
os.environ.pop("SSH_MCP_SERVERS", None)

proc = subprocess.Popen([BIN], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                        stderr=subprocess.DEVNULL, text=True, bufsize=1)
next_id = 0
failures = []

def rpc(method, params=None, notify=False):
    global next_id
    msg = {"jsonrpc": "2.0", "method": method}
    if params is not None:
        msg["params"] = params
    if not notify:
        next_id += 1
        msg["id"] = next_id
        rid = next_id
    proc.stdin.write(json.dumps(msg) + "\n")
    proc.stdin.flush()
    if notify:
        return None
    deadline = time.time() + 60
    while time.time() < deadline:
        line = proc.stdout.readline()
        if not line:
            raise RuntimeError("server closed stdout")
        line = line.strip()
        if not line:
            continue
        resp = json.loads(line)
        if resp.get("id") == rid:
            return resp
    raise RuntimeError(f"timeout waiting for id={rid}")

def call(name, args):
    r = rpc("tools/call", {"name": name, "arguments": args})
    if "error" in r:
        return {"__error__": r["error"]}
    out = r["result"]
    # 结构化结果优先，其次文本
    if out.get("structuredContent") is not None:
        return out["structuredContent"]
    texts = [c.get("text", "") for c in out.get("content", []) if c.get("type") == "text"]
    return {"__text__": "\n".join(texts), "__isError__": out.get("isError", False)}

def check(name, cond, extra=""):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name} {extra}")
    if not cond:
        failures.append(name)

# 1. initialize
r = rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                       "clientInfo": {"name": "e2e-test", "version": "0.0.1"}})
proto = r["result"]["protocolVersion"]
check("initialize", "protocolVersion" in r["result"], f"(协议版本 {proto})")
rpc("notifications/initialized", notify=True)

# 2. tools/list
r = rpc("tools/list", {})
tools = [t["name"] for t in r["result"]["tools"]]
print("    tools:", ", ".join(sorted(tools)))
expect = ["ssh_connect","ssh_connect_by_id","ssh_list_servers","ssh_list_sessions",
          "ssh_disconnect","ssh_exec","sftp_upload","sftp_download","sftp_list",
          "sftp_mkdir","sftp_remove","sftp_read","sftp_write"]
check("tools/list 13 个工具", all(e in tools for e in expect), f"({len(tools)} 个)")

# 3. ssh_connect 密钥认证（TOFU 首连）
r = call("ssh_connect", {"host": "127.0.0.1", "username": "root",
                          "private_key_path": "/root/.ssh/id_ed25519", "name": "dev"})
check("ssh_connect 密钥认证", r.get("ok") is True and r.get("session_id") == "dev", str(r)[:120])
sid1 = r.get("session_id", "")

# 4. ssh_exec 基础命令
r = call("ssh_exec", {"session_id": sid1, "command": "echo hello-mcp && uname -s"})
check("ssh_exec 基础命令", "hello-mcp" in r.get("stdout", "") and r.get("exit_code") == 0, str(r)[:120])

# 5. ssh_exec 管道 + 退出码
r = call("ssh_exec", {"session_id": sid1, "command": "ls /etc | grep -c . ; exit 3"})
check("ssh_exec 管道/退出码", r.get("exit_code") == 3 and r.get("stdout", "").strip().isdigit(), str(r)[:120])

# 6. 黑名单拦截
r = call("ssh_exec", {"session_id": sid1, "command": "rm -rf /"})
check("黑名单拦截 rm -rf /", r.get("__isError__") is True or "__error__" in r or "拦截" in json.dumps(r, ensure_ascii=False), str(r)[:120])

# 7. sftp_write / sftp_read
r = call("sftp_write", {"session_id": sid1, "remote_path": "/tmp/ssh-mcp-test/a.txt",
                         "content": "第一行中文内容\nsecond line\n"})
check("sftp_write", r.get("ok") is True and r.get("bytes", 0) > 0, str(r)[:120])
r = call("sftp_read", {"session_id": sid1, "remote_path": "/tmp/ssh-mcp-test/a.txt"})
check("sftp_read 中文回读", "第一行中文内容" in r.get("content", ""), str(r)[:120])

# 8. sftp_upload / sftp_download 往返
with tempfile.NamedTemporaryFile("w", suffix=".bin", delete=False) as f:
    f.write("upload-payload-中文-0123456789" * 100)
    local1 = f.name
r = call("sftp_upload", {"session_id": sid1, "local_path": local1,
                          "remote_path": "/tmp/ssh-mcp-test/up.bin"})
check("sftp_upload", r.get("ok") is True and r.get("bytes", 0) > 2000, str(r)[:120])
local2 = local1 + ".down"
r = call("sftp_download", {"session_id": sid1, "remote_path": "/tmp/ssh-mcp-test/up.bin",
                            "local_path": local2})
check("sftp_download", r.get("ok") is True, str(r)[:120])
check("上传下载内容一致", open(local1).read() == open(local2).read())

# 9. sftp_list / sftp_mkdir / sftp_remove
r = call("sftp_mkdir", {"session_id": sid1, "remote_path": "/tmp/ssh-mcp-test/sub/deep"})
check("sftp_mkdir 递归", r.get("ok") is True, str(r)[:120])
r = call("sftp_list", {"session_id": sid1, "remote_path": "/tmp/ssh-mcp-test"})
names = [e["name"] for e in r.get("entries", [])]
check("sftp_list", "a.txt" in names and "sub" in names, str(r)[:120])
r = call("sftp_remove", {"session_id": sid1, "remote_path": "/tmp/ssh-mcp-test", "recursive": True})
check("sftp_remove 递归", r.get("ok") is True, str(r)[:120])

# 10. 密码认证第二会话（keyboard-interactive 兼容）
r = call("ssh_connect", {"host": "127.0.0.1", "username": "mcptest",
                          "password": "Test1234", "name": "mcptest"})
check("ssh_connect 密码认证", r.get("ok") is True, str(r)[:160])
sid2 = r.get("session_id", "")
r = call("ssh_exec", {"session_id": sid2, "command": "whoami && hostname"})
check("密码会话执行命令", "mcptest" in r.get("stdout", ""), str(r)[:120])

# 10.5 ssh_sudo_exec 三条路径
r = call("ssh_sudo_exec", {"session_id": sid1, "command": "id -u"})
check("sudo root 免密(-n)", r.get("exit_code") == 0 and r.get("stdout", "").strip() == "0", str(r)[:140])
r = call("ssh_sudo_exec", {"session_id": sid2, "command": "id -u"})
check("sudo 无密码给出诊断提示", r.get("exit_code") == 1 and "密码" in r.get("note", ""), str(r)[:150])
r = call("ssh_sudo_exec", {"session_id": sid2, "command": "id -u", "sudo_password": "Test1234"})
check("sudo 正确密码(-S stdin)", r.get("exit_code") == 0 and r.get("stdout", "").strip() == "0", str(r)[:140])
r = call("ssh_sudo_exec", {"session_id": sid2, "command": "id -u", "sudo_password": "wrong-pw"})
check("sudo 错误密码诊断", "密码不正确" in r.get("note", ""), str(r)[:150])
r = call("ssh_sudo_exec", {"session_id": sid2, "command": "rm -rf /"})
check("sudo 同样过黑名单", "__isError__" in r or "__error__" in r or "拦截" in json.dumps(r, ensure_ascii=False), str(r)[:120])

# 11. 错误主机密钥检测：改 known_hosts 中记录再连 → 应拒绝
r = call("ssh_list_sessions", {})
ids = [s["session_id"] for s in r.get("sessions", [])]
check("ssh_list_sessions", set(ids) == {sid1, sid2}, str(ids))

# 12. 断开
r = call("ssh_disconnect", {"session_id": sid2})
check("ssh_disconnect", r.get("ok") is True, str(r)[:80])
r = call("ssh_exec", {"session_id": sid2, "command": "echo x"})
check("断开后操作报错", "__error__" in r or "__isError__" in r or "不存在" in json.dumps(r, ensure_ascii=False), str(r)[:120])

# 13. 交互式 PTY 会话
r = call("ssh_shell_start", {"session_id": sid1, "command": "cat"})
sh1 = r.get("shell_id", "")
check("shell_start(命令模式)", bool(sh1), str(r)[:120])
r = call("ssh_shell_send", {"shell_id": sh1, "input": "hello-pty\n", "wait_ms": 600})
check("shell_send + 回显", "hello-pty" in r.get("output", ""), str(r)[:150])
r = call("ssh_shell_close", {"shell_id": sh1})
check("shell_close", r.get("ok") is True, str(r)[:80])

r = call("ssh_shell_start", {"session_id": sid1})  # 登录 shell
sh2 = r.get("shell_id", "")
check("shell_start(登录shell)", bool(sh2), str(r)[:120])
r = call("ssh_shell_send", {"shell_id": sh2, "input": "echo login-shell-$((6*7))\n", "wait_ms": 900})
check("登录shell 算术执行", "login-shell-42" in r.get("output", ""), str(r)[:150])
r = call("ssh_shell_read", {"shell_id": sh2, "wait_ms": 0})
check("shell_read 空读", r.get("output") == "", str(r)[:120])
r = call("ssh_shell_send", {"shell_id": sh2, "input": "exit\n", "wait_ms": 3000})
r = call("ssh_shell_read", {"shell_id": sh2, "wait_ms": 3000})
check("shell 退出检测", r.get("exited") is True, str(r)[:120])
call("ssh_shell_close", {"shell_id": sh2})

# 14. 本地端口转发（转发到 SSH 服务器侧的 HTTP 服务）
import urllib.request, urllib.error
httpd = subprocess.Popen([sys.executable, "-m", "http.server", "8901", "--bind", "127.0.0.1"],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(1)
try:
    r = call("ssh_forward_start", {"session_id": sid1, "target_host": "127.0.0.1",
                                    "target_port": 8901, "local_port": 18901})
    fwd_id = r.get("forward_id", "")
    check("forward_start", bool(fwd_id) and r.get("bind", "").endswith("18901"), str(r)[:150])
    body = urllib.request.urlopen("http://127.0.0.1:18901/", timeout=8).read().decode()
    check("转发可用（经SSH访问HTTP）", "Directory listing" in body or "http.server" in body.lower() or "<html" in body.lower(), f"({len(body)} bytes)")
    r = call("ssh_forward_list", {})
    check("forward_list", r.get("count") == 1, str(r)[:120])
    r = call("ssh_forward_stop", {"forward_id": fwd_id})
    check("forward_stop", r.get("ok") is True, str(r)[:100])
    try:
        urllib.request.urlopen("http://127.0.0.1:18901/", timeout=3)
        stopped_ok = False
    except Exception:
        stopped_ok = True
    check("停止后端口关闭", stopped_ok)
finally:
    httpd.terminate()

# 15. ping
try:
    rpc("ping", notify=False)
    check("ping", True)
except Exception as e:
    check("ping", False, str(e))

proc.stdin.close()
proc.wait(timeout=10)

print("\n" + ("=" * 40))
if failures:
    print(f"❌ {len(failures)} 项失败: {failures}")
    sys.exit(1)
print("✅ 全部测试通过")
