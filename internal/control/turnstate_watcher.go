package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"gpt-load/internal/execution"
	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/platform/utils"
	"gpt-load/internal/protocol"
	"gpt-load/internal/turnstate"
)

// ---------------------------------------------------------------------------
// 配置：全部来自环境变量，默认关闭。开启后 watcher 与外部脚本版行为一致：
// 观测到新鲜的健康 state 就写入目标凭据；观测到已知异常形态就清空注入值并把
// 凭据代理切到备用出口，然后用最小请求验证，恢复健康后切回直连。
// ---------------------------------------------------------------------------

type turnStateWatcherSettings struct {
	Enabled        bool
	WatchModels    map[string]struct{}
	PushModels     string
	GroupID        uint
	CredentialID   uint
	PushMaxAge     time.Duration
	HealthyLength  map[int]struct{}
	DegradedLength map[int]struct{}
	PollInterval   time.Duration
	DegradeProxy   outboundproxy.Config
	DegradeReady   bool // DegradeProxy.URL 非空时降级路径才启用
	DirectProxy    outboundproxy.Config
	VerifyModel    string
	VerifyGap      time.Duration // 两次验证请求之间的间隔
	VerifyTime     time.Duration // 单次验证请求的预算
	VerifyMax      int           // 0 = 不限次数
	Verbose        bool
}

const (
	defaultTurnStateGroupID      = 1
	defaultTurnStateCredentialID = 1
	defaultTurnStatePushAge      = time.Hour
	defaultTurnStatePoll         = 15 * time.Second
	defaultTurnStateVerifyGap    = time.Minute
	defaultTurnStateVerifyBudget = 120 * time.Second
	turnStateObservationBuffer   = 256
)

// errTurnStateWatcherDisabled 表示环境里没有开启 watcher，调用方应静默返回。
var errTurnStateWatcherDisabled = errors.New("turn state watcher disabled")

