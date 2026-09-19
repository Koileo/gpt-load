package state

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"gpt-load/internal/outboundproxy"
)

// SettingTurnStateWatcher 是 Web 端可配置的轮次状态 watcher 设置键。它不进
// IsRuntimeSettingKey：与 proxy_config 一样由设置 API 单独放行，响应里也
// 不出现在逐键 overrides 列表中。持久化值为 nil（无该行）表示回退环境变量。
const SettingTurnStateWatcher = "turn_state_watcher"

const (
	DefaultTurnStateGroupID               = 1
	DefaultTurnStateCredentialID          = 1
	DefaultTurnStatePushMaxAgeMS          = int64(time.Hour / time.Millisecond)
	DefaultTurnStatePollIntervalSeconds   = int64(15)
	DefaultTurnStateVerifyIntervalSeconds = int64(60)
	DefaultTurnStateVerifyTimeoutSeconds  = int64(120)
	maxTurnStateLength                    = 100_000
	// maxTurnStateNumber 保持 JSON 安全整数范围；无类型常量以适配 int 参数。
	maxTurnStateNumber = 1<<53 - 1
)

// TurnStateWatcherConfig 是 watcher 的完整配置，字段与 .env 中的 TURN_STATE_*
// 变量一一对应（verify_model 不在此列：它必须与 push 模型一致，没有独立价值）。
type TurnStateWatcherConfig struct {
	Enabled               bool   `json:"enabled"`
	GroupID               uint   `json:"group_id"`
	CredentialID          uint   `json:"credential_id"`
	PushModels            string `json:"push_models"`
	PushMaxAgeMS          int64  `json:"push_max_age_ms"`
	HealthyLengths        []int  `json:"healthy_lengths"`
	DegradedLengths       []int  `json:"degraded_lengths"`
	PollIntervalSeconds   int64  `json:"poll_interval_seconds"`
	DegradeProxyMode      string `json:"degrade_proxy_mode,omitempty"`
	DegradeProxyURL       string `json:"degrade_proxy_url,omitempty"`
	VerifyIntervalSeconds int64  `json:"verify_interval_seconds"`
	VerifyTimeoutSeconds  int64  `json:"verify_timeout_seconds"`
	VerifyMaxAttempts     int    `json:"verify_max_attempts"`
	Verbose               bool   `json:"verbose"`
}

// Equal 判断两份配置在 watcher 行为层面是否一致；watcher 据此决定是否热重启。
func (config *TurnStateWatcherConfig) Equal(other *TurnStateWatcherConfig) bool {
	if config == nil || other == nil {
		return config == other
	}
	return config.Enabled == other.Enabled &&
		config.GroupID == other.GroupID &&
		config.CredentialID == other.CredentialID &&
		config.PushModels == other.PushModels &&
		config.PushMaxAgeMS == other.PushMaxAgeMS &&
		slices.Equal(config.HealthyLengths, other.HealthyLengths) &&
		slices.Equal(config.DegradedLengths, other.DegradedLengths) &&
		config.PollIntervalSeconds == other.PollIntervalSeconds &&
		config.DegradeProxyMode == other.DegradeProxyMode &&
		config.DegradeProxyURL == other.DegradeProxyURL &&
		config.VerifyIntervalSeconds == other.VerifyIntervalSeconds &&
		config.VerifyTimeoutSeconds == other.VerifyTimeoutSeconds &&
		config.VerifyMaxAttempts == other.VerifyMaxAttempts &&
		config.Verbose == other.Verbose
}

