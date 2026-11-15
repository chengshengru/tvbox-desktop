// Package ai 用 DeepSeek 给「当前这一集」写一段剧情总结。翻译自 src/main/services/ai.ts。
//
// 两条路：
//
//  1. Summarize()        只看文本资料。站点侧只给得到整部剧的资料
//     （片名 / 类型 / 演员 / 简介），没有分集字幕，
//     所以模型只能在自己已有的剧集知识上推。不靠谱。
//  2. SummarizeVisual()  渲染进程从正在播的 video 上按 20 秒一帧抽出画面
//     （utils/frames.ts），这里把它们按时间顺序交给
//     deepseek-flash —— 它是多模态的，能真的「看」。
//     提示词里写死了这次没有声音，不许编对白。
//
// 想换成别的 OpenAI 兼容服务，改设置页里的 baseUrl 和 model 就行。
// 注意视觉只有 deepseek-flash 支持，deepseek-v4-pro 会拒图。
//
// 作者：Shiro
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/models"
)

const (
	officialBase = "https://api.deepseek.com"
	defaultModel = "deepseek-flash"
	// 纯文本那条路很快
	timeoutText = 90 * time.Second
	// 视觉那条路要传几十张图，基线给宽一点，再按帧数往上加（上限见下）
	timeoutVisual    = 120 * time.Second
	timeoutVisualMax = 300 * time.Second
	// maxFrames 单次请求最多塞多少帧，防止一次性把 600 的接口上限撞穿
	maxFrames = 140
)

// systemText 纯文本总结的两段 SYSTEM 提示词（全文照抄 TS 版）。
var systemText = strings.Join([]string{
	"你是一个中文影视剧剧情助手。用户会给你一部剧（或番剧、综艺、动漫）的资料，以及他当前正在看的集数。",
	"请针对「这一集」输出内容，不要复述整部剧的简介。",
	"",
	"输出格式（纯文本，不要用 Markdown 标题符号）：",
	"第一段：用 2 到 4 句话说清这一集讲了什么。",
	"然后另起一行写「本集看点」，下面用「- 」列 3 到 5 条。",
	"",
	"硬性要求：",
	"1. 如果资料不足以确定这一集的具体情节，就在第一段里如实说明「仅凭现有资料无法确定这一集的具体情节」，然后只写你能推断的内容，绝对不要编造具体的人物、台词或事件。",
	"2. 不要写「好的」「以下是」这类客套话。",
	"3. 不要重复用户给你的原始资料。",
	"4. 全文控制在 400 字以内，用简体中文。",
}, "\n")

// systemVisual 多模态帧总结的 SYSTEM 提示词（全文照抄 TS 版）。
var systemVisual = strings.Join([]string{
	"你是一个中文影视剧剧情解说。用户会给你一部剧集的资料，以及**按时间顺序从这一集里抽出来的画面**——每张图前面标着它在片中的时间点。",
	"你要根据这些画面写出这一集实际发生了什么，而不是根据剧名去猜。",
	"",
	"输出格式（纯文本，不要用 Markdown 标题符号）：",
	"第一段：用 3 到 6 句话说清这一集的剧情走向，按时间顺序。",
	"然后另起一行写「剧情节点」，下面用「- [时间] 内容」的形式列 4 到 8 条关键节点。把相邻的画面合并成一条，不要一帧一条。",
	"然后另起一行写「本集看点」，下面用「- 」列 2 到 4 条。",
	"",
	"硬性要求：",
	"1. 这些画面是每 20 秒抽一帧得到的，而且**没有声音**。所以：画面上出现字幕或文字时可以照实引用；但**不要编造对白、人物姓名，或画面上根本看不到的情节**；只描述你确实从画面里看到的东西，拿不准的地方就明说看不出来。",
	"2. 如果画面几乎全是片头、黑屏、静态图或者内容高度重复，就在第一段里直接说明「这一集抽到的画面信息量很少」，不要硬编。",
	"3. 不要写「好的」「以下是」这类客套话，不要重复用户给你的资料。",
	"4. 全文控制在 600 字以内，用简体中文。",
}, "\n")

// nonEmpty 「标签：值」，值为空返回空串。
func nonEmpty(label string, value string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return ""
	}
	return label + "：" + text
}

