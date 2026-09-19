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
// 观测到新鲜的健康 state 就写入目标凭据；观测到非健康 state 就清空注入值并把
// 凭据代理切到备用出口，然后用最小请求验证，恢复健康后切回直连。
// ---------------------------------------------------------------------------

type turnStateWatcherSettings struct {
	Enabled       bool
	WatchModels   map[string]struct{}
	PushModels    string
	GroupID       uint
	CredentialID  uint
	PushMaxAge    time.Duration
	HealthyLength map[int]struct{}
	PollInterval  time.Duration
	DegradeProxy  outboundproxy.Config
	DegradeReady  bool // DegradeProxy.URL 非空时降级路径才启用
	DirectProxy   outboundproxy.Config
	VerifyModel   string
	VerifyGap     time.Duration // 两次验证请求之间的间隔
	VerifyTime    time.Duration // 单次验证请求的预算
	VerifyMax     int           // 0 = 不限次数
	Verbose       bool
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
		WatchModels:   map[string]struct{}{},
		HealthyLength: map[int]struct{}{},
		GroupID:       defaultTurnStateGroupID,
		CredentialID:  defaultTurnStateCredentialID,
		PushMaxAge:    defaultTurnStatePushAge,
		PollInterval:  defaultTurnStatePoll,
		VerifyGap:     defaultTurnStateVerifyGap,
		VerifyTime:    defaultTurnStateVerifyBudget,
	}
	if !envBool(getenv("TURN_STATE_WATCHER")) {
		return settings, errTurnStateWatcherDisabled
	}
	settings.Enabled = true

	settings.PushModels = strings.TrimSpace(getenv("TURN_STATE_PUSH_MODELS"))
	watchList := getenv("TURN_STATE_MODELS")
	if strings.TrimSpace(watchList) == "" {
		watchList = settings.PushModels
	}
	for _, model := range strings.Split(watchList, ",") {
		model = strings.ToLower(strings.TrimSpace(model))
		if model != "" {
			settings.WatchModels[model] = struct{}{}
		}
	}
	if len(settings.WatchModels) == 0 {
		return settings, fmt.Errorf("turn state watcher needs TURN_STATE_MODELS or TURN_STATE_PUSH_MODELS")
	}

	if value, err := envUint(getenv("TURN_STATE_PUSH_GROUP_ID")); err == nil && value > 0 {
		settings.GroupID = value
	}
	if value, err := envUint(getenv("TURN_STATE_PUSH_CREDENTIAL_ID")); err == nil && value > 0 {
		settings.CredentialID = value
	}
	if value, err := envMilliseconds(getenv("TURN_STATE_PUSH_MAX_AGE_MS")); err == nil && value > 0 {
		settings.PushMaxAge = value
	}
	for _, entry := range strings.Split(getenv("TURN_STATE_HEALTHY_LENGTHS"), ",") {
		if length, err := strconv.Atoi(strings.TrimSpace(entry)); err == nil && length > 0 {
			settings.HealthyLength[length] = struct{}{}
		}
	}
	if len(settings.HealthyLength) == 0 {
		settings.HealthyLength[292] = struct{}{}
	}
	if value, err := envSeconds(getenv("TURN_STATE_POLL_INTERVAL")); err == nil && value > 0 {
		settings.PollInterval = value
	}

	settings.DegradeProxy = outboundproxy.Config{
		Mode: outboundproxy.Mode(getenv("TURN_STATE_312_PROXY_MODE")),
		URL:  strings.TrimSpace(getenv("TURN_STATE_312_PROXY_URL")),
	}
	if settings.DegradeProxy.Mode == "" {
		settings.DegradeProxy.Mode = outboundproxy.ModeCustom
	}
	settings.DegradeReady = settings.DegradeProxy.URL != ""
	settings.DirectProxy = outboundproxy.Config{
		Mode: outboundproxy.Mode(getenv("TURN_STATE_DIRECT_PROXY_MODE")),
	}
	if settings.DirectProxy.Mode == "" {
		settings.DirectProxy = outboundproxy.Config{Mode: outboundproxy.ModeDirect}
	}

	settings.VerifyModel = strings.TrimSpace(getenv("TURN_STATE_VERIFY_MODEL"))
	if settings.VerifyModel == "" {
		settings.VerifyModel = firstWatchedModel(settings)
	}
	if value, err := envSeconds(getenv("TURN_STATE_VERIFY_INTERVAL")); err == nil && value > 0 {
		settings.VerifyGap = value
	}
	if value, err := envSeconds(getenv("TURN_STATE_VERIFY_TIMEOUT")); err == nil && value > 0 {
		settings.VerifyTime = value
	}
	if value, err := strconv.Atoi(strings.TrimSpace(getenv("TURN_STATE_VERIFY_MAX_ATTEMPTS"))); err == nil && value >= 0 {
		settings.VerifyMax = value
	}
	settings.Verbose = envBool(getenv("TURN_STATE_VERBOSE"))
	return settings, nil
}