// ParseTurnStateWatcherConfig 校验并规范化设置 API 提交的 watcher 配置对象。
// 传 nil 返回 (nil, nil)，表示清除该设置、回退环境变量。解析成功的返回值即
// 规范化结果：默认值已填充、代理 URL 已规范化，可直接持久化与运行。
func ParseTurnStateWatcherConfig(value any) (*TurnStateWatcherConfig, error) {
	if value == nil {
		return nil, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", SettingTurnStateWatcher)
	}
	for key := range object {
		switch key {
		case "enabled", "group_id", "credential_id", "push_models", "push_max_age_ms",
			"healthy_lengths", "degraded_lengths", "poll_interval_seconds",
			"degrade_proxy_mode", "degrade_proxy_url", "verify_interval_seconds",
			"verify_timeout_seconds", "verify_max_attempts", "verbose":
		default:
			return nil, fmt.Errorf("unknown %s field %q", SettingTurnStateWatcher, key)
		}
	}

	config := &TurnStateWatcherConfig{
		GroupID:               DefaultTurnStateGroupID,
		CredentialID:          DefaultTurnStateCredentialID,
		PushMaxAgeMS:          DefaultTurnStatePushMaxAgeMS,
		HealthyLengths:        []int{292, 332},
		DegradedLengths:       []int{312, 356},
		PollIntervalSeconds:   DefaultTurnStatePollIntervalSeconds,
		DegradeProxyMode:      string(outboundproxy.ModeCustom),
		VerifyIntervalSeconds: DefaultTurnStateVerifyIntervalSeconds,
		VerifyTimeoutSeconds:  DefaultTurnStateVerifyTimeoutSeconds,
	}
	var err error
	if _, exists := object["enabled"]; exists {
		if config.Enabled, err = strictBoolean(pathAt("enabled"), object["enabled"]); err != nil {
			return nil, err
		}
	}
	if config.PushModels, err = parseTurnStateModel(
		pathAt("push_models"), object["push_models"], config.Enabled,
	); err != nil {
		return nil, err
	}
	if _, exists := object["group_id"]; exists {
		parsed, err := turnStatePositiveUint(pathAt("group_id"), object["group_id"])
		if err != nil {
			return nil, err
		}
		config.GroupID = parsed
	}
	if _, exists := object["credential_id"]; exists {
		parsed, err := turnStatePositiveUint(pathAt("credential_id"), object["credential_id"])
		if err != nil {
			return nil, err
		}
		config.CredentialID = parsed
	}
	if _, exists := object["push_max_age_ms"]; exists {
		parsed, err := turnStateDurationNumber(
			pathAt("push_max_age_ms"), object["push_max_age_ms"], time.Millisecond,
		)
		if err != nil {
			return nil, err
		}
		config.PushMaxAgeMS = int64(parsed)
	}
	if config.HealthyLengths, err = parseTurnStateLengths(
		pathAt("healthy_lengths"), object["healthy_lengths"], config.HealthyLengths,
	); err != nil {
		return nil, err
	}
	if config.DegradedLengths, err = parseTurnStateLengths(
		pathAt("degraded_lengths"), object["degraded_lengths"], config.DegradedLengths,
	); err != nil {
		return nil, err
	}
	for _, healthy := range config.HealthyLengths {
		for _, degraded := range config.DegradedLengths {
			if healthy == degraded {
				return nil, fmt.Errorf(
					"%s length %d cannot be both healthy and degraded",
					SettingTurnStateWatcher, healthy,
				)
			}
		}
	}
	if _, exists := object["poll_interval_seconds"]; exists {
		parsed, err := turnStateDurationNumber(
			pathAt("poll_interval_seconds"), object["poll_interval_seconds"], time.Second,
		)
		if err != nil {
			return nil, err
		}
		config.PollIntervalSeconds = int64(parsed)
	}
	if config.DegradeProxyURL, err = parseTurnStateDegradeProxy(
		object, config,
	); err != nil {
		return nil, err
	}
	if _, exists := object["verify_interval_seconds"]; exists {
		parsed, err := turnStateDurationNumber(
			pathAt("verify_interval_seconds"), object["verify_interval_seconds"], time.Second,
		)
		if err != nil {
			return nil, err
		}
		config.VerifyIntervalSeconds = int64(parsed)
	}
	if _, exists := object["verify_timeout_seconds"]; exists {
		parsed, err := turnStateDurationNumber(
			pathAt("verify_timeout_seconds"), object["verify_timeout_seconds"], time.Second,
		)
		if err != nil {
			return nil, err
		}
		config.VerifyTimeoutSeconds = int64(parsed)
	}
	if _, exists := object["verify_max_attempts"]; exists {
		parsed, err := wholeNumberInRange(
			pathAt("verify_max_attempts"), object["verify_max_attempts"], 0, maxTurnStateNumber,
		)
		if err != nil {
			return nil, err
		}
		config.VerifyMaxAttempts = parsed
	}
	if _, exists := object["verbose"]; exists {
		if config.Verbose, err = strictBoolean(pathAt("verbose"), object["verbose"]); err != nil {
			return nil, err
		}
	}
	return config, nil
}