// buildUser 把影片资料拼成给模型的 user 文本。
func buildUser(input models.AiSummaryInput) string {
	meta := []string{}
	for _, part := range []string{
		nonEmpty("片名", input.VodName),
		nonEmpty("类型", input.TypeName),
		nonEmpty("年份", input.Year),
		nonEmpty("地区", input.Area),
		nonEmpty("导演", input.Director),
		nonEmpty("演员", input.Actor),
		nonEmpty("评分", input.Score),
		nonEmpty("备注", input.Remarks),
	} {
		if part != "" {
			meta = append(meta, part)
		}
	}

	total := "?"
	if input.EpTotal > 0 {
		total = strconv.Itoa(input.EpTotal)
	}
	line := ""
	if input.LineName != "" {
		line = fmt.Sprintf("，线路「%s」", input.LineName)
	}
	episode := fmt.Sprintf("当前正在看：%s（第 %d 集 / 共 %s 集%s）", input.EpName, input.EpIndex+1, total, line)

	intro := strings.TrimSpace(input.Content)
	introPart := ""
	if intro != "" {
		if rs := []rune(intro); len(rs) > 1200 {
			intro = string(rs[:1200])
		}
		introPart = fmt.Sprintf("\n【整部剧的简介】\n%s", intro)
	}

	parts := []string{}
	for _, part := range []string{"【影片资料】", strings.Join(meta, "\n"), "", episode, introPart} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n")
}

// stamp 把秒数写成 mm:ss，给模型当时间锚点。
func stamp(seconds float64) string {
	s := math.Max(0, math.Round(seconds))
	m := math.Floor(s / 60)
	return fmt.Sprintf("%02d:%02d", int(m), int(s)%60)
}

// contentBlock OpenAI 兼容的多模态内容块。
type contentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// buildVisualUser 资料文本 + 按时间顺序的画面帧，拼成多模态 user 内容。
func buildVisualUser(input models.AiSummaryInput, frames []models.AiFrame) []contentBlock {
	blocks := []contentBlock{
		{Type: "text", Text: buildUser(input)},
		{Type: "text", Text: fmt.Sprintf("\n【按时间顺序抽取的画面】共 %d 帧，每帧前标着它在片中的时间点。", len(frames))},
	}
	for _, frame := range frames {
		blocks = append(blocks, contentBlock{Type: "text", Text: fmt.Sprintf("[%s]", stamp(frame.T))})
		blocks = append(blocks, contentBlock{
			Type: "image_url",
			// detail:'low' 会先缩到 512×512 再推理，省钱也更快；这些图本来就只有 512 宽
			ImageURL: &imageURL{URL: "data:image/jpeg;base64," + frame.B64, Detail: "low"},
		})
	}
	return blocks
}

// apiResponse DeepSeek（OpenAI 兼容）的响应结构。
type apiResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// explainStatus 把 HTTP 状态码翻译成人能看懂的话。
func explainStatus(status int, detail string) string {
	switch {
	case status == 401:
		return "DeepSeek 说这个 API Key 不对（401）。去「设置 → AI 总结」里检查一下。"
	case status == 402:
		return "DeepSeek 账户余额不足（402），去 platform.deepseek.com 充值后再试。"
	case status == 404:
		return fmt.Sprintf("模型名可能不对（404）。%s", detail)
	case status == 413:
		return "这次传的画面太多了，接口拒收（413）。把「抽帧间隔」调大一点再试。"
	case status == 429:
		return "请求太频繁被限流了（429），等一会儿再试。"
	case status >= 500:
		return fmt.Sprintf("DeepSeek 服务端出错（HTTP %d），稍后再试。", status)
	default:
		return fmt.Sprintf("DeepSeek 返回了 HTTP %d。%s", status, detail)
	}
}

type aiConfig struct {
	apiKey string
	model  string
	base   string
	url    string
}

var trailingSlashRe = regexp.MustCompile(`/+$`)

// loadConfig 读取设置里的 DeepSeek 配置。
func loadConfig() (aiConfig, error) {
	st := settings.Get()
	apiKey := strings.TrimSpace(st.AI.APIKey)
	if apiKey == "" {
		return aiConfig{}, errors.New("还没有配置 DeepSeek API Key。去「设置 → AI 总结」里填一下就能用了。")
	}
	model := strings.TrimSpace(st.AI.Model)
	if model == "" {
		model = defaultModel
	}
	base := strings.TrimSpace(st.AI.BaseURL)
	if base == "" {
		base = officialBase
	}
	base = trailingSlashRe.ReplaceAllString(base, "")
	return aiConfig{apiKey: apiKey, model: model, base: base, url: base + "/chat/completions"}, nil
}

