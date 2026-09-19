package control

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gpt-load/internal/execution"
	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/turnstate"
)

func getenvWith(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}

func TestLoadTurnStateWatcherSettingsDisabledByDefault(t *testing.T) {
	t.Parallel()
	if _, err := loadTurnStateWatcherSettings(getenvWith(nil)); err != errTurnStateWatcherDisabled {
		t.Fatalf("loadTurnStateWatcherSettings() error = %v, want disabled", err)
	}
}

func TestLoadTurnStateWatcherSettingsDefaultsAndFallbacks(t *testing.T) {
	t.Parallel()
	settings, err := loadTurnStateWatcherSettings(getenvWith(map[string]string{
		"TURN_STATE_WATCHER":     "1",
		"TURN_STATE_PUSH_MODELS": "GPT-5.6-Sol",
	}))
	if err != nil {
		t.Fatalf("loadTurnStateWatcherSettings() error = %v", err)
	}
	if _, ok := settings.WatchModels["gpt-5.6-sol"]; !ok {
		t.Fatalf("watch models should fall back to push models, got %v", settings.WatchModels)
	}
	if settings.GroupID != defaultTurnStateGroupID || settings.CredentialID != defaultTurnStateCredentialID {
		t.Fatalf("unexpected ids: group %d credential %d", settings.GroupID, settings.CredentialID)
	}
	if _, ok := settings.HealthyLength[292]; !ok || len(settings.HealthyLength) != 2 {
		t.Fatalf("default healthy lengths should include individual and team shapes, got %v", settings.HealthyLength)
	}
	if _, ok := settings.HealthyLength[332]; !ok {
		t.Fatalf("default healthy lengths should include team shape, got %v", settings.HealthyLength)
	}
	if _, ok := settings.DegradedLength[312]; !ok || len(settings.DegradedLength) != 2 {
		t.Fatalf("default degraded lengths should include individual and team shapes, got %v", settings.DegradedLength)
	}
	if settings.DegradeReady {
		t.Fatal("degrade path must be disabled without TURN_STATE_312_PROXY_URL")
	}
	if settings.DirectProxy.Mode != outboundproxy.ModeDirect {
		t.Fatalf("direct proxy mode = %q", settings.DirectProxy.Mode)
	}
	if settings.VerifyModel != "gpt-5.6-sol" {
		t.Fatalf("verify model should default to the first watched model, got %q", settings.VerifyModel)
	}
}

func TestLoadTurnStateWatcherSettingsRequiresModels(t *testing.T) {
	t.Parallel()
	if _, err := loadTurnStateWatcherSettings(getenvWith(map[string]string{
		"TURN_STATE_WATCHER": "1",
	})); err == nil {
		t.Fatal("expected an error when no watch models are configured")
	}
}

func TestLoadTurnStateWatcherSettingsFullConfiguration(t *testing.T) {
	t.Parallel()
	settings, err := loadTurnStateWatcherSettings(getenvWith(map[string]string{
		"TURN_STATE_WATCHER":             "1",
		"TURN_STATE_MODELS":              "model-a",
		"TURN_STATE_PUSH_MODELS":         "model-a",
		"TURN_STATE_PUSH_GROUP_ID":       "3",
		"TURN_STATE_PUSH_CREDENTIAL_ID":  "4",
		"TURN_STATE_CREDENTIAL_ID":       "4",
		"TURN_STATE_PUSH_MAX_AGE_MS":     "60000",
		"TURN_STATE_HEALTHY_LENGTHS":     "292,332",
		"TURN_STATE_DEGRADED_LENGTHS":    "312,356",
		"TURN_STATE_POLL_INTERVAL":       "5",
		"TURN_STATE_312_PROXY_MODE":      "custom",
		"TURN_STATE_312_PROXY_URL":       "socks5://127.0.0.1:3010",
		"TURN_STATE_VERIFY_MODEL":        "model-a",
		"TURN_STATE_VERIFY_INTERVAL":     "30",
		"TURN_STATE_VERIFY_MAX_ATTEMPTS": "3",
	}))
	if err != nil {
		t.Fatalf("loadTurnStateWatcherSettings() error = %v", err)
	}
	if settings.GroupID != 3 || settings.CredentialID != 4 {
		t.Fatalf("unexpected ids: group %d credential %d", settings.GroupID, settings.CredentialID)
	}
	if settings.PushMaxAge != time.Minute {
		t.Fatalf("push max age = %s", settings.PushMaxAge)
	}
	if len(settings.HealthyLength) != 2 {
		t.Fatalf("healthy lengths = %v", settings.HealthyLength)
	}
	if settings.PollInterval != 5*time.Second || settings.VerifyGap != 30*time.Second {
		t.Fatalf("intervals: poll %s verify %s", settings.PollInterval, settings.VerifyGap)
	}
	if !settings.DegradeReady || settings.DegradeProxy.URL != "socks5://127.0.0.1:3010" {
		t.Fatalf("degrade proxy = %+v", settings.DegradeProxy)
	}
	if settings.VerifyMax != 3 || settings.VerifyModel != "model-a" {
		t.Fatalf("verify config = max %d model %q", settings.VerifyMax, settings.VerifyModel)
	}
}

