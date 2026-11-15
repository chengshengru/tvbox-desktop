// Package player 外部播放器探测与拉起。
// 翻译自 src/main/services/player.ts。
//
// 桌面端不内置解码器，遇到浏览器播不了的格式（mkv/ts/HEVC）时，
// 把地址交给本机已安装的播放器是最省事也最稳的做法。
//
// Windows 实现（安装路径扫描 + 注册表文件关联）见 player_windows.go；
// 其余平台的桩实现（设置 command + PATH 查找）见 player_other.go。
//
// 作者：Shiro
package player

import (
	"errors"
	"os"
	"regexp"
	"strings"

	"tvbox-desktop/internal/settings"
	"tvbox-desktop/models"
)

// OpenResult 拉起结果，对应 TS 的 OpenPlayerResult。
type OpenResult struct {
	Player string   `json:"player"`
	Args   []string `json:"args"`
}

// defaultPrefer 默认偏好顺序。用户没在设置里写 prefer 时用它。
var defaultPrefer = []string{"PotPlayer", "mpv", "VLC", "MPC-HC", "MPC-BE", "KMPlayer", "SMPlayer"}

var protoRe = regexp.MustCompile(`(?i)^(https?|rtmp|rtsp|file):`)
var urlArgRe = regexp.MustCompile(`(?i)\{url\}`)
var titleArgRe = regexp.MustCompile(`(?i)\{title\}`)

// existsPath 路径是否存在（对应 node 的 existsSync）。
func existsPath(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// preferOrder 当前生效的偏好列表。
func preferOrder() []string {
	prefer := settings.Get().ExternalPlayer.Prefer
	if len(prefer) > 0 {
		return prefer
	}
	return defaultPrefer
}

// Detect 扫描本机可用的外部播放器（平台实现）。
func Detect() ([]models.PlayerInfo, error) {
	return detectPlatform()
}

// Open 调用外部播放器打开一个地址。
func Open(rawURL string, title string) (OpenResult, error) {
	u := strings.TrimSpace(rawURL)
	if !protoRe.MatchString(u) {
		return OpenResult{}, errors.New("播放地址不是受支持的协议")
	}
	command, err := resolvePlayerPlatform()
	if err != nil {
		return OpenResult{}, err
	}
	if command == "" {
		return OpenResult{}, errors.New("没有找到可用的外部播放器，请在「设置」里手动指定播放器路径")
	}

	template := settings.Get().ExternalPlayer.Args
	if len(template) == 0 {
		template = []string{"{url}"}
	}
	args := make([]string, 0, len(template))
	for _, arg := range template {
		// ReplaceAllLiteralString：地址/标题里可能带 $，不能当反向引用展开
		a := urlArgRe.ReplaceAllLiteralString(arg, u)
		a = titleArgRe.ReplaceAllLiteralString(a, title)
		args = append(args, a)
	}

	if err := launchPlatform(command, args); err != nil {
		return OpenResult{}, err
	}
	return OpenResult{Player: command, Args: args}, nil
}

// appendManual 设置里手填的那条：扫不到也照样列出来，不然界面上会出现
// 「一个都没找到」但上面明明配着一个能用的播放器这种别扭局面。
func appendManual(found []models.PlayerInfo, seen map[string]bool) []models.PlayerInfo {
	manual := strings.TrimSpace(settings.Get().ExternalPlayer.Command)
	if manual != "" && existsPath(manual) && !seen[manual] {
		found = append([]models.PlayerInfo{{Name: "设置里指定的", Path: manual, Source: "manual"}}, found...)
	}
	return found
}
