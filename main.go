package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var buildVersion = "dev"

func main() {
	if err := run(); err != nil {
		log.Printf("程序退出: %v", err)
		showErrorMessage("内网文件共享", err.Error())
		os.Exit(1)
	}
}

func run() error {
	listenAddr := flag.String("listen", "0.0.0.0:3923", "监听地址")
	dataDirFlag := flag.String("data-dir", "", "配置和日志目录")
	noBrowser := flag.Bool("no-browser", false, "启动后不自动打开浏览器")
	showVersion := flag.Bool("version", false, "显示版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Printf("LanFileHub %s\n", buildVersion)
		return nil
	}

	dataDir, err := applicationDataDir(*dataDirFlag)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("创建程序数据目录失败: %w", err)
	}
	logFile, err := configureLogging(dataDir)
	if err != nil {
		return fmt.Errorf("创建日志文件失败: %w", err)
	}
	defer logFile.Close()
	log.Printf("LanFileHub %s 正在启动", buildVersion)

	localURL := localURLFor(*listenAddr)
	if existingInstance(localURL) {
		log.Printf("检测到已经运行的实例")
		if !*noBrowser {
			_ = openBrowser(localURL)
		}
		return nil
	}

	cfg, err := loadConfig(filepath.Join(dataDir, "config.json"))
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	app := newApp(dataDir, *listenAddr, buildVersion, cfg)

	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		return fmt.Errorf("无法监听 %s，请检查端口是否已被占用: %w", *listenAddr, err)
	}
	defer listener.Close()

	server := &http.Server{
		Addr:              *listenAddr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Hour,
		WriteTimeout:      2 * time.Hour,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	app.setServer(server)

	janitorContext, stopJanitor := context.WithCancel(context.Background())
	defer stopJanitor()
	go app.sessionJanitor(janitorContext)

	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupts)
	go func() {
		<-interrupts
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	log.Printf("服务已启动: %s", localURL)
	if !*noBrowser {
		go func() {
			time.Sleep(250 * time.Millisecond)
			if err := openBrowser(localURL); err != nil {
				log.Printf("自动打开浏览器失败: %v", err)
			}
		}()
	}

	err = server.Serve(listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Printf("服务已停止")
	return nil
}

func applicationDataDir(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "LanFileHub"), nil
}

func configureLogging(dataDir string) (*os.File, error) {
	path := filepath.Join(dataDir, "lan-file-hub.log")
	if info, err := os.Stat(path); err == nil && info.Size() > 5*1024*1024 {
		backup := path + ".1"
		_ = os.Remove(backup)
		if err := os.Rename(path, backup); err != nil {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(file))
	return file, nil
}

func localURLFor(listenAddr string) string {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		port = "3923"
	}
	return "http://127.0.0.1:" + port
}

func existingInstance(baseURL string) bool {
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(baseURL + "/api/health")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode == http.StatusOK && response.Header.Get("X-Lan-File-Hub") == "1"
}
