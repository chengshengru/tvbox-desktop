//go:build windows

package player

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"

	"tvbox-desktop/internal/settings"
	"tvbox-desktop/models"
)

/* ------------------------------------------------------------------ *
 * 常见安装路径扫描
 * ------------------------------------------------------------------ */

// programFiles 环境变量里的安装根目录。
func programFiles() []string {
	out := []string{}
	for _, key := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "LOCALAPPDATA"} {
		if dir := os.Getenv(key); dir != "" {
			out = append(out, dir)
		}
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		out = append(out, filepath.Join(local, "Programs"))
	}
	return out
}

// pathDirs PATH 里能直接找到的目录（绿色版 mpv / VLC 常常是这么装的）。
func pathDirs() []string {
	out := []string{}
	for _, dir := range strings.Split(os.Getenv("PATH"), ";") {
		if dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

type candidate struct {
	name  string
	paths []string
}

func candidates() []candidate {
	roots := programFiles()
	under := func(parts ...string) []string {
		out := []string{}
		for _, root := range roots {
			out = append(out, filepath.Join(root, filepath.Join(parts...)))
		}
		return out
	}
	onPath := func(exe string) []string {
		out := []string{}
		for _, dir := range pathDirs() {
			out = append(out, filepath.Join(dir, exe))
		}
		return out
	}

	return []candidate{
		{
			name: "PotPlayer",
			paths: concat(
				under("DAUM", "PotPlayer", "PotPlayerMini64.exe"),
				under("DAUM", "PotPlayer", "PotPlayerMini.exe"),
				under("PotPlayer", "PotPlayerMini64.exe"),
				under("PotPlayer", "PotPlayerMini.exe"),
				onPath("PotPlayerMini64.exe"),
			),
		},
		{
			name: "mpv",
			paths: concat(
				under("mpv", "mpv.exe"),
				under("mpv-player", "mpv.exe"),
				under("mpv.net", "mpvnet.exe"),
				onPath("mpv.exe"),
			),
		},
		{
			name:  "VLC",
			paths: concat(under("VideoLAN", "VLC", "vlc.exe"), onPath("vlc.exe")),
		},
		{
			name: "MPC-HC",
			paths: concat(
				under("MPC-HC", "mpc-hc64.exe"),
				under("MPC-HC64", "mpc-hc64.exe"),
				under("K-Lite Codec Pack", "MPC-HC64", "mpc-hc64.exe"),
				onPath("mpc-hc64.exe"),
			),
		},
		{
			name:  "MPC-BE",
			paths: concat(under("MPC-BE x64", "mpc-be64.exe"), onPath("mpc-be64.exe")),
		},
		{
			name: "KMPlayer",
			paths: concat(
				under("The KMPlayer", "KMPlayer64.exe"),
				under("KMPlayer", "KMPlayer.exe"),
				onPath("KMPlayer64.exe"),
			),
		},
		{
			name:  "SMPlayer",
			paths: concat(under("SMPlayer", "smplayer.exe"), onPath("smplayer.exe")),
		},
	}
}

func concat(lists ...[]string) []string {
	out := []string{}
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

/* ------------------------------------------------------------------ *
 * 系统文件关联：绿色版 / 装在奇怪目录里的播放器扫不到，但用户多半把它
 * 设成了 .mkv / .mp4 的默认打开方式，问一下注册表就能知道。
 * ------------------------------------------------------------------ */

var assocExts = []string{".mkv", ".mp4", ".ts", ".flv"}

// notAPlayer 这些 ProgId 就算被设成默认也不是「播放器」，别往列表里塞。
var notAPlayer = regexp.MustCompile(`(?i)^(mse?edge|chrome|chromium|firefox|brave|opera|quark|vivaldi|360|sogou|qqbrowser|notepad|code|explorer|photos|mspaint|acdsee|wmp11\.assocfile)`)

var progIdSuffixRe = regexp.MustCompile(`(?i)\.[a-z0-9]+$`)
var exeSuffixRe = regexp.MustCompile(`(?i)\.exe$`)
var quotedRe = regexp.MustCompile(`^"([^"]+)"`)

// regStringValue 读一个注册表字符串值，读不到返回空串。
func regStringValue(root registry.Key, path string, name string) string {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// exeOf 命令行 `"C:\a\b.exe" "%1"` → `C:\a\b.exe`；解析不出来返回空串。
func exeOf(command string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return ""
	}
	raw := trimmed
	if m := quotedRe.FindStringSubmatch(trimmed); m != nil {
		raw = m[1]
	} else if fields := strings.Fields(trimmed); len(fields) > 0 {
		raw = fields[0]
	}
	if !exeSuffixRe.MatchString(raw) {
		return ""
	}
	return raw
}

func fromRegistry() []models.PlayerInfo {
	out := []models.PlayerInfo{}
	seen := map[string]bool{}

	for _, ext := range assocExts {
		// 用户改过默认就用 UserChoice，没改过就退回 .ext 的默认值
		progID := regStringValue(registry.CURRENT_USER,
			`Software\Microsoft\Windows\CurrentVersion\Explorer\FileExts\`+ext+`\UserChoice`, "ProgId")
		if progID == "" {
			progID = regStringValue(registry.CURRENT_USER, `Software\Classes\`+ext, "")
			if progID == "" {
				progID = regStringValue(registry.LOCAL_MACHINE, `Software\Classes\`+ext, "")
			}
		}
		if progID == "" || notAPlayer.MatchString(progID) {
			continue
		}

		command := regStringValue(registry.CURRENT_USER, `Software\Classes\`+progID+`\shell\open\command`, "")
		if command == "" {
			command = regStringValue(registry.LOCAL_MACHINE, `Software\Classes\`+progID+`\shell\open\command`, "")
		}
		exe := exeOf(command)
		if exe == "" || seen[strings.ToLower(exe)] || !existsPath(exe) {
			continue
		}
		seen[strings.ToLower(exe)] = true

		// ProgId 形如 `PotPlayer64.mkv`，去掉扩展名后缀当显示名
		name := progIdSuffixRe.ReplaceAllString(progID, "")
		if rs := []rune(name); len(rs) > 40 {
			name = string(rs[:40])
		}
		if name == "" {
			name = "系统默认"
		}
		out = append(out, models.PlayerInfo{Name: name, Path: exe, Source: "registry"})
	}
	return out
}

/* ------------------------------------------------------------------ *
 * 平台入口
 * ------------------------------------------------------------------ */

// detectPlatform 扫描本机常见安装路径，按用户设置的偏好排序；
// 认识的播放器排前面，系统关联里翻出来的排后面。注册表查不动就算了，扫描的结果照样能用。
func detectPlatform() ([]models.PlayerInfo, error) {
	prefer := preferOrder()
	found := []models.PlayerInfo{}
	seen := map[string]bool{}

	for _, candidate := range candidates() {
		for _, path := range candidate.paths {
			if seen[path] || !existsPath(path) {
				continue
			}
			seen[path] = true
			found = append(found, models.PlayerInfo{Name: candidate.name, Path: path, Source: "scan"})
			break
		}
	}

	rank := func(info models.PlayerInfo) int {
		for i, name := range prefer {
			if name == info.Name {
				return i
			}
		}
		return len(prefer)
	}
	sort.SliceStable(found, func(i, j int) bool { return rank(found[i]) < rank(found[j]) })

	for _, info := range fromRegistry() {
		if seen[info.Path] {
			continue
		}
		seen[info.Path] = true
		found = append(found, info)
	}

	found = appendManual(found, seen)
	return found, nil
}

// resolvePlayerPlatform 用户手动指定的播放器优先，其次按偏好列表在本机找一个。
func resolvePlayerPlatform() (string, error) {
	manual := strings.TrimSpace(settings.Get().ExternalPlayer.Command)
	if manual != "" {
		if existsPath(manual) {
			return manual, nil
		}
		return "", fmt.Errorf("设置里指定的播放器不存在：%s", manual)
	}
	found, err := detectPlatform()
	if err != nil || len(found) == 0 {
		return "", nil
	}
	return found[0].Path, nil
}

// launchPlatform DETACHED 语义：新进程组 + 脱离控制台（对应 node spawn 的 detached:true + unref），
// stdio 全部忽略，播放器随起随走，不等它退出。
func launchPlatform(command string, args []string) error {
	cmd := exec.Command(command, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
	if err := cmd.Start(); err != nil {
		return errors.New("拉起播放器失败：" + err.Error())
	}
	// 不 Wait 也不持有句柄，等价 child.unref()
	return cmd.Process.Release()
}
