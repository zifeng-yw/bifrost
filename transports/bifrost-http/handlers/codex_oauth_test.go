package handlers

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
)

func TestCodexOAuthConnectedAfterStatusUpdateClearsDescription(t *testing.T) {
	key := schemas.Key{
		Name:        "renamed OAuth key",
		Value:       *schemas.NewSecretVar(`{"access_token":"access","refresh_token":"refresh"}`),
		Status:      schemas.KeyStatusSuccess,
		Description: "",
	}
	service := codexOAuthServiceForTest(&schemas.OpenAIConfig{CodexOAuth: true}, []schemas.Key{key})

	connected, err := service.connected(schemas.OpenAI)
	if err != nil {
		t.Fatalf("connected returned an error: %v", err)
	}
	if !connected {
		t.Fatal("expected Codex OAuth credentials to remain connected after description was cleared")
	}
}

func TestCodexOAuthConnectedRejectsPlatformAPIKey(t *testing.T) {
	key := schemas.Key{
		Name:  "platform key",
		Value: *schemas.NewSecretVar("sk-platform"),
	}
	service := codexOAuthServiceForTest(&schemas.OpenAIConfig{CodexOAuth: true}, []schemas.Key{key})

	connected, err := service.connected(schemas.OpenAI)
	if err != nil {
		t.Fatalf("connected returned an error: %v", err)
	}
	if connected {
		t.Fatal("expected a Platform API key not to be treated as Codex OAuth credentials")
	}
}

func codexOAuthServiceForTest(openAIConfig *schemas.OpenAIConfig, keys []schemas.Key) *codexOAuthWebService {
	return &codexOAuthWebService{
		handler: &ProviderHandler{
			inMemoryStore: &lib.Config{
				Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
					schemas.OpenAI: {
						Keys:         keys,
						OpenAIConfig: openAIConfig,
					},
				},
			},
		},
	}
}