func firstWatchedModel(settings turnStateWatcherSettings) string {
	return sortedWatchedModels(settings)[0]
}

func envBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envUint(value string) (uint, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	return uint(parsed), err
}

func envMilliseconds(value string) (time.Duration, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return time.Duration(parsed) * time.Millisecond, err
}

func envSeconds(value string) (time.Duration, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return time.Duration(parsed * float64(time.Second)), err
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
	desired         string // 最新观测到、还没确认写入凭据的健康 state
	degraded        bool   // 已进入降级（备用代理生效中）
	degradeProxySet bool   // 备用代理已确认写入
	verifyPaused    bool   // 达到尝试上限后暂停，等下一次非健康观测再恢复
	verifyAttempts  int
	nextVerifyAt    time.Time
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
		"Turn state watcher started (models %s · healthy lengths %s · poll %s)",
		strings.Join(sortedWatchedModels(settings), "/"),
		joinHealthyLengths(settings),
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
	if target.credential.proxy.Config != watcher.settings.DegradeProxy {
		return
	}
	watcher.mu.Lock()
	watcher.degraded = true
	watcher.degradeProxySet = true
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
	if !turnStateModelWatched(watcher.settings.WatchModels, observation.ClientModel, observation.UpstreamModel) {
		return
	}
	healthy := turnStateLengthHealthy(observation.TurnState, watcher.settings.HealthyLength)
	if watcher.settings.Verbose {
		turnStateLog(logrus.DebugLevel, logrus.Fields{
			"event":         "control.turn_state_observed",
			"credential_id": observation.CredentialID,
			"length":        len(observation.TurnState),
			"healthy":       healthy,
		}, fmt.Sprintf("observed turn state of %d chars (healthy=%v)", len(observation.TurnState), healthy))
	}
	if healthy {
		if watcher.now().Sub(observation.ObservedAt) > watcher.settings.PushMaxAge {
			return
		}
		watcher.mu.Lock()
		watcher.desired = observation.TurnState
		watcher.mu.Unlock()
		return
	}
	if !watcher.settings.DegradeReady {
		return
	}
	watcher.enterDegraded(ctx)
}

// sweep 是每个轮询节拍的一次巡检：把待写的注入值推下去；处于降级时按节奏验证。
func (watcher *turnStateWatcher) sweep(ctx context.Context) {
	watcher.flushPush(ctx)
	if !watcher.settings.DegradeReady || !watcher.degraded || watcher.verifyPaused {
		return
	}
	if watcher.now().Before(watcher.nextVerifyAt) {
		return
	}
	watcher.verifyOnce(ctx)
}

// flushPush 把最新健康 state 写入目标凭据；与凭据当前值相同就跳过。
func (watcher *turnStateWatcher) flushPush(ctx context.Context) {
	watcher.mu.Lock()
	desired := watcher.desired
	watcher.mu.Unlock()
	if desired == "" {
		return
	}
	stored, ok := watcher.storedTurnState(ctx)
	if !ok {
		return
	}
	if desired == stored {
		return
	}
	if err := watcher.updateCredential(ctx, &desired, nil); err != nil {
		if ctx.Err() == nil {
			turnStateLog(logrus.WarnLevel, logrus.Fields{
				"event":         "control.turn_state_push_failed",
				"credential_id": watcher.settings.CredentialID,
			}, "Turn state push failed, will retry: "+err.Error())
		}
		return
	}
	turnStateLog(logrus.InfoLevel, logrus.Fields{
		"event":         "control.turn_state_pushed",
		"group_id":      watcher.settings.GroupID,
		"credential_id": watcher.settings.CredentialID,
		"length":        len(desired),
	}, fmt.Sprintf("injected fresh turn state (%d chars) into credential %d", len(desired), watcher.settings.CredentialID))
}

// enterDegraded 进入（或保持在）降级：清空注入值（过期值继续注入会阻止上游发
// 新 292）并确保备用代理生效。两步都幂等，失败会在后续观测与节拍里重试。
func (watcher *turnStateWatcher) enterDegraded(ctx context.Context) {
	watcher.mu.Lock()
	first := !watcher.degraded
	watcher.degraded = true
	watcher.verifyPaused = false
	watcher.verifyAttempts = 0
	if first {
		watcher.nextVerifyAt = watcher.now()
	}
	proxyConfirmed := watcher.degradeProxySet
	watcher.mu.Unlock()
	if first {
		turnStateLog(logrus.WarnLevel, logrus.Fields{
			"event":         "control.turn_state_degraded",
			"group_id":      watcher.settings.GroupID,
			"credential_id": watcher.settings.CredentialID,
		}, "non-healthy turn state observed; clearing injection and switching proxy")
	}
	if stored, ok := watcher.storedTurnState(ctx); ok && stored != "" {
		empty := ""
		if err := watcher.updateCredential(ctx, &empty, nil); err != nil {
			turnStateLog(logrus.WarnLevel, logrus.Fields{
				"event":         "control.turn_state_clear_failed",
				"credential_id": watcher.settings.CredentialID,
			}, "Turn state clear failed, will retry on next sighting: "+err.Error())
		} else {
			turnStateLog(logrus.InfoLevel, logrus.Fields{
				"event":         "control.turn_state_cleared",
				"credential_id": watcher.settings.CredentialID,
			}, "cleared injected turn state")
		}
	}
	if !proxyConfirmed {
		watcher.ensureDegradeProxy(ctx)
	}
}