// loadTurnStateWatcherSettings 读取环境变量并做默认值与校验。getenv 注入是为了测试。
func loadTurnStateWatcherSettings(getenv func(string) string) (turnStateWatcherSettings, error) {
	settings := turnStateWatcherSettings{
		WatchModels:    map[string]struct{}{},
		HealthyLength:  map[int]struct{}{},
		DegradedLength: map[int]struct{}{},
		GroupID:        defaultTurnStateGroupID,
		CredentialID:   defaultTurnStateCredentialID,
		PushMaxAge:     defaultTurnStatePushAge,
		PollInterval:   defaultTurnStatePoll,
		VerifyGap:      defaultTurnStateVerifyGap,
		VerifyTime:     defaultTurnStateVerifyBudget,
	}
	enabled, err := envBoolValue("TURN_STATE_WATCHER", getenv("TURN_STATE_WATCHER"), false)
	if err != nil {
		return settings, err
	}
	if !enabled {
		return settings, errTurnStateWatcherDisabled
	}
	settings.Enabled = true

	settings.PushModels, err = boundTurnStateModel(
		"TURN_STATE_PUSH_MODELS", getenv("TURN_STATE_PUSH_MODELS"),
	)
	if err != nil {
		return settings, err
	}
	settings.WatchModels[strings.ToLower(settings.PushModels)] = struct{}{}
	if watchModel := strings.TrimSpace(getenv("TURN_STATE_MODELS")); watchModel != "" {
		watchModel, err = boundTurnStateModel("TURN_STATE_MODELS", watchModel)
		if err != nil {
			return settings, err
		}
		if !strings.EqualFold(watchModel, settings.PushModels) {
			return settings, fmt.Errorf("TURN_STATE_MODELS must match TURN_STATE_PUSH_MODELS")
		}
	}

	if settings.GroupID, err = envPositiveUint(
		"TURN_STATE_PUSH_GROUP_ID", getenv("TURN_STATE_PUSH_GROUP_ID"), settings.GroupID,
	); err != nil {
		return settings, err
	}
	if settings.CredentialID, err = envPositiveUint(
		"TURN_STATE_PUSH_CREDENTIAL_ID", getenv("TURN_STATE_PUSH_CREDENTIAL_ID"), settings.CredentialID,
	); err != nil {
		return settings, err
	}
	if sourceCredential := strings.TrimSpace(getenv("TURN_STATE_CREDENTIAL_ID")); sourceCredential != "" {
		var sourceID uint
		if sourceID, err = envPositiveUint(
			"TURN_STATE_CREDENTIAL_ID", sourceCredential, settings.CredentialID,
		); err != nil {
			return settings, err
		}
		if sourceID != settings.CredentialID {
			return settings, fmt.Errorf("TURN_STATE_CREDENTIAL_ID must match TURN_STATE_PUSH_CREDENTIAL_ID")
		}
	}
	if settings.PushMaxAge, err = envPositiveDuration(
		"TURN_STATE_PUSH_MAX_AGE_MS", getenv("TURN_STATE_PUSH_MAX_AGE_MS"), "ms", settings.PushMaxAge,
	); err != nil {
		return settings, err
	}
	if settings.HealthyLength, err = envLengthSet(
		"TURN_STATE_HEALTHY_LENGTHS", getenv("TURN_STATE_HEALTHY_LENGTHS"), 292, 332,
	); err != nil {
		return settings, err
	}
	if settings.DegradedLength, err = envLengthSet(
		"TURN_STATE_DEGRADED_LENGTHS", getenv("TURN_STATE_DEGRADED_LENGTHS"), 312, 356,
	); err != nil {
		return settings, err
	}
	for length := range settings.HealthyLength {
		if _, exists := settings.DegradedLength[length]; exists {
			return settings, fmt.Errorf("turn-state length %d cannot be both healthy and degraded", length)
		}
	}
	if settings.PollInterval, err = envPositiveDuration(
		"TURN_STATE_POLL_INTERVAL", getenv("TURN_STATE_POLL_INTERVAL"), "s", settings.PollInterval,
	); err != nil {
		return settings, err
	}

	degradeMode, err := aliasedEnv(
		"TURN_STATE_DEGRADE_PROXY_MODE", getenv("TURN_STATE_DEGRADE_PROXY_MODE"),
		"TURN_STATE_312_PROXY_MODE", getenv("TURN_STATE_312_PROXY_MODE"),
	)
	if err != nil {
		return settings, err
	}
	degradeURL, err := aliasedEnv(
		"TURN_STATE_DEGRADE_PROXY_URL", getenv("TURN_STATE_DEGRADE_PROXY_URL"),
		"TURN_STATE_312_PROXY_URL", getenv("TURN_STATE_312_PROXY_URL"),
	)
	if err != nil {
		return settings, err
	}
	settings.DegradeProxy = outboundproxy.Config{
		Mode: outboundproxy.Mode(degradeMode),
		URL:  degradeURL,
	}
	if settings.DegradeProxy.Mode == "" {
		settings.DegradeProxy.Mode = outboundproxy.ModeCustom
	}
	if settings.DegradeProxy.URL != "" {
		settings.DegradeProxy, err = outboundproxy.Normalize(settings.DegradeProxy)
		if err != nil || settings.DegradeProxy.Mode != outboundproxy.ModeCustom {
			return settings, fmt.Errorf("TURN_STATE_DEGRADE_PROXY_URL must be a valid http or socks5 proxy")
		}
		settings.DegradeReady = true
	} else if degradeMode != "" {
		return settings, fmt.Errorf("TURN_STATE_DEGRADE_PROXY_MODE requires TURN_STATE_DEGRADE_PROXY_URL")
	}
	settings.DirectProxy = outboundproxy.Config{
		Mode: outboundproxy.Mode(getenv("TURN_STATE_DIRECT_PROXY_MODE")),
	}
	if settings.DirectProxy.Mode == "" {
		settings.DirectProxy = outboundproxy.Config{Mode: outboundproxy.ModeDirect}
	}
	settings.DirectProxy, err = outboundproxy.Normalize(settings.DirectProxy)
	if err != nil || settings.DirectProxy.Mode != outboundproxy.ModeDirect {
		return settings, fmt.Errorf("TURN_STATE_DIRECT_PROXY_MODE must be direct")
	}

	settings.VerifyModel = strings.TrimSpace(getenv("TURN_STATE_VERIFY_MODEL"))
	if settings.VerifyModel == "" {
		settings.VerifyModel = strings.ToLower(settings.PushModels)
	} else {
		settings.VerifyModel, err = boundTurnStateModel("TURN_STATE_VERIFY_MODEL", settings.VerifyModel)
		if err != nil {
			return settings, err
		}
		if !strings.EqualFold(settings.VerifyModel, settings.PushModels) {
			return settings, fmt.Errorf("TURN_STATE_VERIFY_MODEL must match TURN_STATE_PUSH_MODELS")
		}
	}
	if settings.VerifyGap, err = envPositiveDuration(
		"TURN_STATE_VERIFY_INTERVAL", getenv("TURN_STATE_VERIFY_INTERVAL"), "s", settings.VerifyGap,
	); err != nil {
		return settings, err
	}
	if settings.VerifyTime, err = envPositiveDuration(
		"TURN_STATE_VERIFY_TIMEOUT", getenv("TURN_STATE_VERIFY_TIMEOUT"), "s", settings.VerifyTime,
	); err != nil {
		return settings, err
	}
	if settings.VerifyMax, err = envNonNegativeInt(
		"TURN_STATE_VERIFY_MAX_ATTEMPTS", getenv("TURN_STATE_VERIFY_MAX_ATTEMPTS"), 0,
	); err != nil {
		return settings, err
	}
	if settings.Verbose, err = envBoolValue(
		"TURN_STATE_VERBOSE", getenv("TURN_STATE_VERBOSE"), false,
	); err != nil {
		return settings, err
	}
	return settings, nil
}