// callDeepSeek 发一次 OpenAI 兼容请求。user 是 string 或 []contentBlock。
func callDeepSeek(ctx context.Context, cfg aiConfig, system string, user any, maxTokens int, timeout time.Duration) (models.AiSummaryResult, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()

	body, err := json.Marshal(map[string]any{
		"model": cfg.model,
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		// 写摘要不需要思维链：关掉它，快很多也便宜很多
		"thinking":   map[string]any{"type": "disabled"},
		"max_tokens": maxTokens,
		"stream":     false,
	})
	if err != nil {
		return models.AiSummaryResult{}, err
	}

	res, err := httputil.Request(attemptCtx, cfg.url, httputil.Options{
		Method:  "POST",
		Timeout: timeout.Seconds(),
		Header: map[string]string{
			"content-type":  "application/json",
			"authorization": "Bearer " + cfg.apiKey,
		},
		Body: string(body),
		// 非 2xx 也要拿到响应体，按状态码翻译成中文文案
		AllowHTTPError: true,
	})
	if err != nil {
		if attemptCtx.Err() != nil {
			return models.AiSummaryResult{}, fmt.Errorf("等 DeepSeek 回复超过 %d 秒，先不等了。可以再点一次重试。", int(timeout/time.Second))
		}
		return models.AiSummaryResult{}, fmt.Errorf("连不上 %s：%s。检查网络，或者系统代理是不是开着但没跑起来。", cfg.base, err)
	}

	raw := string(res.Body)
	var parsed apiResponse
	// 解析失败就保持空结构，下面按状态码报错
	_ = json.Unmarshal(res.Body, &parsed)

	if !res.OK {
		detail := ""
		if parsed.Error != nil {
			detail = parsed.Error.Message
		} else {
			if rs := []rune(raw); len(rs) > 200 {
				raw = string(rs[:200])
			}
			detail = raw
		}
		return models.AiSummaryResult{}, errors.New(explainStatus(res.Status, strings.TrimSpace(detail)))
	}

	text := ""
	if len(parsed.Choices) > 0 {
		text = strings.TrimSpace(parsed.Choices[0].Message.Content)
	}
	if text == "" {
		return models.AiSummaryResult{}, errors.New("DeepSeek 这次没返回任何内容，再点一次试试。")
	}

	result := models.AiSummaryResult{
		Text:  text,
		Model: cfg.model,
		Ms:    time.Since(started).Milliseconds(),
	}
	if parsed.Usage != nil {
		result.Usage = &models.AiUsage{
			Prompt:     parsed.Usage.PromptTokens,
			Completion: parsed.Usage.CompletionTokens,
			Total:      parsed.Usage.TotalTokens,
		}
	}
	return result, nil
}

// Summarize 只看站点资料，让模型依据自己的剧集知识写。看不清片的时候用这条。
func Summarize(ctx context.Context, input models.AiSummaryInput) (models.AiSummaryResult, error) {
	cfg, err := loadConfig()
	if err != nil {
		return models.AiSummaryResult{}, err
	}
	return callDeepSeek(ctx, cfg, systemText, buildUser(input), 900, timeoutText)
}

// SummarizeVisual 抽了画面，让模型真的看一遍。frames 为空时退回文本那条路。
func SummarizeVisual(ctx context.Context, input models.AiSummaryInput, frames []models.AiFrame) (models.AiSummaryResult, error) {
	usable := []models.AiFrame{}
	for _, f := range frames {
		if len(f.B64) > 64 {
			usable = append(usable, f)
		}
	}
	if len(usable) > maxFrames {
		usable = usable[:maxFrames]
	}
	if len(usable) == 0 {
		return Summarize(ctx, input)
	}

	cfg, err := loadConfig()
	if err != nil {
		return models.AiSummaryResult{}, err
	}
	// 帧多了以后上传 + 推理都慢，按帧数把超时放宽
	timeout := timeoutVisual + time.Duration(len(usable))*time.Second
	if timeout > timeoutVisualMax {
		timeout = timeoutVisualMax
	}
	return callDeepSeek(ctx, cfg, systemVisual, buildVisualUser(input, usable), 1600, timeout)
}
