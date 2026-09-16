package agent

import (
	"context"
	"log/slog"
	"time"
)

func discoverMuseModels(ctx context.Context, command Command) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// model/list requires no session and makes no inference request. Do not
	// trust a workspace or create a durable session merely to populate a picker.
	c, closeHost, err := startMuseHost(Config{ExecutablePath: command.Path, LaunchPrefix: filterLaunchPrefix(command.Prefix, "muse", slog.Default()), Logger: slog.Default(), provider: "muse"}, []string{"serve"}, "", 0)
	if err != nil {
		return nil, err
	}
	defer closeHost()
	if err := c.initialize(ctx, ""); err != nil {
		return nil, err
	}
	var result struct {
		Models []struct {
			ID       string `json:"modelId"`
			Label    string `json:"displayLabel"`
			Provider string `json:"providerId"`
			Default  bool   `json:"isDefault"`
		} `json:"models"`
	}
	if err := c.call(ctx, "model/list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(result.Models))
	for _, m := range result.Models {
		if m.ID == "" {
			continue
		}
		label := m.Label
		if label == "" {
			label = m.ID
		}
		// The effort vocabulary belongs to the stable MSP turn/start schema.
		thinking := &ModelThinking{}
		for _, value := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
			thinking.SupportedLevels = append(thinking.SupportedLevels, ThinkingLevel{Value: value, Label: value})
		}
		models = append(models, Model{ID: m.ID, Label: label, Provider: m.Provider, Default: m.Default, Thinking: thinking})
	}
	return models, nil
}