func boundTurnStateModel(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s must name exactly one model", name)
	}
	if strings.ContainsAny(value, ",*") {
		return "", fmt.Errorf("%s must name exactly one model without wildcards", name)
	}
	return value, nil
}

func envBoolValue(name, value string, fallback bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return fallback, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be a boolean", name)
	}
}

func envPositiveUint(name, value string, fallback uint) (uint, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 || uint64(uint(parsed)) != parsed {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return uint(parsed), nil
}

func envPositiveDuration(name, value, unit string, fallback time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value + unit)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive number of %s", name, unit)
	}
	return parsed, nil
}

func envNonNegativeInt(name, value string, fallback int) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}

func envLengthSet(name, value string, defaults ...int) (map[int]struct{}, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		result := make(map[int]struct{}, len(defaults))
		for _, length := range defaults {
			result[length] = struct{}{}
		}
		return result, nil
	}
	result := map[int]struct{}{}
	for _, entry := range strings.Split(value, ",") {
		length, err := strconv.Atoi(strings.TrimSpace(entry))
		if err != nil || length <= 0 {
			return nil, fmt.Errorf("%s must be a comma-separated list of positive integers", name)
		}
		result[length] = struct{}{}
	}
	return result, nil
}

func aliasedEnv(primaryName, primaryValue, legacyName, legacyValue string) (string, error) {
	primaryValue = strings.TrimSpace(primaryValue)
	legacyValue = strings.TrimSpace(legacyValue)
	if primaryValue != "" && legacyValue != "" && primaryValue != legacyValue {
		return "", fmt.Errorf("%s and %s conflict", primaryName, legacyName)
	}
	if primaryValue != "" {
		return primaryValue, nil
	}
	return legacyValue, nil
}

// ---------------------------------------------------------------------------
// watcher 主体
// ---------------------------------------------------------------------------

// turnStateWatcher 持有 watcher 的进程内状态。注入值与代理的真实状态以注册表和
// 数据库为准，这里只记"我们的动作做到哪了"，重启后通过读取凭据当前代理恢复。
type turnStateWatcher struct {
	service      *Service
	settings     turnStateWatcherSettings
	observations chan turnstate.Observation
	now          func() time.Time

	mu              sync.Mutex
	desired         turnStateCandidate // 最新观测到、还没确认写入凭据的健康 state
	degraded        bool               // 已进入降级（备用代理生效中）
	degradeProxySet bool               // 备用代理已确认写入
	verifyPaused    bool               // 达到尝试上限后暂停，等下一次非健康观测再恢复
	verifyAttempts  int
	nextVerifyAt    time.Time
}

type turnStateCandidate struct {
	value      string
	observedAt time.Time
}