func pathAt(field string) string {
	return SettingTurnStateWatcher + "." + field
}

// parseTurnStateModel 要求恰好一个不含逗号与通配符的模型名；关闭状态下允许
// 留空，这样 UI 可以先保存一份停用配置再补齐模型。
func parseTurnStateModel(path string, value any, enabled bool) (string, error) {
	if value == nil {
		if enabled {
			return "", fmt.Errorf("%s is required when enabled", path)
		}
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", path)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		if enabled {
			return "", fmt.Errorf("%s is required when enabled", path)
		}
		return "", nil
	}
	if strings.ContainsAny(text, ",*") {
		return "", fmt.Errorf("%s must name exactly one model without wildcards", path)
	}
	return text, nil
}

func turnStatePositiveUint(path string, value any) (uint, error) {
	parsed, err := wholeNumberInRange(path, value, 1, math.MaxInt32)
	if err != nil {
		return 0, err
	}
	return uint(parsed), nil
}

func turnStateDurationNumber(path string, value any, unit time.Duration) (int, error) {
	maximum := int(math.MaxInt64 / int64(unit))
	return wholeNumberInRange(path, value, 1, maximum)
}

func parseTurnStateLengths(path string, value any, fallback []int) ([]int, error) {
	if value == nil {
		return append([]int(nil), fallback...), nil
	}
	rawList, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of positive integers", path)
	}
	lengths := make([]int, 0, len(rawList))
	seen := make(map[int]struct{}, len(rawList))
	for index, raw := range rawList {
		parsed, err := wholeNumberInRange(
			fmt.Sprintf("%s[%d]", path, index), raw, 1, maxTurnStateLength,
		)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[parsed]; duplicate {
			return nil, fmt.Errorf("%s contains duplicate length %d", path, parsed)
		}
		seen[parsed] = struct{}{}
		lengths = append(lengths, parsed)
	}
	return lengths, nil
}

// parseTurnStateDegradeProxy 与环境变量语义一致：mode 只支持 custom；给了
// URL 才启用备用出口，mode 缺省补 custom；给了 mode 却没给 URL 视为配置错误。
func parseTurnStateDegradeProxy(object map[string]any, config *TurnStateWatcherConfig) (string, error) {
	mode := ""
	if raw, exists := object["degrade_proxy_mode"]; exists && raw != nil {
		text, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", pathAt("degrade_proxy_mode"))
		}
		mode = strings.TrimSpace(text)
	}
	url := ""
	if raw, exists := object["degrade_proxy_url"]; exists && raw != nil {
		text, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", pathAt("degrade_proxy_url"))
		}
		url = strings.TrimSpace(text)
	}
	if mode != "" && mode != string(outboundproxy.ModeCustom) {
		return "", fmt.Errorf(
			"%s must be %s",
			pathAt("degrade_proxy_mode"),
			outboundproxy.ModeCustom,
		)
	}
	if url == "" {
		if mode != "" {
			return "", fmt.Errorf(
				"%s requires %s",
				pathAt("degrade_proxy_mode"),
				pathAt("degrade_proxy_url"),
			)
		}
		config.DegradeProxyMode = ""
		return "", nil
	}
	normalized, err := outboundproxy.Normalize(outboundproxy.Config{
		Mode: outboundproxy.ModeCustom, URL: url,
	})
	if err != nil || normalized.Mode != outboundproxy.ModeCustom {
		return "", fmt.Errorf(
			"%s must be a valid http or socks5 proxy",
			pathAt("degrade_proxy_url"),
		)
	}
	config.DegradeProxyMode = string(normalized.Mode)
	return normalized.URL, nil
}
