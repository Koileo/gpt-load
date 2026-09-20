package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/platform/config"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
	"gpt-load/internal/turnstate"
)

func TestTurnStateWatcherSettingsFromConfig(t *testing.T) {
	t.Parallel()
	// 生产链路里快照中的配置必然先经过 ParseTurnStateWatcherConfig，默认值由
	// 解析阶段填充；这里按同样的顺序构造，验证解析结果能原样转成运行设置。
	parsed, err := state.ParseTurnStateWatcherConfig(map[string]any{
		"enabled":           true,
		"group_id":          3,
		"credential_id":     7,
		"push_models":       "GPT-5.6-Sol",
		"degrade_proxy_url": "http://127.0.0.1:8080",
	})
	if err != nil {
		t.Fatalf("ParseTurnStateWatcherConfig() error = %v", err)
	}
	settings, err := turnStateWatcherSettingsFromConfig(*parsed)
	if err != nil {
		t.Fatalf("turnStateWatcherSettingsFromConfig() error = %v", err)
	}
	if !settings.Enabled || !settings.AutoBind || settings.GroupID != 0 || settings.CredentialID != 0 {
		t.Fatalf("web settings must wait for automatic binding: %+v", settings)
	}
	if settings.PushModels != "" || settings.VerifyModel != "" || len(settings.WatchModels) != 0 {
		t.Fatalf("web settings must not retain manual models: %+v", settings)
	}
	if _, ok := settings.HealthyLength[332]; !ok {
		t.Fatalf("healthy lengths must carry the parsed defaults: %+v", settings.HealthyLength)
	}
	if _, ok := settings.DegradedLength[312]; !ok {
		t.Fatalf("degraded lengths must carry the parsed defaults: %+v", settings.DegradedLength)
	}
	if !settings.DegradeReady || settings.DegradeProxy.URL != "http://127.0.0.1:8080" {
		t.Fatalf("degrade proxy must be ready: %+v", settings.DegradeProxy)
	}
	if settings.DirectProxy.Mode != outboundproxy.ModeDirect {
		t.Fatalf("direct proxy must stay direct: %+v", settings.DirectProxy)
	}
	if settings.PushMaxAge != time.Duration(state.DefaultTurnStatePushMaxAgeMS)*time.Millisecond {
		t.Fatalf("push max age must fall back to the default: %s", settings.PushMaxAge)
	}
	if settings.PollInterval != time.Duration(state.DefaultTurnStatePollIntervalSeconds)*time.Second {
		t.Fatalf("poll interval must fall back to the default: %s", settings.PollInterval)
	}
}

func TestTurnStateWatcherSettingsFromConfigDisabled(t *testing.T) {
	t.Parallel()
	if _, err := turnStateWatcherSettingsFromConfig(state.TurnStateWatcherConfig{Enabled: false}); err == nil {
		t.Fatal("a disabled config must not convert into runnable settings")
	}
	if _, err := turnStateWatcherSettingsFromConfig(state.TurnStateWatcherConfig{
		Enabled: true, PushModels: "m", DegradeProxyURL: "ftp://bad",
	}); err == nil {
		t.Fatal("an invalid degrade proxy must fail conversion")
	}
}