func TestLoadTurnStateWatcherSettingsRejectsCrossBinding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values map[string]string
	}{
		{
			name: "source credential differs from target",
			values: map[string]string{
				"TURN_STATE_PUSH_CREDENTIAL_ID": "4",
				"TURN_STATE_CREDENTIAL_ID":      "5",
			},
		},
		{
			name: "multiple injection models",
			values: map[string]string{
				"TURN_STATE_PUSH_MODELS": "model-a,model-b",
			},
		},
		{
			name: "wildcard injection model",
			values: map[string]string{
				"TURN_STATE_PUSH_MODELS": "model-*",
			},
		},
		{
			name: "observation model differs from injection model",
			values: map[string]string{
				"TURN_STATE_MODELS": "model-b",
			},
		},
		{
			name: "verification model differs from injection model",
			values: map[string]string{
				"TURN_STATE_VERIFY_MODEL": "model-b",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{
				"TURN_STATE_WATCHER":     "1",
				"TURN_STATE_PUSH_MODELS": "model-a",
			}
			for key, value := range test.values {
				values[key] = value
			}
			if _, err := loadTurnStateWatcherSettings(getenvWith(values)); err == nil {
				t.Fatalf("expected cross-binding configuration to be rejected: %v", test.values)
			}
		})
	}
}

func TestLoadTurnStateWatcherSettingsRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "toggle", key: "TURN_STATE_WATCHER", value: "sometimes"},
		{name: "group", key: "TURN_STATE_PUSH_GROUP_ID", value: "zero"},
		{name: "source credential", key: "TURN_STATE_CREDENTIAL_ID", value: "0"},
		{name: "healthy lengths", key: "TURN_STATE_HEALTHY_LENGTHS", value: "292,nope"},
		{name: "poll interval", key: "TURN_STATE_POLL_INTERVAL", value: "forever"},
		{name: "verify attempts", key: "TURN_STATE_VERIFY_MAX_ATTEMPTS", value: "-1"},
		{name: "proxy", key: "TURN_STATE_DEGRADE_PROXY_URL", value: "ftp://proxy.example.test:21"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{
				"TURN_STATE_WATCHER":     "1",
				"TURN_STATE_PUSH_MODELS": "gpt-5.6-sol",
				test.key:                 test.value,
			}
			if _, err := loadTurnStateWatcherSettings(getenvWith(values)); err == nil {
				t.Fatalf("expected %s=%q to be rejected", test.key, test.value)
			}
		})
	}
}

func TestTurnStateClassificationHelpers(t *testing.T) {
	t.Parallel()
	healthy := map[int]struct{}{292: {}, 332: {}}
	if !turnStateLengthHealthy(strings.Repeat("a", 292), healthy) {
		t.Fatal("292 chars should be healthy")
	}
	if !turnStateLengthHealthy(strings.Repeat("a", 332), healthy) {
		t.Fatal("332 chars should be healthy")
	}
	if turnStateLengthHealthy(strings.Repeat("a", 312), healthy) {
		t.Fatal("312 chars should not be healthy")
	}
	if turnStateLengthHealthy("", healthy) {
		t.Fatal("empty value should not be healthy")
	}
	models := map[string]struct{}{"gpt-5.6-sol": {}}
	if !turnStateModelWatched(models, "GPT-5.6-Sol", "other") {
		t.Fatal("model matching should be case-insensitive across both names")
	}
	if turnStateModelWatched(models, "gpt-4o") {
		t.Fatal("unwatched model must not match")
	}
}

