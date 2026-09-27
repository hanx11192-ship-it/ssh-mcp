# ssh-mcp — 让 Agent 通过 MCP 管理 SSH 设备

![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
![MCP](https://img.shields.io/badge/MCP-Model%20Context%20Protocol-8A2BE2)
![Version](https://img.shields.io/badge/version-1.2.0-green)
![License](https://img.shields.io/badge/license-MIT-blue)

**单个二进制、零运行时依赖**的 MCP (Model Context Protocol) 服务器。它把 SSH 封装成 21 个标准工具，任何支持 MCP 的客户端（WorkBuddy / CodeBuddy / Claude Desktop / Claude Code / Cursor / Cline / Cherry Studio …）都能据此直接连接并管理局域网内的设备：NAS、路由器、交换机、树莓派、开发板、服务器……支持密码、私钥、keyboard-interactive（网络设备常用）三种认证，还支持**sudo 免交互执行**、**交互式 PTY 会话**与**本地端口转发**。

> 纯 Go 实现（官方 MCP SDK v1.8.0 + x/crypto/ssh + pkg/sftp），`CGO_ENABLED=0` 静态编译，**Windows 免安装任何东西，Android 在 Termux 中直接运行**。v1.2.0。

## 为什么是它

- **单文件部署**：7 平台预编译二进制见 `dist/`，复制即用，无 Node/Python 运行时
- **agent 友好**：输出自动截断防爆上下文、ANSI 剥离、命令黑名单、超时 SIGKILL、结构化结果
- **凭据不出本机**：`servers.json` 预配置 + by-id 连接，密码/私钥不进入对话
- **sudo 不卡壳**：`ssh_sudo_exec` 免交互输密码（stdin 通道，不进远端进程列表）
- **双传输**：stdio（客户端拉起）+ Streamable HTTP（手机 agent / 工作流平台直连 URL）

## 目录内容

| 路径 | 说明 |
|---|---|
| `dist/ssh-mcp-windows-amd64.exe` | Windows x64（大多数 Windows 电脑） |
| `dist/ssh-mcp-windows-arm64.exe` | Windows on ARM |
| `dist/ssh-mcp-linux-amd64` | Linux x64（大多数 PC / 服务器 / NAS） |
| `dist/ssh-mcp-linux-arm64` | Linux/Android arm64（**Termux 直接跑**、树莓派 4/5、ARM NAS） |
| `dist/ssh-mcp-linux-arm` | Linux armv6/7（老树莓派、部分路由器） |
| `dist/ssh-mcp-darwin-amd64` | macOS Intel |
| `dist/ssh-mcp-darwin-arm64` | macOS Apple Silicon (M1-M4) |
| `src/` | 完整 Go 源码（可自行构建） |
| `servers.json.example` | 预配置服务器示例 |

## 仓库结构

```
ssh-mcp/
├── README.md              # 本文档
├── LICENSE                # MIT
├── servers.json.example   # 预配置服务器示例（复制为 ~/.ssh-mcp/servers.json）
├── src/                   # Go 源码
│   ├── main.go            # 入口：stdio / HTTP 传输选择
│   ├── server.go          # 21 个 MCP 工具注册
│   ├── ssh.go             # 连接管理、认证（TOFU 主机校验）、命令执行、sudo
│   ├── sftp.go            # SFTP 上传/下载/目录/读写
│   ├── pty.go             # 交互式 PTY 会话（ANSI 剥离、环形缓冲）
│   ├── forward.go         # 本地端口转发（direct-tcpip 通道）
│   ├── config.go          # 预配置、黑名单、数据目录
│   ├── go.mod / go.sum    # 依赖清单
│   └── e2e_test.py        # 端到端测试（33 项断言，走真实 SSH）
└── dist/                  # 7 平台预编译二进制
```

## 从源码构建

需要 Go ≥ 1.26（国内网络建议先设置模块代理）：

```bash
export GOPROXY=https://goproxy.cn,direct   # 可选，国内加速
cd src
go build -trimpath -ldflags="-s -w" -o ssh-mcp .
```

全平台交叉编译（纯静态，无需任何 C 工具链）：

```bash
for t in windows/amd64 windows/arm64 linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64; do
  os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=".exe"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags="-s -w" \
    -o "../dist/ssh-mcp-$os-$arch$ext" .
done
```

## 运行测试

`src/e2e_test.py` 通过 MCP stdio 协议逐工具驱动二进制，共 33 项断言
（连接/密钥/密码、exec、sudo 三路径、SFTP 全流程、PTY 会话、端口转发、黑名单等），
需要一个可登录的 SSH 服务器：

```bash
# 准备：目标机开启 sshd，root 或普通用户可登录
python3 src/e2e_test.py ./dist/ssh-mcp-linux-amd64
```

## 快速开始

### 1. WorkBuddy 接入完整示例

下面以 **Windows + WorkBuddy** 为例，走一遍从零到可用的完整流程。其他 MCP 客户端思路相同，只是配置文件位置和「信任」入口不同（见第 2 节）。

#### 第 1 步：放置二进制

把 `dist/ssh-mcp-windows-amd64.exe` 放到一个固定目录，例如 `D:\mcp\ssh\`。
（Linux / macOS 换成对应平台的二进制，路径按需调整。）

#### 第 2 步：写连接器配置

编辑 `~/.workbuddy-ai/mcp.json`（Windows 即 `C:\Users\<用户名>\.workbuddy-ai\mcp.json`），文件不存在就新建：

```json
{
  "mcpServers": {
    "ssh-tool": {
      "command": "D:\\mcp\\ssh\\ssh-mcp-windows-amd64.exe",
      "args": [],
      "env": {
        "SSH_MCP_HOME": "D:\\mcp\\ssh"
      }
    }
  }
}
```

要点：

- `command` 必须写**绝对路径**；Windows 路径的反斜杠要双写（`\\`）
- `env.SSH_MCP_HOME` 指定数据目录，`servers.json` 与 `known_hosts` 都放这里；不写则默认 `~/.ssh-mcp`
- 键名 `ssh-tool` 可自定义，会成为工具名前缀（如 `ssh_connect_by_id` 实际是 `<前缀>__ssh_connect_by_id`）

#### 第 3 步：在 WorkBuddy 里启用连接器

新增的 MCP 服务器**不会自动生效**，需要手动信任一次：

1. 重启 WorkBuddy（或重载当前会话）—— 工具索引是**会话启动时**快照的，不重载看不到新工具
2. 打开左侧「**连接器**」→ 右上角「**自定义连接器**」入口
3. 找到 `ssh-tool`，点「**信任**」启用

启用后对话里就能看到 21 个 `ssh_*` / `sftp_*` 工具了。

#### 第 4 步：配置设备清单（可选，但强烈推荐）

编辑 `D:\mcp\ssh\servers.json`（即上一步 `SSH_MCP_HOME` 指向的目录）：

```json
{
  "servers": [
    { "id": "nas",    "host": "192.168.1.10", "username": "admin",
      "private_key_path": "~/.ssh/id_ed25519" },
    { "id": "router", "host": "192.168.1.1",  "username": "root",
      "password": "改成你的密码" },
    { "id": "note2",  "host": "192.168.10.9", "port": 22, "username": "umeko",
      "password": "改成你的密码", "sudo_password": "改成你的 sudo 密码" }
  ]
}
```

填好后 agent 只需 `ssh_connect_by_id(server_id="nas")`，**密码/私钥留在本机、不会进入对话上下文**。改完保存即生效，无需重启。

#### 第 5 步：用自然语言指挥

配置完就不用管协议了，直接说人话。真实示例：

> **你**：连上 note2，看看磁盘和内存占用

agent 会自己串起工具调用：

```
ssh_connect_by_id(server_id="note2")
  → {"ok": true, "session_id": "192.168.10.9:22", "username": "umeko", ...}

ssh_exec(session_id="192.168.10.9:22", command="df -h / ; free -h")
  → Filesystem      Size  Used Avail Use% Mounted on
    /dev/sda15      113G  3.5G  105G   4% /
    Mem:           5.5Gi  214Mi  5.0Gi
```

> **你**：用 sudo 看下 docker 容器

```
ssh_sudo_exec(session_id="192.168.10.9:22", command="docker ps -a")
  → {"as": "sudo", "exit_code": 0, "stdout": "..."}
```

注意：`ssh_sudo_exec` 的 command **不需要写 `sudo` 前缀**；密码通过 stdin 传给 `sudo -S`，不会出现在远端进程列表或命令行里。

> **你**：把这份配置传到 NAS 的 /etc/ 下

```
sftp_upload(session_id="...", local_path="D:\\conf\\app.conf",
            remote_path="/etc/app.conf")
```

> **你**：开个终端，用 top 看看

```
ssh_shell_start(session_id="...")        # 交互式 PTY，可跑 top / vim 等全屏程序
ssh_shell_send(shell_id="...", input="top\n")
ssh_shell_close(shell_id="...")
```

#### 第 6 步：收尾

```
ssh_disconnect(session_id="192.168.10.9:22")
```

#### 常见问题

| 现象 | 原因 / 解决 |
|---|---|
| 对话里看不到 ssh 工具 | 没重载会话，或连接器没点「信任」。工具索引是会话启动时的快照 |
| `dial tcp ... i/o timeout` | 设备没开机 / IP 变了 / 不在同一网段。先用 `ping` 确认（设备刚开机时 sshd 可能还没起来，等十几秒再试） |
| `host key mismatch` | 设备重装或换了主机密钥。确认无中间人风险后，删掉 `known_hosts` 中该主机的行再连 |
| `servers.json` 改了没生效 | 检查 JSON 语法。非法 JSON 会被静默忽略，表现为 `ssh_list_servers` 返回 `count: 0` |
| sudo 报 `a password is required` | 未提供 `sudo_password` 且远端未配 NOPASSWD。在 `servers.json` 里补 `sudo_password`，或给该用户配 `NOPASSWD` |
| 改了 `mcp.json` 不生效 | 需要重启 WorkBuddy 并重新「信任」连接器 |

### 2. 通用 MCP 客户端配置（Windows / Linux / macOS）

以 Windows x64 为例，在 MCP 客户端配置中添加（其他平台替换二进制路径即可）：

```json
{
  "mcpServers": {
    "ssh": {
      "command": "C:\\tools\\ssh-mcp-windows-amd64.exe"
    }
  }
}
```

Linux / macOS：

```json
{
  "mcpServers": {
    "ssh": {
      "command": "/usr/local/bin/ssh-mcp-linux-amd64"
    }
  }
}
```

保存后重启客户端，直接用自然语言指挥 agent：「连上 192.168.1.10 看看磁盘占用」「把这份配置传到 NAS 的 /etc/」。

### 3. Android（Termux）

`ssh-mcp-linux-arm64` 是纯静态二进制，在 Termux 中无需 root 直接运行：

```bash
cp ssh-mcp-linux-arm64 ~/bin/ssh-mcp && chmod +x ~/bin/ssh-mcp
```

- 手机上的 agent 若支持本地 stdio MCP：command 填 Termux 路径（如 `/data/data/com.termux/files/home/bin/ssh-mcp`）。
- 更通用的做法（任何手机 agent 都能用）：**把 ssh-mcp 以 HTTP 模式跑在局域网内一台常开设备上**，手机 agent 通过 URL 连接，见下节。

### 4. HTTP 模式（手机 agent / 无法拉起本地进程的客户端）

```bash
./ssh-mcp-linux-amd64 -http :8899          # 局域网内任意一台设备/NAS 上运行
```

客户端配置（type 为 http / url 的客户端）：

```json
{
  "mcpServers": {
    "ssh": { "type": "http", "url": "http://192.168.1.2:8899/mcp" }
  }
}
```

建议配合 Bearer Token（环境变量 `SSH_MCP_HTTP_TOKEN`）：

```bash
SSH_MCP_HTTP_TOKEN=your-secret ./ssh-mcp -http :8899
```

客户端 URL 可写成 `http://192.168.1.2:8899/mcp`，并在 header 中带 `Authorization: Bearer your-secret`（支持该选项的客户端）。

## 21 个工具一览

| 工具 | 说明 |
|---|---|
| `ssh_connect` | 建立连接，返回 `session_id`；支持 password / private_key / private_key_path，缺省回退 `~/.ssh` 默认密钥；可选保存 `sudo_password` |
| `ssh_connect_by_id` | 按 id 连接预配置服务器（凭据留在本机，不进对话） |
| `ssh_list_servers` | 列出预配置服务器（不显示明文密码） |
| `ssh_list_sessions` | 列出活跃会话 |
| `ssh_disconnect` | 断开会话 |
| `ssh_exec` | 执行 shell 命令，返回 stdout / stderr / exit_code；支持 cwd、超时自动 SIGKILL |
| `ssh_sudo_exec` | 以 **sudo** 执行命令（免交互输密码，见下文「sudo 与 root 权限」）；黑名单同样生效 |
| `sftp_upload` / `sftp_download` | 上传 / 下载文件（父目录自动创建） |
| `sftp_list` / `sftp_mkdir` / `sftp_remove` | 列目录 / 递归建目录 / 删除（目录递归删） |
| `sftp_read` / `sftp_write` | 读 / 写远程文本文件（支持追加） |
| `ssh_shell_start` | 启动**交互式 PTY 会话**（登录 shell 或指定命令），可跑 vim/top 等全屏程序 |
| `ssh_shell_send` | 向 PTY 发送输入并收取新输出（输出已剥离 ANSI、规范换行） |
| `ssh_shell_read` | 轮询读取 PTY 新输出（可等待） |
| `ssh_shell_close` | 关闭 PTY 会话 |
| `ssh_forward_start` | 建立**本地端口转发**（等同 `ssh -L`），可穿透访问设备内网服务 |
| `ssh_forward_list` / `ssh_forward_stop` | 查看转发状态 / 停止转发 |

## sudo 与 root 权限

非交互式 SSH 下 `sudo` 会卡在密码提示，本工具提供 `ssh_sudo_exec` 专门解决。密码来源优先级：

1. **调用时传入** `sudo_password` —— 适合临时用一次
2. **建连时保存**：`ssh_connect` 传 `sudo_password`，或预配置里写 `"sudo_password": "..."` —— 密码只存本机内存/配置文件，不进对话
3. **远端 NOPASSWD**：两者都没有时走 `sudo -n`，要求远端已配置免密 sudo

远端推荐配置（一劳永逸，安全性也最好）：

```bash
# 在远程设备上以管理员执行（或 sudo visudo 编辑）
echo "admin ALL=(ALL) NOPASSWD: ALL" | sudo tee /etc/sudoers.d/admin
```

要点：
- 密码通过 stdin 传给 `sudo -S`，**不会出现在远端进程列表或命令行里**
- `ssh_sudo_exec` 的 command 不需要写 `sudo` 前缀
- sudo 失败时返回可读诊断：未提供密码 / 密码不正确 / 用户不在 sudoers
- 直接用 root 密钥登录的设备（多数 NAS/路由器）无需以上任何配置，`ssh_exec` 即可

## 预配置服务器（推荐）

把常用设备写进 `~/.ssh-mcp/servers.json`（或 `SSH_MCP_HOME` 指定的目录）：

```json
{
  "servers": [
    { "id": "nas",     "host": "192.168.1.10", "username": "admin",
      "private_key_path": "~/.ssh/id_ed25519" },
    { "id": "router",  "host": "192.168.1.1",  "username": "root", "password": "..." },
    { "id": "pi",      "host": "192.168.1.20", "port": 2222, "username": "pi", "password": "..." }
  ]
}
```

之后 agent 只需 `ssh_connect_by_id(server_id="nas")`，密码/私钥**永远不进入对话上下文**。也可用环境变量等价配置（JSON 数组）：

```json
{
  "mcpServers": {
    "ssh": {
      "command": "ssh-mcp",
      "env": {
        "SSH_MCP_SERVERS": "[{\"id\":\"nas\",\"host\":\"192.168.1.10\",\"username\":\"admin\",\"password\":\"...\"}]"
      }
    }
  }
}
```

## 安全设计

- **凭据不出本机**：认证参数只在本进程内使用；推荐用 servers.json 预配置，避免凭据出现在对话里
- **TOFU 主机密钥校验**：首次连接自动记录指纹到 `~/.ssh-mcp/known_hosts`，之后密钥不匹配会拒绝连接并提示中间人风险；`host_key_policy=strict` 可拒绝陌生主机
- **命令黑名单**：默认拦截 `rm -rf /`、`mkfs`、`dd` 写整盘、fork 炸弹等毁灭性命令；`SSH_MCP_CMD_BLACKLIST` 自定义或 `off` 关闭（注：黑名单作用于 `ssh_exec`；交互式 PTY 中的输入不受其限制）
- **超时保护**：命令默认 30s 超时（上限 3600s），超时自动 SIGKILL；空闲会话（SSH 连接 / PTY / 转发）默认 60 分钟自动回收
- **转发默认仅本机监听**：`ssh_forward_start` 的 `local_host` 默认 `127.0.0.1`，避免意外向局域网暴露内网服务
- **输出限流**：单次输出截断至 128KB（保留头尾），防止长输出撑爆模型上下文
- **HTTP 模式鉴权**：`SSH_MCP_HTTP_TOKEN` 可选 Bearer Token

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `SSH_MCP_HOME` | `~/.ssh-mcp` | 数据目录（servers.json / known_hosts） |
| `SSH_MCP_SERVERS` | - | 预配置服务器 JSON（优先于文件） |
| `SSH_MCP_CMD_BLACKLIST` | 内置规则 | 命令黑名单正则（逗号分隔），`off` 关闭 |
| `SSH_MCP_HOST_KEY_POLICY` | `auto_accept` | `strict` 拒绝陌生主机密钥 |
| `SSH_MCP_SESSION_TTL` | `60` | 空闲会话回收分钟数，`-1` 关闭 |
| `SSH_MCP_HTTP_TOKEN` | 空 | HTTP 模式 Bearer Token |

## 常见问题

**Q：连接报「host key mismatch」？**
设备重装/更换了主机密钥。确认无中间人风险后，删除 `~/.ssh-mcp/known_hosts` 中该主机的行再连。

**Q：SFTP 报「启动 SFTP 子系统失败」？**
对端未启用 SFTP 子系统（部分嵌入式设备）。可改用 `ssh_exec` + `cat`/`>` 读写小文件。

**Q：执行命令要交互确认怎么办？**
MCP 的 SSH 执行是非交互式的，请用非交互参数：`apt install -y`、`DEBIAN_FRONTEND=noninteractive`、`yes | ...` 等。

**Q：从源码构建？**
见上文「从源码构建」。需 Go ≥ 1.26，模块代理 `GOPROXY=https://goproxy.cn,direct`。

**Q：测试？**
`src/e2e_test.py` 是完整的 MCP 协议端到端测试（33 项断言），需一个可登录的 SSH 服务：`python3 e2e_test.py ./ssh-mcp`

## License

[MIT](LICENSE) © ssh-mcp contributors