// RunTurnStateWatcher 在控制面后台启动 turn-state watcher；未在环境中开启时
// 立即返回，不产生任何可见行为。
func (s *Service) RunTurnStateWatcher(ctx context.Context) {
	if s == nil || s.db == nil || ctx == nil {
		return
	}
	settings, err := loadTurnStateWatcherSettings(os.Getenv)
	if err != nil {
		if !errors.Is(err, errTurnStateWatcherDisabled) {
			turnStateLog(logrus.WarnLevel, logrus.Fields{
				"event": "control.turn_state_watcher_config_invalid",
			}, "Turn state watcher configuration is invalid: "+err.Error())
		}
		return
	}
	watcher := &turnStateWatcher{
		service:      s,
		settings:     settings,
		observations: make(chan turnstate.Observation, turnStateObservationBuffer),
		now:          time.Now,
	}
	restore := turnstate.Subscribe(watcher.forward)
	defer restore()
	turnStateLog(logrus.InfoLevel, logrus.Fields{
		"event":         "control.turn_state_watcher_started",
		"group_id":      settings.GroupID,
		"credential_id": settings.CredentialID,
	}, fmt.Sprintf(
		"Turn state watcher started (model %s · credential %d · healthy lengths %s · degraded lengths %s · poll %s)",
		settings.PushModels,
		settings.CredentialID,
		joinHealthyLengths(settings),
		joinTurnStateLengths(settings.DegradedLength),
		settings.PollInterval,
	))
	watcher.restoreState(ctx)
	ticker := time.NewTicker(settings.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case observation := <-watcher.observations:
			watcher.observe(ctx, observation)
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			watcher.sweep(ctx)
		}
	}
}

// restoreState 在启动时判断凭据是否还停在降级配置上，是则继续验证流程，
// 与外部脚本重启后从状态文件恢复 pending_verify 等价。
func (watcher *turnStateWatcher) restoreState(ctx context.Context) {
	if !watcher.settings.DegradeReady {
		return
	}
	target, err := watcher.service.buildDegradationTarget(
		ctx, watcher.settings.GroupID, watcher.settings.CredentialID,
	)
	if err != nil {
		if ctx.Err() == nil {
			turnStateLog(logrus.WarnLevel, logrus.Fields{
				"event":         "control.turn_state_watcher_restore_failed",
				"credential_id": watcher.settings.CredentialID,
			}, "Turn state watcher could not inspect the credential at startup: "+err.Error())
		}
		return
	}
	if !watcher.degradeProxyActive(target.credential.proxy) {
		return
	}
	watcher.mu.Lock()
	watcher.degraded = true
	// 启动前可能只切了代理却没清掉旧 state，下一次 sweep 用一次原子更新
	// 同时确认这两个条件。
	watcher.degradeProxySet = false
	watcher.nextVerifyAt = watcher.now()
	watcher.mu.Unlock()
	turnStateLog(logrus.InfoLevel, logrus.Fields{
		"event":         "control.turn_state_watcher_resumed",
		"credential_id": watcher.settings.CredentialID,
	}, "credential is still on the degrade proxy; resuming verification")
}

// forward 是观测回调：运行在请求热路径上，必须绝不阻塞。缓冲写满时丢最旧的。
func (watcher *turnStateWatcher) forward(observation turnstate.Observation) {
	select {
	case watcher.observations <- observation:
		return
	default:
	}
	select {
	case <-watcher.observations:
	default:
	}
	select {
	case watcher.observations <- observation:
	default:
	}
}

