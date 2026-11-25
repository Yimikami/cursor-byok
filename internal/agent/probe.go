package agent

import (
	"context"
	"errors"
	"strings"
)

// ProbeAdapter checks the same streaming transport used by real agent turns.
// A 2xx status or an empty event stream alone does not establish inference.
func ProbeAdapter(ctx context.Context, target AdapterTarget) (string, error) {
	text, err := collectSingleResponse(ctx, pickProviderStreamer(target.ProviderType),
		target.BaseURL, target.APIKey, target.Model, target.Opts,
		[]openAIMessage{textMessage("user", "Reply with OK only.")})
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("provider returned no response text")
	}
	return text, nil
}