func TestTurnStateValueFreshUsesFernetIssuedAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	raw := make([]byte, 1+8+16+10*16+32)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(now.Add(-2*time.Hour).Unix()))
	value := base64.URLEncoding.EncodeToString(raw)
	if turnStateValueFresh(value, now, now, time.Hour) {
		t.Fatal("a structurally valid old Fernet token must use its issued time, not its observation time")
	}
	if !turnStateValueFresh(strings.Repeat("f", 292), now, now, time.Hour) {
		t.Fatal("an unknown envelope should fall back to its trusted in-process observation time")
	}
}

// turnStateRecordingExecutor records verify probes and returns a canned result.
type turnStateRecordingExecutor struct {
	mu     sync.Mutex
	result execution.AttemptResult
	specs  []execution.AttemptSpec
}

func (executor *turnStateRecordingExecutor) Execute(
	_ context.Context, spec execution.AttemptSpec,
) execution.AttemptResult {
	executor.mu.Lock()
	executor.specs = append(executor.specs, spec)
	result := executor.result
	executor.mu.Unlock()
	return result
}

func (executor *turnStateRecordingExecutor) ExecuteStream(
	_ context.Context, _ execution.AttemptSpec, _ execution.StreamSink,
) execution.StreamResult {
	return execution.StreamResult{}
}

func (executor *turnStateRecordingExecutor) recordedSpecs() []execution.AttemptSpec {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]execution.AttemptSpec(nil), executor.specs...)
}

