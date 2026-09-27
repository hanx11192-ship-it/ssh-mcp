// ssh-mcp：把 SSH 能力暴露为 MCP (Model Context Protocol) 服务的单二进制工具。
//
// 传输模式：
//   - stdio（默认）：由 MCP 客户端（WorkBuddy / Claude Desktop / Cursor 等）拉起
//   - streamable HTTP（-http :8899）：供手机 agent 等无法拉起本地进程的客户端通过局域网访问
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "1.2.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[ssh-mcp] ")

	var (
		httpAddr = flag.String("http", "", "启用 Streamable HTTP 传输并监听该地址（如 :8899）；缺省使用 stdio 传输")
		httpPath = flag.String("http-path", "/mcp", "HTTP 模式的端点路径（默认 /mcp）")
		showVer  = flag.Bool("version", false, "打印版本号")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("ssh-mcp", version)
		return
	}

	startJanitor()
	initBlacklist()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := newMCPServer()

	if *httpAddr != "" {
		mux := http.NewServeMux()
		mux.Handle(*httpPath, withAuth(mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return srv }, nil,
		)))
		log.Printf("HTTP 传输已启动，监听 %s%s", *httpAddr, *httpPath)
		log.Fatal(http.ListenAndServe(*httpAddr, mux))
		return
	}

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// withAuth：HTTP 模式可选 Bearer Token 鉴权（SSH_MCP_HTTP_TOKEN 环境变量）。
func withAuth(next http.Handler) http.Handler {
	token := os.Getenv("SSH_MCP_HTTP_TOKEN")
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
