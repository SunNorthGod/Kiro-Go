// Package main provides the entry point for Kiro API Proxy.
//
// Kiro API Proxy is a reverse proxy service that translates Kiro API requests
// into OpenAI and Anthropic (Claude) compatible formats. Key features include:
//   - Multi-account pool with round-robin load balancing
//   - Automatic OAuth token refresh
//   - Streaming response support for real-time AI interactions
//   - Admin panel for account and configuration management
//
// The service exposes the following endpoints:
//   - /v1/messages - Claude API compatible endpoint
//   - /v1/chat/completions - OpenAI API compatible endpoint
//   - /admin - Web-based administration panel
package main

import (
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"kiro-go/proxy"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	// 配置文件路径，支持环境变量覆盖
	configPath := "data/config.json"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	// 确保数据目录存在
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// 加载配置
	if err := config.Init(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize log level: LOG_LEVEL env var takes priority over config, defaulting to "info".
	logger.Init(config.GetLogLevel())

	// Optionally switch persistence to PostgreSQL (via DATABASE_URL). When enabled,
	// accounts, API keys and usage/billing become PG-backed (existing JSON data is
	// migrated in on first run); server settings continue to live in the JSON file.
	// Must run before pool.GetPool() so the pool sees the DB-sourced accounts.
	if usingDB, err := config.EnableDatabaseFromEnv(); err != nil {
		log.Fatalf("Failed to initialize PostgreSQL backend: %v", err)
	} else if usingDB {
		logger.Infof("Persistence backend: PostgreSQL (DATABASE_URL)")
	} else {
		logger.Infof("Persistence backend: JSON file (%s)", configPath)
	}

	// 环境变量覆盖密码
	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		config.SetPassword(envPassword)
	}

	// Fail closed on an unset or default admin password. The admin panel controls
	// every account and the whole billing ledger, so a "changeme" password is a
	// full takeover vector. Operators MUST set a strong password (ADMIN_PASSWORD
	// env var, or the "password" field in the config file) before the service
	// will start. This is intentional (see ops doc): production already sets one.
	if pw := config.GetPassword(); pw == "" || pw == "changeme" {
		log.Fatalf("refusing to start: admin password is unset or the insecure default %q. Set a strong password via the ADMIN_PASSWORD environment variable (or the \"password\" field in %s) and restart.", "changeme", configPath)
	}

	// 初始化账号池
	pool.GetPool()

	// 创建 HTTP 处理器（包含后台刷新任务）
	handler := proxy.NewHandler()

	// 启动服务器
	addr := fmt.Sprintf("%s:%d", config.GetHost(), config.GetPort())
	logger.Infof("NorthGod Kiro-Go starting on http://%s (log level: %s)", addr, logger.LevelName(logger.GetLevel()))
	logger.Infof("Admin panel: http://%s/admin", addr)
	logger.Infof("Claude API: http://%s/v1/messages", addr)
	logger.Infof("OpenAI API: http://%s/v1/chat/completions", addr)

	// WriteTimeout intentionally 0: SSE streams can run for minutes while the
	// upstream model produces tokens. ReadHeaderTimeout + ReadTimeout still
	// guard against slowloris-style header/body stalls.
	srv := &http.Server{
		Addr:              addr,
		Handler:           proxy.WithGzip(handler),
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}
