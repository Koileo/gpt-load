package state

import (
	"testing"
)

func TestParseTurnStateWatcherConfigNil(t *testing.T) {
	t.Parallel()
	config, err := ParseTurnStateWatcherConfig(nil)
	if err != nil {
		t.Fatalf("ParseTurnStateWatcherConfig(nil) error = %v", err)
	}
	if config != nil {
		t.Fatalf("ParseTurnStateWatcherConfig(nil) = %#v, want nil", config)
	}
}

func TestParseTurnStateWatcherConfigDefaults(t *testing.T) {
	t.Parallel()
	config, err := ParseTurnStateWatcherConfig(map[string]any{})
	if err != nil {
		t.Fatalf("ParseTurnStateWatcherConfig({}) error = %v", err)
	}
	if config.Enabled {
		t.Fatal("default enabled must be false")
	}
	if config.GroupID != DefaultTurnStateGroupID || config.CredentialID != DefaultTurnStateCredentialID {
		t.Fatalf("default binding = %d/%d", config.GroupID, config.CredentialID)
	}
	if config.PushMaxAgeMS != DefaultTurnStatePushMaxAgeMS {
		t.Fatalf("default push max age = %d", config.PushMaxAgeMS)
	}
	if len(config.HealthyLengths) != 2 || config.HealthyLengths[0] != 292 || config.HealthyLengths[1] != 332 {
		t.Fatalf("default healthy lengths = %v", config.HealthyLengths)
	}
	if len(config.DegradedLengths) != 2 || config.DegradedLengths[0] != 312 || config.DegradedLengths[1] != 356 {
		t.Fatalf("default degraded lengths = %v", config.DegradedLengths)
	}
	if config.PollIntervalSeconds != DefaultTurnStatePollIntervalSeconds {
		t.Fatalf("default poll interval = %d", config.PollIntervalSeconds)
	}
	if config.VerifyIntervalSeconds != DefaultTurnStateVerifyIntervalSeconds {
		t.Fatalf("default verify interval = %d", config.VerifyIntervalSeconds)
	}
	if config.VerifyTimeoutSeconds != DefaultTurnStateVerifyTimeoutSeconds {
		t.Fatalf("default verify timeout = %d", config.VerifyTimeoutSeconds)
	}
	if config.VerifyMaxAttempts != 0 || config.Verbose || config.DegradeProxyURL != "" {
		t.Fatalf("unexpected defaults: %+v", config)
	}
}

func TestParseTurnStateWatcherConfigEnabledNormalizesProxy(t *testing.T) {
	t.Parallel()
	config, err := ParseTurnStateWatcherConfig(map[string]any{
		"enabled":                 true,
		"group_id":                3,
		"credential_id":           7,
		"push_models":             "GPT-5.6-Sol",
		"push_max_age_ms":         60000,
		"healthy_lengths":         []any{292},
		"degraded_lengths":        []any{312, 356},
		"poll_interval_seconds":   5,
		"degrade_proxy_url":       "socks5://127.0.0.1:1080",
		"verify_interval_seconds": 30,
		"verify_timeout_seconds":  60,
		"verify_max_attempts":     3,
		"verbose":                 true,
	})
	if err != nil {
		t.Fatalf("ParseTurnStateWatcherConfig(full) error = %v", err)
	}
	if !config.Enabled || config.PushModels != "GPT-5.6-Sol" {
		t.Fatalf("unexpected model config: %+v", config)
	}
	if config.GroupID != 3 || config.CredentialID != 7 {
		t.Fatalf("unexpected binding: %+v", config)
	}
	if config.PushMaxAgeMS != 60000 || config.PollIntervalSeconds != 5 ||
		config.VerifyIntervalSeconds != 30 || config.VerifyTimeoutSeconds != 60 ||
		config.VerifyMaxAttempts != 3 || !config.Verbose {
		t.Fatalf("unexpected numeric config: %+v", config)
	}
	if config.DegradeProxyMode != "custom" || config.DegradeProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("unexpected degrade proxy: %+v", config)
	}
}

func TestParseTurnStateWatcherConfigDisabledAllowsEmptyModel(t *testing.T) {
	t.Parallel()
	config, err := ParseTurnStateWatcherConfig(map[string]any{"enabled": false})
	if err != nil {
		t.Fatalf("disabled config with no model error = %v", err)
	}
	if config.PushModels != "" || config.DegradeProxyMode != "" {
		t.Fatalf("disabled config should stay minimal: %+v", config)
	}
}

func TestParseTurnStateWatcherConfigRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		object map[string]any
	}{
		{"enabled requires model", map[string]any{"enabled": true}},
		{"model with comma", map[string]any{"enabled": true, "push_models": "a,b"}},
		{"model with wildcard", map[string]any{"enabled": true, "push_models": "gpt-*"}},
		{"model not string", map[string]any{"enabled": true, "push_models": 42}},
		{"zero group id", map[string]any{"enabled": true, "push_models": "m", "group_id": 0}},
		{"zero credential id", map[string]any{"enabled": true, "push_models": "m", "credential_id": 0}},
		{"zero push max age", map[string]any{"push_max_age_ms": 0}},
		{"zero poll interval", map[string]any{"poll_interval_seconds": 0}},
		{"negative verify attempts", map[string]any{"verify_max_attempts": -1}},
		{"lengths not array", map[string]any{"healthy_lengths": 292}},
		{"zero length", map[string]any{"degraded_lengths": []any{0}}},
		{"duplicate lengths", map[string]any{"healthy_lengths": []any{292, 292}}},
		{"overlapping lengths", map[string]any{"healthy_lengths": []any{312}, "degraded_lengths": []any{312}}},
		{"unsupported proxy mode", map[string]any{"degrade_proxy_mode": "inherit", "degrade_proxy_url": "http://127.0.0.1:1"}},
		{"mode without url", map[string]any{"degrade_proxy_mode": "custom"}},
		{"invalid proxy url", map[string]any{"degrade_proxy_url": "ftp://127.0.0.1:1"}},
		{"enabled not boolean", map[string]any{"enabled": "yes"}},
		{"unknown field", map[string]any{"nope": true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			config, err := ParseTurnStateWatcherConfig(testCase.object)
			if err == nil {
				t.Fatalf("ParseTurnStateWatcherConfig(%v) = %+v, want error", testCase.object, config)
			}
		})
	}
}

func TestParseTurnStateWatcherConfigNonObject(t *testing.T) {
	t.Parallel()
	if _, err := ParseTurnStateWatcherConfig("enabled"); err == nil {
		t.Fatal("string value must be rejected")
	}
}

func TestTurnStateWatcherConfigEqual(t *testing.T) {
	t.Parallel()
	base := &TurnStateWatcherConfig{
		Enabled: true, PushModels: "m", HealthyLengths: []int{292}, DegradedLengths: []int{312},
	}
	if !base.Equal(&TurnStateWatcherConfig{
		Enabled: true, PushModels: "m", HealthyLengths: []int{292}, DegradedLengths: []int{312},
	}) {
		t.Fatal("identical configs must be equal")
	}
	for name, mutate := range map[string]func(*TurnStateWatcherConfig){
		"enabled":       func(c *TurnStateWatcherConfig) { c.Enabled = false },
		"model":         func(c *TurnStateWatcherConfig) { c.PushModels = "other" },
		"healthy":       func(c *TurnStateWatcherConfig) { c.HealthyLengths = []int{332} },
		"degraded":      func(c *TurnStateWatcherConfig) { c.DegradedLengths = []int{356} },
		"proxy url":     func(c *TurnStateWatcherConfig) { c.DegradeProxyURL = "http://127.0.0.1:1" },
		"poll interval": func(c *TurnStateWatcherConfig) { c.PollIntervalSeconds = 99 },
	} {
		changed := &TurnStateWatcherConfig{
			Enabled: true, PushModels: "m", HealthyLengths: []int{292}, DegradedLengths: []int{312},
		}
		mutate(changed)
		if base.Equal(changed) {
			t.Fatalf("changing %s must break equality", name)
		}
	}
	var nilConfig *TurnStateWatcherConfig
	if !nilConfig.Equal(nil) {
		t.Fatal("two nil configs must be equal")
	}
	if base.Equal(nil) || nilConfig.Equal(base) {
		t.Fatal("nil and non-nil configs must not be equal")
	}
}

func TestResolveRuntimeSettingsTurnStateWatcher(t *testing.T) {
	t.Parallel()
	resolved, err := ResolveRuntimeSettings(map[string]any{})
	if err != nil {
		t.Fatalf("ResolveRuntimeSettings({}) error = %v", err)
	}
	if resolved.TurnStateWatcher != nil {
		t.Fatalf("absent setting must resolve to nil, got %+v", resolved.TurnStateWatcher)
	}
	resolved, err = ResolveRuntimeSettings(map[string]any{
		SettingTurnStateWatcher: map[string]any{"enabled": false},
	})
	if err != nil {
		t.Fatalf("ResolveRuntimeSettings(watcher) error = %v", err)
	}
	if resolved.TurnStateWatcher == nil || resolved.TurnStateWatcher.Enabled {
		t.Fatalf("unexpected watcher config: %+v", resolved.TurnStateWatcher)
	}
	if _, err := ResolveRuntimeSettings(map[string]any{
		SettingTurnStateWatcher: map[string]any{"enabled": true},
	}); err == nil {
		t.Fatal("enabled config without model must fail to resolve")
	}
}