// ensureDegradeProxy 在还没有确认写入过备用代理时推一次；失败则下个节拍重试。
func (watcher *turnStateWatcher) ensureDegradeProxy(ctx context.Context) {
	proxy := watcher.settings.DegradeProxy
	if err := watcher.updateCredential(ctx, nil, &proxy); err != nil {
		turnStateLog(logrus.WarnLevel, logrus.Fields{
			"event":         "control.turn_state_proxy_push_failed",
			"credential_id": watcher.settings.CredentialID,
		}, "Degrade proxy push failed, will retry: "+err.Error())
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
	}, fmt.Sprintf("credential %d proxy switched to %s", watcher.settings.CredentialID, proxy.Mode))
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
	if target.credential.proxy.Config != watcher.settings.DegradeProxy {
		watcher.mu.Lock()
		watcher.degradeProxySet = false
		watcher.mu.Unlock()
		watcher.ensureDegradeProxy(ctx)
	}
	result, probeErr := watcher.probe(ctx, target)
	if probeErr != nil {
		if ctx.Err() == nil {
			turnStateLog(logrus.WarnLevel, logrus.Fields{
				"event":         "control.turn_state_verify_request_failed",
				"credential_id": watcher.settings.CredentialID,
			}, "Turn state verify request failed: "+probeErr.Error())
			watcher.scheduleNextVerify()
		}
		return
	}
	value := result.UpstreamTurnState
	if !turnStateLengthHealthy(value, watcher.settings.HealthyLength) {
		watcher.mu.Lock()
		watcher.verifyAttempts++
		attempts := watcher.verifyAttempts
		watcher.mu.Unlock()
		outcome := fmt.Sprintf("status %d", result.StatusCode)
		if value != "" {
			outcome = fmt.Sprintf("turn state %d chars", len(value))
		}
		if watcher.settings.VerifyMax > 0 && attempts >= watcher.settings.VerifyMax {
			watcher.mu.Lock()
			watcher.verifyPaused = true
			watcher.mu.Unlock()
			turnStateLog(logrus.WarnLevel, logrus.Fields{
				"event":         "control.turn_state_verify_gave_up",
				"attempts":      attempts,
				"credential_id": watcher.settings.CredentialID,
			}, fmt.Sprintf("verify attempt limit reached after %d tries (%s); waiting for the next non-healthy sighting", attempts, outcome))
			return
		}
		turnStateLog(logrus.InfoLevel, logrus.Fields{
			"event":         "control.turn_state_verify_pending",
			"attempts":      attempts,
			"credential_id": watcher.settings.CredentialID,
		}, fmt.Sprintf("verify #%d: %s · retry in %s", attempts, outcome, watcher.settings.VerifyGap))
		watcher.scheduleNextVerify()
		return
	}

	// 恢复：先把验证拿到的健康 state 写入凭据，再把代理切回直连。
	watcher.mu.Lock()
	watcher.desired = value
	watcher.mu.Unlock()
	watcher.flushPush(ctx)
	direct := watcher.settings.DirectProxy
	if err := watcher.updateCredential(ctx, nil, &direct); err != nil {
		turnStateLog(logrus.WarnLevel, logrus.Fields{
			"event":         "control.turn_state_restore_failed",
			"credential_id": watcher.settings.CredentialID,
		}, "Direct proxy restore failed, will retry on next sweep: "+err.Error())
		watcher.scheduleNextVerify()
		return
	}
	watcher.mu.Lock()
	watcher.degraded = false
	watcher.degradeProxySet = false
	watcher.verifyPaused = false
	attempts := watcher.verifyAttempts
	watcher.verifyAttempts = 0
	watcher.nextVerifyAt = time.Time{}
	watcher.mu.Unlock()
	turnStateLog(logrus.InfoLevel, logrus.Fields{
		"event":         "control.turn_state_restored",
		"group_id":      watcher.settings.GroupID,
		"credential_id": watcher.settings.CredentialID,
		"attempts":      attempts + 1,
	}, fmt.Sprintf("healthy turn state restored after %d attempt(s) — proxy back to %s", attempts+1, direct.Mode))
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

func sortedWatchedModels(settings turnStateWatcherSettings) []string {
	names := make([]string, 0, len(settings.WatchModels))
	for name := range settings.WatchModels {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func joinHealthyLengths(settings turnStateWatcherSettings) string {
	lengths := make([]string, 0, len(settings.HealthyLength))
	for length := range settings.HealthyLength {
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
