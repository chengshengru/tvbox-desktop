//go:build !windows

package player

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"tvbox-desktop/internal/settings"
	"tvbox-desktop/models"
)

// detectPlatform Linux/macOS 桩实现：没有安装路径扫描与注册表关联，
// 只做设置里的 command + PATH 查找（which 语义），保证非 Windows 平台可编译、可基本使用。
func detectPlatform() ([]models.PlayerInfo, error) {
	prefer := preferOrder()
	found := []models.PlayerInfo{}
	seen := map[string]bool{}

	for _, name := range prefer {
		exe, err := exec.LookPath(strings.ToLower(name))
		if err != nil || seen[exe] {
			continue
		}
		seen[exe] = true
		found = append(found, models.PlayerInfo{Name: name, Path: exe, Source: "scan"})
	}

	manual := strings.TrimSpace(settings.Get().ExternalPlayer.Command)
	if manual != "" {
		path := manual
		if !existsPath(manual) {
			if p, err := exec.LookPath(manual); err == nil {
				path = p
			} else {
				path = ""
			}
		}
		if path != "" && !seen[path] {
			seen[path] = true
			found = append([]models.PlayerInfo{{Name: "设置里指定的", Path: path, Source: "manual"}}, found...)
		}
	}
	return found, nil
}

// resolvePlayerPlatform 用户手动指定的播放器优先（绝对路径或 PATH 里的命令名），
// 其次按偏好列表在本机找一个。
func resolvePlayerPlatform() (string, error) {
	manual := strings.TrimSpace(settings.Get().ExternalPlayer.Command)
	if manual != "" {
		if existsPath(manual) {
			return manual, nil
		}
		if p, err := exec.LookPath(manual); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("设置里指定的播放器不存在：%s", manual)
	}
	found, err := detectPlatform()
	if err != nil || len(found) == 0 {
		return "", nil
	}
	return found[0].Path, nil
}

// launchPlatform DETACHED 语义近似：新会话（setsid）拉起，stdio 忽略，不等退出。
func launchPlatform(command string, args []string) error {
	cmd := exec.Command(command, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return errors.New("拉起播放器失败：" + err.Error())
	}
	return cmd.Process.Release()
}