// observe 处理一条观测：健康值进入推送候选，非健康值触发降级。
func (watcher *turnStateWatcher) observe(ctx context.Context, observation turnstate.Observation) {
	if observation.CredentialID != watcher.settings.CredentialID ||
		observation.StatusCode < http.StatusOK || observation.StatusCode >= http.StatusMultipleChoices {
		return
	}
	if !turnStateModelWatched(watcher.settings.WatchModels, observation.ClientModel, observation.UpstreamModel) {
		return
	}
	healthy := turnStateLengthHealthy(observation.TurnState, watcher.settings.HealthyLength)
	_, degraded := watcher.settings.DegradedLength[len(observation.TurnState)]
	if watcher.settings.Verbose {
		turnStateLog(logrus.DebugLevel, logrus.Fields{
			"event":         "control.turn_state_observed",
			"credential_id": observation.CredentialID,
			"length":        len(observation.TurnState),
			"healthy":       healthy,
		}, fmt.Sprintf("observed turn state of %d chars (healthy=%v)", len(observation.TurnState), healthy))
	}
	if healthy {
		if !turnStateValueFresh(
			observation.TurnState, observation.ObservedAt, watcher.now(), watcher.settings.PushMaxAge,
		) {
			return
		}
		watcher.mu.Lock()
		watcher.desired = turnStateCandidate{
			value: observation.TurnState, observedAt: observation.ObservedAt,
		}
		watcher.mu.Unlock()
		return
	}
	// 未知长度不自动改凭据或代理。上游格式变化时宁可只记日志，也不能把所有
	// 正常流量误判成降级；已知异常形态可由 TURN_STATE_DEGRADED_LENGTHS 扩展。
	if !degraded || !watcher.settings.DegradeReady {
		return
	}
	watcher.enterDegraded(ctx)
}

// sweep 是每个轮询节拍的一次巡检：把待写的注入值推下去；处于降级时按节奏验证。
func (watcher *turnStateWatcher) sweep(ctx context.Context) {
	watcher.flushPush(ctx)
	watcher.mu.Lock()
	degraded := watcher.degraded
	proxySet := watcher.degradeProxySet
	verifyPaused := watcher.verifyPaused
	nextVerifyAt := watcher.nextVerifyAt
	watcher.mu.Unlock()
	if !watcher.settings.DegradeReady || !degraded || verifyPaused {
		return
	}
	if !proxySet {
		watcher.ensureDegraded(ctx)
		watcher.scheduleNextVerify()
		return
	}
	if watcher.now().Before(nextVerifyAt) {
		return
	}
	watcher.verifyOnce(ctx)
}

// flushPush 把最新健康 state 写入目标凭据；与凭据当前值相同就跳过。
func (watcher *turnStateWatcher) flushPush(ctx context.Context) {
	watcher.mu.Lock()
	desired := watcher.desired
	degraded := watcher.degraded
	watcher.mu.Unlock()
	if desired.value == "" || degraded {
		return
	}
	if !turnStateValueFresh(desired.value, desired.observedAt, watcher.now(), watcher.settings.PushMaxAge) {
		watcher.clearDesired(desired.value)
		return
	}
	stored, ok := watcher.storedTurnState(ctx)
	if !ok {
		return
	}
	if desired.value == stored {
		watcher.clearDesired(desired.value)
		return
	}
	if err := watcher.updateCredential(ctx, &desired.value, nil); err != nil {
		if ctx.Err() == nil {
			turnStateLog(logrus.WarnLevel, logrus.Fields{
				"event":         "control.turn_state_push_failed",
				"credential_id": watcher.settings.CredentialID,
			}, "Turn state push failed, will retry: "+err.Error())
		}
		return
	}
	watcher.clearDesired(desired.value)
	turnStateLog(logrus.InfoLevel, logrus.Fields{
		"event":         "control.turn_state_pushed",
		"group_id":      watcher.settings.GroupID,
		"credential_id": watcher.settings.CredentialID,
		"length":        len(desired.value),
	}, fmt.Sprintf("injected fresh turn state (%d chars) into credential %d", len(desired.value), watcher.settings.CredentialID))
}

func (watcher *turnStateWatcher) clearDesired(value string) {
	watcher.mu.Lock()
	if watcher.desired.value == value {
		watcher.desired = turnStateCandidate{}
	}
	watcher.mu.Unlock()
}

// enterDegraded 进入（或保持在）降级：清空注入值（过期值继续注入会阻止上游发
// 新健康值）并确保备用代理生效。动作幂等，失败会在后续观测与节拍里重试。
func (watcher *turnStateWatcher) enterDegraded(ctx context.Context) {
	watcher.mu.Lock()
	first := !watcher.degraded
	if first {
		watcher.degraded = true
		watcher.verifyPaused = false
		watcher.verifyAttempts = 0
		watcher.nextVerifyAt = watcher.now()
	} else if watcher.verifyPaused {
		// 达到上限以后，只让下一条明确的降级观测重新开启一轮验证。
		watcher.verifyPaused = false
		watcher.verifyAttempts = 0
		watcher.nextVerifyAt = watcher.now()
	}
	// 旧候选必须在清库前丢掉，否则下一个 sweep 会把刚清掉的 state 回灌。
	watcher.desired = turnStateCandidate{}
	proxyConfirmed := watcher.degradeProxySet
	watcher.mu.Unlock()
	if first {
		turnStateLog(logrus.WarnLevel, logrus.Fields{
			"event":         "control.turn_state_degraded",
			"group_id":      watcher.settings.GroupID,
			"credential_id": watcher.settings.CredentialID,
		}, "non-healthy turn state observed; clearing injection and switching proxy")
	}
	stored, storedOK := watcher.storedTurnState(ctx)
	if !proxyConfirmed || !storedOK || stored != "" {
		watcher.ensureDegraded(ctx)
	}
}