func TestResolveTurnStateWatcherSettingsPrefersWebConfig(t *testing.T) {
	t.Parallel()
	envSettings := turnStateWatcherSettings{Enabled: true, PushModels: "env-model"}
	webConfig, err := state.ParseTurnStateWatcherConfig(map[string]any{
		"enabled": true, "push_models": "web-model",
		"healthy_lengths": []any{292}, "degraded_lengths": []any{312},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &state.ConfigSnapshot{}
	snapshot.Settings.TurnStateWatcher = webConfig

	settings, source := resolveTurnStateWatcherSettings(snapshot, envSettings, nil)
	if settings == nil || !settings.AutoBind || settings.PushModels != "" || source != webConfig {
		t.Fatalf("web config must win over env: %+v source=%v", settings, source)
	}

	disabledSnapshot := &state.ConfigSnapshot{}
	disabledSnapshot.Settings.TurnStateWatcher = &state.TurnStateWatcherConfig{Enabled: false}
	if settings, _ := resolveTurnStateWatcherSettings(disabledSnapshot, envSettings, nil); settings != nil {
		t.Fatalf("a disabled web config must stop the watcher: %+v", settings)
	}

	settings, source = resolveTurnStateWatcherSettings(nil, envSettings, nil)
	if settings == nil || settings.PushModels != "env-model" || source != nil {
		t.Fatalf("without web config the env settings must apply: %+v source=%v", settings, source)
	}
	if settings, _ := resolveTurnStateWatcherSettings(nil, envSettings, errTurnStateWatcherDisabled); settings != nil {
		t.Fatal("disabled env must leave the watcher off")
	}
}

// TestRunTurnStateWatcherHotReloadsWebConfig 覆盖完整监督流程：Web 配置启动
// watcher、观测推送落库，以及改配置后按新模型热重启。
func TestRunTurnStateWatcherHotReloadsWebConfig(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	groupID := createGroupWithCredentials(t, fixture, "sk-turn-state-web")
	var credential models.Credential
	if err := fixture.db.Where("group_id = ?", groupID).Take(&credential).Error; err != nil {
		t.Fatal(err)
	}

	watcherConfig := func(model string) config.Settings {
		return config.Settings{
			state.SettingTurnStateWatcher: map[string]any{
				"enabled":               true,
				"group_id":              int(groupID),
				"credential_id":         int(credential.ID),
				"push_models":           model,
				"poll_interval_seconds": 1,
			},
		}
	}
	if _, err := fixture.manager.Publish(state.CompileInput{SystemSettings: watcherConfig("gpt-4o")}); err != nil {
		t.Fatalf("publish watcher config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.service.RunTurnStateWatcher(ctx)
	}()

	observe := func(model, value string) {
		t.Helper()
		turnstate.Observe(turnstate.Observation{
			CredentialID: credential.ID, ClientModel: model,
			TurnState: value, StatusCode: 200, ObservedAt: time.Now(),
		})
	}
	waitPushed := func(model, value string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			observe(model, value)
			var row models.Credential
			if err := fixture.db.Where("id = ?", credential.ID).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.CodexTurnState == value {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("credential turn state = %d chars, want the %d-char pushed value",
					len(row.CodexTurnState), len(value))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// 阶段一：watcher 按 gpt-4o 启动并推送健康观测。
	fresh := strings.Repeat("f", 292)
	waitPushed("gpt-4o", fresh)

	// 阶段二：无关设置发布不能把配置未变的 watcher 停掉。
	unrelated := watcherConfig("gpt-4o")
	unrelated[state.SettingRequestTimeout] = 900
	if _, err := fixture.manager.Publish(state.CompileInput{SystemSettings: unrelated}); err != nil {
		t.Fatalf("publish unrelated config: %v", err)
	}
	stillRunning := strings.Repeat("u", 292)
	waitPushed("gpt-4o", stillRunning)

	// 阶段三：Web 配置改成 gpt-4o-mini 后热重启，旧模型的观测不再生效。
	if _, err := fixture.manager.Publish(state.CompileInput{SystemSettings: watcherConfig("gpt-4o-mini")}); err != nil {
		t.Fatalf("publish reloaded watcher config: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	reloaded := strings.Repeat("r", 292)
	waitPushed("gpt-4o-mini", reloaded)

	// 阶段四：停用后用完全相同的配置重新启用，必须重新启动。
	if _, err := fixture.manager.Publish(state.CompileInput{SystemSettings: config.Settings{
		state.SettingTurnStateWatcher: map[string]any{"enabled": false},
	}}); err != nil {
		t.Fatalf("disable watcher config: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := fixture.manager.Publish(state.CompileInput{SystemSettings: watcherConfig("gpt-4o-mini")}); err != nil {
		t.Fatalf("re-enable watcher config: %v", err)
	}
	reenabled := strings.Repeat("e", 292)
	waitPushed("gpt-4o-mini", reenabled)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunTurnStateWatcher must stop after the context is cancelled")
	}
}

func TestSuperviseTurnStateWatcherStartsEnvironmentConfig(t *testing.T) {
	t.Parallel()
	fixture := newServiceFixture(t)
	groupID := createGroupWithCredentials(t, fixture, "sk-turn-state-env-supervisor")
	var credential models.Credential
	if err := fixture.db.Where("group_id = ?", groupID).Take(&credential).Error; err != nil {
		t.Fatal(err)
	}
	envSettings := turnStateWatcherSettings{
		Enabled: true, GroupID: groupID, CredentialID: credential.ID,
		PushModels: "gpt-4o", WatchModels: map[string]struct{}{"gpt-4o": {}},
		PushMaxAge: time.Hour, PollInterval: 50 * time.Millisecond,
		HealthyLength: map[int]struct{}{292: {}}, DegradedLength: map[int]struct{}{312: {}},
		VerifyGap: time.Minute, VerifyTime: time.Minute,
		DirectProxy: outboundproxy.Config{Mode: outboundproxy.ModeDirect},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.service.superviseTurnStateWatcher(ctx, envSettings, nil)
	}()

	want := strings.Repeat("v", 292)
	deadline := time.Now().Add(5 * time.Second)
	for {
		turnstate.Observe(turnstate.Observation{
			CredentialID: credential.ID, ClientModel: "gpt-4o",
			TurnState: want, StatusCode: 200, ObservedAt: time.Now(),
		})
		var row models.Credential
		if err := fixture.db.Where("id = ?", credential.ID).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.CodexTurnState == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("environment watcher did not start under the supervisor")
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}
