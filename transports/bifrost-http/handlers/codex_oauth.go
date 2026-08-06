package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	openaiProvider "github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

const (
	codexOAuthUserCodeURL       = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	codexOAuthDeviceTokenURL    = "https://auth.openai.com/api/accounts/deviceauth/token"
	codexOAuthVerificationURL   = "https://auth.openai.com/codex/device"
	codexOAuthDeviceRedirectURI = "https://auth.openai.com/deviceauth/callback"
	codexOAuthFlowLifetime      = 15 * time.Minute
)

type codexOAuthFlowStatus string

const (
	codexOAuthFlowPending  codexOAuthFlowStatus = "pending"
	codexOAuthFlowComplete codexOAuthFlowStatus = "complete"
	codexOAuthFlowFailed   codexOAuthFlowStatus = "failed"
)

type codexOAuthWebService struct {
	handler *ProviderHandler
	client  *http.Client
	mu      sync.RWMutex
	flows   map[string]*codexOAuthFlow
}

type codexOAuthFlow struct {
	Provider     schemas.ModelProvider
	DeviceAuthID string
	UserCode     string
	Interval     time.Duration
	ExpiresAt    time.Time
	Status       codexOAuthFlowStatus
	Error        string
}

type codexOAuthStartResponse struct {
	FlowID          string `json:"flow_id"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
}

type codexOAuthFlowResponse struct {
	Status codexOAuthFlowStatus `json:"status"`
	Error  string               `json:"error,omitempty"`
}

type codexOAuthConnectionResponse struct {
	Connected bool `json:"connected"`
}

type codexOAuthUserCodeResponse struct {
	DeviceAuthID string          `json:"device_auth_id"`
	UserCode     string          `json:"user_code"`
	Interval     json.RawMessage `json:"interval"`
}

type codexOAuthDeviceTokenResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
	Error             any    `json:"error"`
}

type codexOAuthTokenExchangeResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func newCodexOAuthWebService(handler *ProviderHandler) *codexOAuthWebService {
	return &codexOAuthWebService{
		handler: handler,
		client:  &http.Client{Timeout: 30 * time.Second},
		flows:   make(map[string]*codexOAuthFlow),
	}
}

func (h *ProviderHandler) startCodexOAuth(ctx *fasthttp.RequestCtx) {
	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}
	response, err := h.codexOAuth.start(ctx, provider)
	if err != nil {
		SendError(ctx, codexOAuthHTTPStatus(err), err.Error())
		return
	}
	SendJSONWithStatus(ctx, response, fasthttp.StatusAccepted)
}

func (h *ProviderHandler) getCodexOAuthFlow(ctx *fasthttp.RequestCtx) {
	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}
	flowID, ok := ctx.UserValue("flow_id").(string)
	if !ok || strings.TrimSpace(flowID) == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "Missing OAuth flow ID")
		return
	}
	response, err := h.codexOAuth.flowStatus(provider, flowID)
	if err != nil {
		SendError(ctx, codexOAuthHTTPStatus(err), err.Error())
		return
	}
	SendJSON(ctx, response)
}

func (h *ProviderHandler) getCodexOAuthConnection(ctx *fasthttp.RequestCtx) {
	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}
	connected, err := h.codexOAuth.connected(provider)
	if err != nil {
		SendError(ctx, codexOAuthHTTPStatus(err), err.Error())
		return
	}
	SendJSON(ctx, codexOAuthConnectionResponse{Connected: connected})
}

func (h *ProviderHandler) disconnectCodexOAuth(ctx *fasthttp.RequestCtx) {
	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}
	if err := h.codexOAuth.disconnect(ctx, provider); err != nil {
		SendError(ctx, codexOAuthHTTPStatus(err), err.Error())
		return
	}
	SendJSON(ctx, codexOAuthConnectionResponse{Connected: false})
}

func (service *codexOAuthWebService) start(ctx context.Context, provider schemas.ModelProvider) (*codexOAuthStartResponse, error) {
	config, err := service.validateProvider(provider)
	if err != nil {
		return nil, err
	}
	for _, key := range config.Keys {
		if isCodexOAuthManagedKey(key) {
			return nil, fmt.Errorf("Codex OAuth is already connected")
		}
		return nil, fmt.Errorf("Codex OAuth requires a provider without Platform API keys; use a separate OpenAI provider")
	}

	request := map[string]string{"client_id": openaiProvider.CodexOAuthClientID}
	response := codexOAuthUserCodeResponse{}
	status, err := service.postJSON(ctx, codexOAuthUserCodeURL, request, &response)
	if err != nil {
		return nil, fmt.Errorf("start Codex OAuth: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices || response.DeviceAuthID == "" || response.UserCode == "" {
		return nil, fmt.Errorf("OpenAI rejected the Codex OAuth start request")
	}
	interval := parseCodexOAuthInterval(response.Interval)
	flowID := uuid.NewString()
	flow := &codexOAuthFlow{
		Provider:     provider,
		DeviceAuthID: response.DeviceAuthID,
		UserCode:     response.UserCode,
		Interval:     interval,
		ExpiresAt:    time.Now().Add(codexOAuthFlowLifetime),
		Status:       codexOAuthFlowPending,
	}
	service.mu.Lock()
	service.removeExpiredFlowsLocked()
	for _, existing := range service.flows {
		if existing.Provider == provider && existing.Status == codexOAuthFlowPending {
			service.mu.Unlock()
			return nil, fmt.Errorf("Codex OAuth connection is already in progress")
		}
	}
	service.flows[flowID] = flow
	service.mu.Unlock()
	go service.poll(flowID)

	return &codexOAuthStartResponse{
		FlowID:          flowID,
		UserCode:        flow.UserCode,
		VerificationURI: codexOAuthVerificationURL,
		Interval:        int(flow.Interval / time.Second),
	}, nil
}

func (service *codexOAuthWebService) poll(flowID string) {
	service.mu.RLock()
	flow := service.flows[flowID]
	service.mu.RUnlock()
	if flow == nil {
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), flow.ExpiresAt)
	defer cancel()
	ticker := time.NewTicker(flow.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			service.failFlow(flowID, "Codex OAuth authorization expired")
			return
		case <-ticker.C:
			credentials, pending, err := service.pollOnce(ctx, flow)
			if pending {
				continue
			}
			if err != nil {
				service.failFlow(flowID, err.Error())
				return
			}
			if err := service.connect(ctx, flow.Provider, credentials); err != nil {
				service.failFlow(flowID, fmt.Sprintf("save Codex OAuth credentials: %v", err))
				return
			}
			service.mu.Lock()
			if current := service.flows[flowID]; current != nil {
				current.Status = codexOAuthFlowComplete
				current.DeviceAuthID = ""
			}
			service.mu.Unlock()
			return
		}
	}
}

func (service *codexOAuthWebService) pollOnce(ctx context.Context, flow *codexOAuthFlow) (schemas.CodexOAuthCredentials, bool, error) {
	deviceResponse := codexOAuthDeviceTokenResponse{}
	status, err := service.postJSON(ctx, codexOAuthDeviceTokenURL, map[string]string{
		"device_auth_id": flow.DeviceAuthID,
		"user_code":      flow.UserCode,
	}, &deviceResponse)
	if err != nil {
		return schemas.CodexOAuthCredentials{}, false, fmt.Errorf("poll Codex OAuth: %w", err)
	}
	if status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusTooManyRequests {
		return schemas.CodexOAuthCredentials{}, true, nil
	}
	errorCode := codexOAuthErrorCode(deviceResponse.Error)
	if errorCode == "deviceauth_authorization_pending" || errorCode == "authorization_pending" || errorCode == "slow_down" {
		return schemas.CodexOAuthCredentials{}, true, nil
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices || deviceResponse.AuthorizationCode == "" || deviceResponse.CodeVerifier == "" {
		return schemas.CodexOAuthCredentials{}, false, fmt.Errorf("OpenAI rejected the Codex OAuth authorization")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", openaiProvider.CodexOAuthClientID)
	form.Set("code", deviceResponse.AuthorizationCode)
	form.Set("code_verifier", deviceResponse.CodeVerifier)
	form.Set("redirect_uri", codexOAuthDeviceRedirectURI)
	tokenResponse := codexOAuthTokenExchangeResponse{}
	status, err = service.postForm(ctx, openaiProvider.CodexOAuthTokenURL, form, &tokenResponse)
	if err != nil {
		return schemas.CodexOAuthCredentials{}, false, fmt.Errorf("exchange Codex OAuth code: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices || tokenResponse.AccessToken == "" || tokenResponse.RefreshToken == "" {
		return schemas.CodexOAuthCredentials{}, false, fmt.Errorf("OpenAI rejected the Codex OAuth token exchange")
	}
	return schemas.CodexOAuthCredentials{AccessToken: tokenResponse.AccessToken, RefreshToken: tokenResponse.RefreshToken}, false, nil
}

func (service *codexOAuthWebService) connect(ctx context.Context, provider schemas.ModelProvider, credentials schemas.CodexOAuthCredentials) error {
	_, err := service.validateProvider(provider)
	if err != nil {
		return err
	}
	value, err := sonic.MarshalString(credentials)
	if err != nil {
		return err
	}
	enabled := true
	key := schemas.Key{
		ID:      uuid.NewString(),
		Name:    "Codex OAuth (" + string(provider) + ")",
		Value:   *schemas.NewSecretVar(value),
		Models:  schemas.WhiteList{"*"},
		Weight:  1,
		Enabled: &enabled,
	}
	if err := service.handler.inMemoryStore.AddProviderKey(ctx, provider, key); err != nil {
		return err
	}
	updatedConfig, err := service.handler.inMemoryStore.GetProviderConfigRaw(provider)
	if err != nil {
		_ = service.handler.inMemoryStore.RemoveProviderKey(ctx, provider, key.ID)
		return err
	}
	openAIConfig := &schemas.OpenAIConfig{}
	if updatedConfig.OpenAIConfig != nil {
		*openAIConfig = *updatedConfig.OpenAIConfig
	}
	openAIConfig.DisableStore = true
	openAIConfig.CodexOAuth = true
	updatedConfig.OpenAIConfig = openAIConfig
	if updatedConfig.CustomProviderConfig != nil {
		customProviderConfig := *updatedConfig.CustomProviderConfig
		customProviderConfig.IsKeyLess = false
		updatedConfig.CustomProviderConfig = &customProviderConfig
	}
	networkConfig := schemas.DefaultNetworkConfig
	if updatedConfig.NetworkConfig != nil {
		networkConfig = *updatedConfig.NetworkConfig
	}
	networkConfig.BaseURL = openaiProvider.ChatGPTBackendBaseURL
	updatedConfig.NetworkConfig = &networkConfig
	if err := service.handler.inMemoryStore.UpdateProviderConfig(ctx, provider, *updatedConfig); err != nil {
		_ = service.handler.inMemoryStore.RemoveProviderKey(ctx, provider, key.ID)
		return err
	}
	if _, err := service.handler.modelsManager.ReloadProvider(ctx, provider); err != nil {
		logger.Warn("Catalog refresh failed after Codex OAuth connection for provider %s: %v", provider, err)
	}
	return nil
}

func isCodexOAuthManagedKey(key schemas.Key) bool {
	credentials := schemas.CodexOAuthCredentials{}
	if err := sonic.UnmarshalString(key.Value.GetValue(), &credentials); err != nil {
		return false
	}
	return strings.TrimSpace(credentials.AccessToken) != "" || strings.TrimSpace(credentials.RefreshToken) != ""
}

func (service *codexOAuthWebService) connected(provider schemas.ModelProvider) (bool, error) {
	config, err := service.validateProvider(provider)
	if err != nil {
		return false, err
	}
	if config.OpenAIConfig == nil || !config.OpenAIConfig.CodexOAuth {
		return false, nil
	}
	for _, key := range config.Keys {
		if isCodexOAuthManagedKey(key) {
			return true, nil
		}
	}
	return false, nil
}

func (service *codexOAuthWebService) disconnect(ctx context.Context, provider schemas.ModelProvider) error {
	config, err := service.validateProvider(provider)
	if err != nil {
		return err
	}
	for _, key := range config.Keys {
		if isCodexOAuthManagedKey(key) {
			if err := service.handler.inMemoryStore.RemoveProviderKey(ctx, provider, key.ID); err != nil && !errors.Is(err, lib.ErrNotFound) {
				return err
			}
			if err := service.handler.modelsManager.OnKeyDeleted(ctx, provider, key.ID); err != nil {
				logger.Warn("Catalog refresh failed after Codex OAuth disconnection for provider %s: %v", provider, err)
			}
			break
		}
	}
	updatedConfig, err := service.handler.inMemoryStore.GetProviderConfigRaw(provider)
	if err != nil {
		return err
	}
	if updatedConfig.OpenAIConfig != nil {
		openAIConfig := *updatedConfig.OpenAIConfig
		openAIConfig.CodexOAuth = false
		updatedConfig.OpenAIConfig = &openAIConfig
		if err := service.handler.inMemoryStore.UpdateProviderConfig(ctx, provider, *updatedConfig); err != nil {
			return err
		}
	}
	return nil
}

func (service *codexOAuthWebService) flowStatus(provider schemas.ModelProvider, flowID string) (*codexOAuthFlowResponse, error) {
	service.mu.RLock()
	flow := service.flows[flowID]
	if flow == nil || flow.Provider != provider {
		service.mu.RUnlock()
		return nil, lib.ErrNotFound
	}
	response := &codexOAuthFlowResponse{
		Status: flow.Status,
		Error:  flow.Error,
	}
	service.mu.RUnlock()
	return response, nil
}

func (service *codexOAuthWebService) validateProvider(provider schemas.ModelProvider) (*configstore.ProviderConfig, error) {
	config, err := service.handler.inMemoryStore.GetProviderConfigRaw(provider)
	if err != nil {
		return nil, err
	}
	baseProvider := provider
	if config.CustomProviderConfig != nil {
		baseProvider = config.CustomProviderConfig.BaseProviderType
	}
	if baseProvider != schemas.OpenAI {
		return nil, fmt.Errorf("Codex OAuth is only available for OpenAI providers")
	}
	return config, nil
}

func (service *codexOAuthWebService) failFlow(flowID, message string) {
	service.mu.Lock()
	if flow := service.flows[flowID]; flow != nil && flow.Status == codexOAuthFlowPending {
		flow.Status = codexOAuthFlowFailed
		flow.Error = message
		flow.DeviceAuthID = ""
	}
	service.mu.Unlock()
}

func (service *codexOAuthWebService) removeExpiredFlowsLocked() {
	cutoff := time.Now().Add(-time.Hour)
	for id, flow := range service.flows {
		if flow.ExpiresAt.Before(cutoff) {
			delete(service.flows, id)
		}
	}
}

func (service *codexOAuthWebService) postJSON(ctx context.Context, endpoint string, body any, output any) (int, error) {
	payload, err := sonic.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	return service.do(req, output)
}

func (service *codexOAuthWebService) postForm(ctx context.Context, endpoint string, form url.Values, output any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return service.do(req, output)
}

func (service *codexOAuthWebService) do(req *http.Request, output any) (int, error) {
	response, err := service.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response.StatusCode, err
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, output); err != nil && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			return response.StatusCode, err
		}
	}
	return response.StatusCode, nil
}

func codexOAuthErrorCode(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		code, _ := typed["code"].(string)
		return code
	default:
		return ""
	}
}

func parseCodexOAuthInterval(raw json.RawMessage) time.Duration {
	seconds := 5
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &seconds); err != nil {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				if parsed, parseErr := strconv.Atoi(value); parseErr == nil {
					seconds = parsed
				}
			}
		}
	}
	if seconds < 1 {
		seconds = 1
	}
	return time.Duration(seconds) * time.Second
}

func codexOAuthHTTPStatus(err error) int {
	if errors.Is(err, lib.ErrNotFound) {
		return fasthttp.StatusNotFound
	}
	message := err.Error()
	if strings.Contains(message, "already connected") || strings.Contains(message, "already in progress") {
		return fasthttp.StatusConflict
	}
	if strings.Contains(message, "only available") || strings.Contains(message, "requires a provider") {
		return fasthttp.StatusBadRequest
	}
	return fasthttp.StatusBadGateway
}