// ensureDegraded 用一次凭据更新同时清掉旧 state 并切到备用代理。这样任何一步失败
// 都不会留下“备用代理已生效但旧 state 仍在注入”的半完成状态。
func (watcher *turnStateWatcher) ensureDegraded(ctx context.Context) {
	proxy := watcher.settings.DegradeProxy
	empty := ""
	if err := watcher.updateCredential(ctx, &empty, &proxy); err != nil {
		turnStateLog(logrus.WarnLevel, logrus.Fields{
			"event":         "control.turn_state_proxy_push_failed",
			"credential_id": watcher.settings.CredentialID,
		}, "Degraded-state update failed, will retry: "+err.Error())
		return
	}
	watcher.mu.Lock()
	watcher.degradeProxySet = true
	watcher.mu.Unlock()
	turnStateLog(logrus.InfoLevel, logrus.Fields{
		"event":         "control.turn_state_proxy_switched",
		"group_id":      watcher.settings.GroupID,
		"credential_id": watcher.settings.CredentialID,
		"mode":          string(proxy.Mode),
	}, fmt.Sprintf("credential %d turn state cleared and proxy switched to %s", watcher.settings.CredentialID, proxy.Mode))
}

// verifyOnce 发一次最小验证请求，健康则把 state 写回并把代理切回直连。
func (watcher *turnStateWatcher) verifyOnce(ctx context.Context) {
	target, err := watcher.service.buildDegradationTarget(
		ctx, watcher.settings.GroupID, watcher.settings.CredentialID,
	)
	if err != nil {
		if ctx.Err() == nil {
			turnStateLog(logrus.WarnLevel, logrus.Fields{
				"event":         "control.turn_state_verify_target_failed",
				"credential_id": watcher.settings.CredentialID,
			}, "Turn state verify target could not be built: "+err.Error())
			watcher.scheduleNextVerify()
		}
		return
	}
	// 代理不在备用配置上，说明上次写入没成功或者被人手动改过，重新推一遍。
	if !watcher.degradeProxyActive(target.credential.proxy) {
		watcher.mu.Lock()
		watcher.degradeProxySet = false
		watcher.mu.Unlock()
		watcher.ensureDegraded(ctx)
		// target 是切换前构建的快照，不能拿它立刻探测，否则这一枪仍会走旧代理。
		watcher.scheduleNextVerify()
		return
	}
	attempt := watcher.beginVerifyAttempt()
	result, probeErr := watcher.probe(ctx, target)
	if probeErr != nil {
		if ctx.Err() == nil {
			watcher.verifyFailed(attempt, "request failed: "+probeErr.Error())
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	value := result.UpstreamTurnState
	now := watcher.now()
	healthy := result.StatusCode >= http.StatusOK && result.StatusCode < http.StatusMultipleChoices &&
		turnStateLengthHealthy(value, watcher.settings.HealthyLength) &&
		turnStateValueFresh(value, now, now, watcher.settings.PushMaxAge)
	if !healthy {
		outcome := fmt.Sprintf("status %d", result.StatusCode)
		if value != "" {
			outcome = fmt.Sprintf("turn state %d chars", len(value))
		}
		watcher.verifyFailed(attempt, outcome)
		return
	}

	// 恢复也只做一次原子更新；不允许 state 写入失败后仍把代理切回直连。
	direct := watcher.settings.DirectProxy
	if err := watcher.updateCredential(ctx, &value, &direct); err != nil {
		turnStateLog(logrus.WarnLevel, logrus.Fields{
			"event":         "control.turn_state_restore_failed",
			"credential_id": watcher.settings.CredentialID,
		}, "Healthy-state restore failed, will retry on next sweep: "+err.Error())
		watcher.scheduleNextVerify()
		return
	}
	watcher.mu.Lock()
	watcher.desired = turnStateCandidate{}
	watcher.degraded = false
	watcher.degradeProxySet = false
	watcher.verifyPaused = false
	watcher.verifyAttempts = 0
	watcher.nextVerifyAt = time.Time{}
	watcher.mu.Unlock()
	turnStateLog(logrus.InfoLevel, logrus.Fields{
		"event":         "control.turn_state_restored",
		"group_id":      watcher.settings.GroupID,
		"credential_id": watcher.settings.CredentialID,
		"attempts":      attempt,
	}, fmt.Sprintf("healthy turn state restored after %d attempt(s) — proxy back to %s", attempt, direct.Mode))
}

func (watcher *turnStateWatcher) degradeProxyActive(effective outboundproxy.Effective) bool {
	return effective.Source == outboundproxy.SourceCredential &&
		effective.Config == watcher.settings.DegradeProxy
}

func (watcher *turnStateWatcher) beginVerifyAttempt() int {
	watcher.mu.Lock()
	watcher.verifyAttempts++
	attempt := watcher.verifyAttempts
	watcher.mu.Unlock()
	return attempt
}

func (watcher *turnStateWatcher) verifyFailed(attempt int, outcome string) {
	if watcher.settings.VerifyMax > 0 && attempt >= watcher.settings.VerifyMax {
		watcher.mu.Lock()
		watcher.verifyPaused = true
		watcher.mu.Unlock()
		turnStateLog(logrus.WarnLevel, logrus.Fields{
			"event":         "control.turn_state_verify_gave_up",
			"attempts":      attempt,
			"credential_id": watcher.settings.CredentialID,
		}, fmt.Sprintf("verify attempt limit reached after %d tries (%s); waiting for the next degraded sighting", attempt, outcome))
		return
	}
	turnStateLog(logrus.InfoLevel, logrus.Fields{
		"event":         "control.turn_state_verify_pending",
		"attempts":      attempt,
		"credential_id": watcher.settings.CredentialID,
	}, fmt.Sprintf("verify #%d: %s · retry in %s", attempt, outcome, watcher.settings.VerifyGap))
	watcher.scheduleNextVerify()
}

func (watcher *turnStateWatcher) scheduleNextVerify() {
	watcher.mu.Lock()
	watcher.nextVerifyAt = watcher.now().Add(watcher.settings.VerifyGap)
	watcher.mu.Unlock()
}

// probe 构造一个固定到目标凭据的最小 Responses 请求，直接走进程内执行器，
// 不经过 HTTP 回环也不消耗访问密钥。
func (watcher *turnStateWatcher) probe(
	ctx context.Context,
	target degradationTarget,
) (execution.AttemptResult, error) {
	model := watcher.settings.VerifyModel
	routeMode, supported := target.resolvedTarget.ModeForModel(
		protocol.OpenAIResponses, execution.OperationResponsesCreate, model,
	)
	if !supported {
		return execution.AttemptResult{}, fmt.Errorf(
			"channel %s does not expose a responses route for model %s", target.channelID, model,
		)
	}
	body, err := json.Marshal(turnStateVerifyRequest{
		Model: model, Input: "ping", Stream: false, MaxOutputTokens: 16,
	})
	if err != nil {
		return execution.AttemptResult{}, err
	}
	requestID, err := watcher.service.newExecutionID()
	if err != nil {
		return execution.AttemptResult{}, err
	}
	attemptID, err := watcher.service.newExecutionID()
	if err != nil {
		return execution.AttemptResult{}, err
	}
	spec := execution.NewAttemptSpec(execution.AttemptSpec{
		RequestID: requestID, AttemptID: attemptID, Sequence: 1,
		ChannelID:      string(target.channelID),
		RouteMode:      execution.RouteMode(routeMode),
		ClientProtocol: protocol.OpenAIResponses,
		Operation:      execution.OperationResponsesCreate,
		ClientModel:    model, UpstreamModel: model,
		Method: http.MethodPost, Path: "/v1/responses",
		Header:            applyControlHeaderRules(target.headerRules, target.credential.apiKey),
		Body:              body,
		ConfiguredHeaders: target.headerRules.ConfiguredNames(),
		TargetConfig:      target.resolvedTarget.TargetConfig,
		Timeouts:          degradationTimeouts(target, watcher.settings.VerifyTime),
		Credential:        target.credential.snapshot,
		Proxy:             target.credential.proxy,
		ProxyFingerprint:  target.credential.proxyFingerprint,
	})
	if err := spec.Validate(); err != nil {
		return execution.AttemptResult{}, fmt.Errorf("build verify attempt: %w", err)
	}
	attemptCtx := ctx
	if watcher.settings.VerifyTime > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, watcher.settings.VerifyTime)
		defer cancel()
	}
	return watcher.service.executor.Execute(attemptCtx, spec), nil
}

