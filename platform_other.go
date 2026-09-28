//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}

func openBrowser(url string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}

func showErrorMessage(title, message string) {
	_, _ = fmt.Fprintf(os.Stderr, "%s: %s\n", title, message)
}

func pickFolder(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("当前系统暂未提供原生目录选择窗口")
	}

	output, err := exec.CommandContext(ctx, "osascript", "-e", `POSIX path of (choose folder with prompt "请选择要共享的文件夹")`).Output()
	if err != nil {
		return "", errors.New("未选择文件夹")
	}

	folder := strings.TrimSpace(string(output))
	if folder == "" {
		return "", errors.New("未选择文件夹")
	}
	return folder, nil
}