func newTurnStateWatcherFixture(t *testing.T) (serviceFixture, *turnStateWatcher, uint, uint) {
	t.Helper()
	fixture := newServiceFixture(t)
	groupID := createGroupWithCredentials(t, fixture, "sk-turn-state-watcher")
	var row models.Credential
	if err := fixture.db.Where("group_id = ?", groupID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	length := 292
	watcher := &turnStateWatcher{
		service:      fixture.service,
		observations: make(chan turnstate.Observation, turnStateObservationBuffer),
		now:          func() time.Time { return time.UnixMilli(row.UpdatedAtMS + 5000) },
		settings: turnStateWatcherSettings{
			Enabled:        true,
			WatchModels:    map[string]struct{}{"gpt-4o": {}},
			PushModels:     "gpt-4o",
			GroupID:        groupID,
			CredentialID:   row.ID,
			PushMaxAge:     time.Hour,
			HealthyLength:  map[int]struct{}{length: {}},
			DegradedLength: map[int]struct{}{312: {}, 356: {}},
			PollInterval:   defaultTurnStatePoll,
			DirectProxy:    outboundproxy.Config{Mode: outboundproxy.ModeDirect},
			VerifyModel:    "gpt-4o",
			VerifyGap:      time.Second,
			VerifyTime:     10 * time.Second,
		},
	}
	return fixture, watcher, groupID, row.ID
}

func credentialTurnStateRow(t *testing.T, fixture serviceFixture, credentialID uint) models.Credential {
	t.Helper()
	var row models.Credential
	if err := fixture.db.Where("id = ?", credentialID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func TestTurnStateWatcherPushesFreshHealthyState(t *testing.T) {
	t.Parallel()
	fixture, watcher, _, credentialID := newTurnStateWatcherFixture(t)
	fresh := strings.Repeat("f", 292)

	watcher.observe(t.Context(), turnstate.Observation{
		CredentialID: credentialID, ClientModel: "gpt-4o",
		TurnState: fresh, StatusCode: http.StatusOK,
		ObservedAt: watcher.now().Add(-time.Second),
	})
	watcher.flushPush(t.Context())

	row := credentialTurnStateRow(t, fixture, credentialID)
	if row.CodexTurnState != fresh {
		t.Fatalf("credential turn state = %d chars, want the pushed value", len(row.CodexTurnState))
	}
	if row.CodexTurnStateModels != "gpt-4o" {
		t.Fatalf("credential turn state models = %q", row.CodexTurnStateModels)
	}
	if row.CodexTurnStateSetAtMS == 0 {
		t.Fatal("pushing a new value must set the freshness timestamp")
	}

	// 同一个值重复观测不产生重复写入：写入时刻应保持不变。
	setAt := row.CodexTurnStateSetAtMS
	watcher.observe(t.Context(), turnstate.Observation{
		CredentialID: credentialID, ClientModel: "gpt-4o",
		TurnState: fresh, StatusCode: http.StatusOK,
		ObservedAt: watcher.now().Add(-time.Second),
	})
	watcher.flushPush(t.Context())
	if row := credentialTurnStateRow(t, fixture, credentialID); row.CodexTurnStateSetAtMS != setAt {
		t.Fatalf("duplicate push changed set_at from %d to %d", setAt, row.CodexTurnStateSetAtMS)
	}

	// 超过时效上限的老值不允许注入。
	watcher.observe(t.Context(), turnstate.Observation{
		CredentialID: credentialID, ClientModel: "gpt-4o",
		TurnState: strings.Repeat("o", 292), StatusCode: http.StatusOK,
		ObservedAt: watcher.now().Add(-2 * time.Hour),
	})
	watcher.flushPush(t.Context())
	if row := credentialTurnStateRow(t, fixture, credentialID); row.CodexTurnState != fresh {
		t.Fatal("stale observations must never be injected")
	}

	// 候选进入队列时还新鲜、真正落库前已经过期，也不能被延迟注入。
	now := watcher.now()
	watcher.now = func() time.Time { return now }
	delayed := strings.Repeat("l", 292)
	watcher.observe(t.Context(), turnstate.Observation{
		CredentialID: credentialID, ClientModel: "gpt-4o",
		TurnState: delayed, StatusCode: http.StatusOK,
		ObservedAt: now.Add(-59 * time.Minute),
	})
	watcher.now = func() time.Time { return now.Add(2 * time.Minute) }
	watcher.flushPush(t.Context())
	if row := credentialTurnStateRow(t, fixture, credentialID); row.CodexTurnState != fresh {
		t.Fatal("a queued observation that expired before flush must not be injected")
	}
}

func TestTurnStateWatcherIgnoresUnwatchedModels(t *testing.T) {
	t.Parallel()
	fixture, watcher, _, credentialID := newTurnStateWatcherFixture(t)

	watcher.observe(t.Context(), turnstate.Observation{
		CredentialID: credentialID, ClientModel: "gpt-4o-mini",
		TurnState: strings.Repeat("f", 292), StatusCode: http.StatusOK,
		ObservedAt: watcher.now(),
	})
	watcher.flushPush(t.Context())
	if row := credentialTurnStateRow(t, fixture, credentialID); row.CodexTurnState != "" {
		t.Fatal("unwatched model observations must not be pushed")
	}
}

func TestTurnStateWatcherIgnoresOtherCredentialsAndUnknownShapes(t *testing.T) {
	t.Parallel()
	fixture, watcher, _, credentialID := newTurnStateWatcherFixture(t)
	watcher.settings.DegradeProxy = outboundproxy.Config{
		Mode: outboundproxy.ModeCustom, URL: "socks5://127.0.0.1:3010",
	}
	watcher.settings.DegradeReady = true

	for _, observation := range []turnstate.Observation{
		{
			CredentialID: credentialID + 1, ClientModel: "gpt-4o",
			TurnState: strings.Repeat("f", 292), StatusCode: http.StatusOK, ObservedAt: watcher.now(),
		},
		{
			CredentialID: credentialID, ClientModel: "gpt-4o",
			TurnState: strings.Repeat("u", 340), StatusCode: http.StatusOK, ObservedAt: watcher.now(),
		},
	} {
		watcher.observe(t.Context(), observation)
	}
	watcher.sweep(t.Context())
	row := credentialTurnStateRow(t, fixture, credentialID)
	if row.CodexTurnState != "" || row.ProxyConfig != nil {
		t.Fatalf("unrelated observations changed the target credential: state=%d proxy=%v", len(row.CodexTurnState), row.ProxyConfig != nil)
	}
	watcher.mu.Lock()
	degraded := watcher.degraded
	watcher.mu.Unlock()
	if degraded {
		t.Fatal("unknown turn-state lengths must not trigger proxy mutation")
	}
}

func TestTurnStateWatcherDegradesOnUnhealthyState(t *testing.T) {
	t.Parallel()
	fixture, watcher, _, credentialID := newTurnStateWatcherFixture(t)
	healthy := strings.Repeat("h", 292)
	watcher.settings.DegradeProxy = outboundproxy.Config{
		Mode: outboundproxy.ModeCustom, URL: "socks5://127.0.0.1:3010",
	}
	watcher.settings.DegradeReady = true

	// 先让凭据持有一个健康值，再观测到 312：注入值应被清空，代理切到备用。
	watcher.observe(t.Context(), turnstate.Observation{
		CredentialID: credentialID, ClientModel: "gpt-4o",
		TurnState: healthy, StatusCode: http.StatusOK, ObservedAt: watcher.now(),
	})
	watcher.flushPush(t.Context())
	if row := credentialTurnStateRow(t, fixture, credentialID); row.CodexTurnState != healthy {
		t.Fatal("precondition failed: credential should hold the healthy state")
	}

	watcher.observe(t.Context(), turnstate.Observation{
		CredentialID: credentialID, ClientModel: "gpt-4o",
		TurnState: strings.Repeat("d", 312), StatusCode: http.StatusOK, ObservedAt: watcher.now(),
	})
	// 回归：下一个节拍不能把降级前残留的健康候选重新注入。
	watcher.sweep(t.Context())

	watcher.mu.Lock()
	degraded := watcher.degraded
	watcher.mu.Unlock()
	if !degraded {
		t.Fatal("non-healthy observation must enter degraded mode")
	}
	row := credentialTurnStateRow(t, fixture, credentialID)
	if row.CodexTurnState != "" {
		t.Fatalf("degrade must clear the injected state, got %d chars", len(row.CodexTurnState))
	}
	if row.CodexTurnStateSetAtMS != 0 {
		t.Fatal("clearing must reset the freshness timestamp")
	}
	var proxy outboundproxy.Config
	decodeProxy := func(raw *string) outboundproxy.Config {
		t.Helper()
		plaintext, err := fixture.encryption.Decrypt(*raw)
		if err != nil {
			t.Fatalf("decrypt proxy config: %v", err)
		}
		var config outboundproxy.Config
		if err := json.Unmarshal([]byte(plaintext), &config); err != nil {
			t.Fatalf("decode proxy config: %v", err)
		}
		return config
	}
	proxy = decodeProxy(row.ProxyConfig)
	if proxy.Mode != outboundproxy.ModeCustom || proxy.URL != "socks5://127.0.0.1:3010" {
		t.Fatalf("degrade proxy = %+v", proxy)
	}

	// 恢复直连：验证请求返回健康值后，state 写回、代理切回 direct。
	stub := &turnStateRecordingExecutor{result: execution.AttemptResult{
		DispatchState: execution.DispatchMaybeSent, ResponseStarted: true,
		StatusCode: http.StatusOK, UpstreamTurnState: healthy,
	}}
	fixture.service.executor = stub
	watcher.verifyOnce(t.Context())

	watcher.mu.Lock()
	stillDegraded := watcher.degraded
	watcher.mu.Unlock()
	if stillDegraded {
		t.Fatal("verify with a healthy state must leave degraded mode")
	}
	if len(stub.recordedSpecs()) != 1 {
		t.Fatalf("probe count = %d, want 1", len(stub.recordedSpecs()))
	}
	probe := stub.recordedSpecs()[0]
	if probe.Credential.ID != credentialID || probe.Operation != execution.OperationResponsesCreate {
		t.Fatalf("probe spec targets credential %d op %s", probe.Credential.ID, probe.Operation)
	}
	if probe.Proxy.Config.Mode != outboundproxy.ModeCustom {
		t.Fatalf("probe should run through the degrade proxy, got %+v", probe.Proxy.Config)
	}
	row = credentialTurnStateRow(t, fixture, credentialID)
	if row.CodexTurnState != healthy {
		t.Fatal("restore must write the verified healthy state back")
	}
	if row.ProxyConfig == nil {
		t.Fatal("restore must push the direct proxy config")
	} else if proxy = decodeProxy(row.ProxyConfig); proxy.Mode != outboundproxy.ModeDirect {
		t.Fatalf("restored proxy = %+v", proxy)
	}
}

func TestTurnStateWatcherRestoresStartupDegradedState(t *testing.T) {
	t.Parallel()
	fixture, watcher, _, credentialID := newTurnStateWatcherFixture(t)
	degrade := outboundproxy.Config{
		Mode: outboundproxy.ModeCustom, URL: "socks5://127.0.0.1:3010",
	}
	if _, err := fixture.service.UpdateGroupCredential(t.Context(), watcher.settings.GroupID, credentialID,
		CredentialUpdateRequest{Proxy: optionalField[outboundproxy.Config]{Set: true, Value: degrade}},
	); err != nil {
		t.Fatalf("precondition: set degrade proxy: %v", err)
	}
	watcher.settings.DegradeProxy = degrade
	watcher.settings.DegradeReady = true

	watcher.restoreState(t.Context())
	watcher.mu.Lock()
	degraded, proxySet := watcher.degraded, watcher.degradeProxySet
	watcher.mu.Unlock()
	if !degraded || proxySet {
		t.Fatalf("startup on the degrade proxy must resume and reconfirm atomic state (degraded=%v set=%v)", degraded, proxySet)
	}
	watcher.sweep(t.Context())
	watcher.mu.Lock()
	proxySet = watcher.degradeProxySet
	watcher.mu.Unlock()
	if !proxySet {
		t.Fatal("first sweep must confirm the cleared state and degrade proxy together")
	}

	// 未开启降级路径时不做任何恢复判定。
	watcher.settings.DegradeReady = false
	watcher.mu.Lock()
	watcher.degraded, watcher.degradeProxySet = false, false
	watcher.mu.Unlock()
	watcher.restoreState(t.Context())
	watcher.mu.Lock()
	degraded = watcher.degraded
	watcher.mu.Unlock()
	if degraded {
		t.Fatal("restoreState must be a no-op without a configured degrade proxy")
	}
}

func TestTurnStateWatcherGiveUpAfterVerifyLimit(t *testing.T) {
	t.Parallel()
	fixture, watcher, _, _ := newTurnStateWatcherFixture(t)
	watcher.settings.DegradeProxy = outboundproxy.Config{
		Mode: outboundproxy.ModeCustom, URL: "socks5://127.0.0.1:3010",
	}
	watcher.settings.DegradeReady = true
	watcher.settings.VerifyMax = 1
	stub := &turnStateRecordingExecutor{result: execution.AttemptResult{
		DispatchState: execution.DispatchMaybeSent, ResponseStarted: true,
		StatusCode: http.StatusOK, UpstreamTurnState: strings.Repeat("d", 312),
	}}
	fixture.service.executor = stub

	watcher.mu.Lock()
	watcher.degraded = true
	watcher.mu.Unlock()
	watcher.ensureDegraded(t.Context())
	watcher.verifyOnce(t.Context())

	watcher.mu.Lock()
	paused, attempts := watcher.verifyPaused, watcher.verifyAttempts
	watcher.mu.Unlock()
	if !paused || attempts != 1 {
		t.Fatalf("after the attempt limit: paused=%v attempts=%d", paused, attempts)
	}
	// 暂停期间 sweep 不再发探测。
	watcher.sweep(t.Context())
	if len(stub.recordedSpecs()) != 1 {
		t.Fatalf("sweep while paused must not probe, probe count = %d", len(stub.recordedSpecs()))
	}

	// 活跃验证期间的重复异常不能把计数清零；暂停以后下一条明确异常才开启新一轮。
	watcher.mu.Lock()
	watcher.verifyPaused = false
	watcher.verifyAttempts = 1
	watcher.mu.Unlock()
	watcher.observe(t.Context(), turnstate.Observation{
		CredentialID: watcher.settings.CredentialID, ClientModel: "gpt-4o",
		TurnState: strings.Repeat("d", 312), StatusCode: http.StatusOK, ObservedAt: watcher.now(),
	})
	watcher.mu.Lock()
	attempts = watcher.verifyAttempts
	watcher.verifyPaused = true
	watcher.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("active degraded observation reset attempts to %d", attempts)
	}
	watcher.observe(t.Context(), turnstate.Observation{
		CredentialID: watcher.settings.CredentialID, ClientModel: "gpt-4o",
		TurnState: strings.Repeat("d", 312), StatusCode: http.StatusOK, ObservedAt: watcher.now(),
	})
	watcher.mu.Lock()
	paused, attempts = watcher.verifyPaused, watcher.verifyAttempts
	watcher.mu.Unlock()
	if paused || attempts != 0 {
		t.Fatalf("new degraded sighting should resume a paused cycle: paused=%v attempts=%d", paused, attempts)
	}
}

func TestTurnStateWatcherRebuildsTargetAfterProxySwitch(t *testing.T) {
	t.Parallel()
	fixture, watcher, _, credentialID := newTurnStateWatcherFixture(t)
	watcher.settings.DegradeProxy = outboundproxy.Config{
		Mode: outboundproxy.ModeCustom, URL: "socks5://127.0.0.1:3010",
	}
	watcher.settings.DegradeReady = true
	healthy := strings.Repeat("h", 292)
	stub := &turnStateRecordingExecutor{result: execution.AttemptResult{
		DispatchState: execution.DispatchMaybeSent, ResponseStarted: true,
		StatusCode: http.StatusOK, UpstreamTurnState: healthy,
	}}
	fixture.service.executor = stub
	watcher.mu.Lock()
	watcher.degraded = true
	// Simulate stale in-memory confirmation while the persisted credential is direct.
	watcher.degradeProxySet = true
	watcher.mu.Unlock()

	watcher.verifyOnce(t.Context())
	if got := len(stub.recordedSpecs()); got != 0 {
		t.Fatalf("verification used the pre-switch target snapshot, probes = %d", got)
	}
	row := credentialTurnStateRow(t, fixture, credentialID)
	if row.ProxyConfig == nil {
		t.Fatal("verify should restore the degrade proxy before probing")
	}

	watcher.verifyOnce(t.Context())
	if got := len(stub.recordedSpecs()); got != 1 {
		t.Fatalf("verification after rebuilding the target probes = %d, want 1", got)
	}
}

func TestTurnStateBusSubscribeAndRestore(t *testing.T) {
	t.Parallel()
	seen := make(chan turnstate.Observation, 1)
	second := make(chan turnstate.Observation, 1)
	restore := turnstate.Subscribe(func(o turnstate.Observation) { seen <- o })
	restoreSecond := turnstate.Subscribe(func(o turnstate.Observation) { second <- o })
	defer restoreSecond()
	turnstate.Observe(turnstate.Observation{CredentialID: 7, TurnState: "value"})
	if got := <-seen; got.CredentialID != 7 || got.TurnState != "value" {
		t.Fatalf("observation = %+v", got)
	}
	if got := <-second; got.CredentialID != 7 {
		t.Fatalf("second subscriber observation = %+v", got)
	}
	restore()
	turnstate.Observe(turnstate.Observation{CredentialID: 8})
	select {
	case got := <-seen:
		t.Fatalf("restore must detach the subscriber, got %+v", got)
	default:
	}
	if got := <-second; got.CredentialID != 8 {
		t.Fatalf("detaching one subscriber affected another: %+v", got)
	}
	restoreSecond()
	// 没有订阅者时 Observe 必须是无害的空操作；解除函数也必须幂等。
	restoreSecond()
	turnstate.Observe(turnstate.Observation{CredentialID: 9})
}

func TestTurnStateWatcherChannelOverflowDropsOldest(t *testing.T) {
	t.Parallel()
	_, watcher, _, credentialID := newTurnStateWatcherFixture(t)
	for i := 0; i < turnStateObservationBuffer+10; i++ {
		watcher.forward(turnstate.Observation{CredentialID: credentialID, ClientModel: "gpt-4o"})
	}
	if len(watcher.observations) != turnStateObservationBuffer {
		t.Fatalf("buffer length = %d, want %d", len(watcher.observations), turnStateObservationBuffer)
	}
}