// storedTurnState 读取凭据当前注入值；注册表读不到时返回 ok=false，调用方下个
// 节拍重试。
func (watcher *turnStateWatcher) storedTurnState(ctx context.Context) (string, bool) {
	if watcher.service.registry == nil {
		return "", false
	}
	entries, err := watcher.service.registry.SnapshotGroupCredentialEntriesExact(
		watcher.settings.GroupID, []uint{watcher.settings.CredentialID},
	)
	if err != nil || len(entries) != 1 {
		return "", false
	}
	return entries[0].CodexTurnState, true
}

// updateCredential 复用管理端的凭据更新路径：校验、时效起点与注册表同步都已
// 在那里实现。turnState 为 nil 表示不动注入值，proxy 为 nil 表示不动代理。
func (watcher *turnStateWatcher) updateCredential(
	ctx context.Context,
	turnState *string,
	proxy *outboundproxy.Config,
) error {
	request := CredentialUpdateRequest{}
	if turnState != nil {
		request.CodexTurnState = optionalField[string]{Set: true, Value: *turnState}
		if *turnState != "" && watcher.settings.PushModels != "" {
			request.CodexTurnStateModels = optionalField[string]{
				Set: true, Value: watcher.settings.PushModels,
			}
		}
	}
	if proxy != nil {
		request.Proxy = optionalField[outboundproxy.Config]{Set: true, Value: *proxy}
	}
	_, err := watcher.service.UpdateGroupCredential(
		ctx, watcher.settings.GroupID, watcher.settings.CredentialID, request,
	)
	return err
}

type turnStateVerifyRequest struct {
	Model           string `json:"model"`
	Input           string `json:"input"`
	Stream          bool   `json:"stream"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

// ---------------------------------------------------------------------------
// 纯函数辅助
// ---------------------------------------------------------------------------

func turnStateModelWatched(models map[string]struct{}, names ...string) bool {
	for _, name := range names {
		if _, ok := models[strings.ToLower(strings.TrimSpace(name))]; ok {
			return true
		}
	}
	return false
}

func turnStateLengthHealthy(value string, healthy map[int]struct{}) bool {
	if value == "" {
		return false
	}
	_, ok := healthy[len(value)]
	return ok
}

func turnStateFresh(observedAt, now time.Time, maxAge time.Duration) bool {
	if observedAt.IsZero() || maxAge <= 0 || observedAt.After(now) {
		return false
	}
	return now.Sub(observedAt) <= maxAge
}

func turnStateValueFresh(value string, observedAt, now time.Time, maxAge time.Duration) bool {
	if issuedAt, ok := turnstate.FernetIssuedAt(value); ok {
		observedAt = issuedAt
	}
	return turnStateFresh(observedAt, now, maxAge)
}

func joinHealthyLengths(settings turnStateWatcherSettings) string {
	return joinTurnStateLengths(settings.HealthyLength)
}

func joinTurnStateLengths(values map[int]struct{}) string {
	lengths := make([]string, 0, len(values))
	for length := range values {
		lengths = append(lengths, strconv.Itoa(length))
	}
	sort.Strings(lengths)
	return strings.Join(lengths, "/")
}

func turnStateLog(level logrus.Level, fields logrus.Fields, message string) {
	utils.LogPlaneBestEffort(
		logrus.StandardLogger(), level, utils.LogPlaneControl, fields, message,
	)
}
