package openai

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const (
	// ChatGPTBaseURL and ChatGPTBackendBaseURL are shared by Codex OAuth and ChatGPT passthrough routes.
	ChatGPTBaseURL        = "https://chatgpt.com"
	ChatGPTBackendBaseURL = ChatGPTBaseURL + "/backend-api"
	CodexOAuthTokenURL    = "https://auth.openai.com/oauth/token"
	CodexOAuthClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"

	codexOAuthCompatibilityVersion = "0.144.4"
)

type codexOAuthManager struct {
	client          *fasthttp.Client
	logger          schemas.Logger
	provider        schemas.ModelProvider
	credentialStore schemas.CodexOAuthCredentialStore
	mu              sync.Mutex
	tokens          map[string]*codexOAuthToken
}

type codexOAuthToken struct {
	mu           sync.Mutex
	initialized  bool
	accessToken  string
	refreshToken string
	accountID    string
	expiresAt    time.Time
}

type codexOAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type codexOAuthClaims struct {
	ExpiresAt int64 `json:"exp"`
	Auth      struct {
		AccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
}

type codexListModelsResponse struct {
	Models []struct {
		Slug string `json:"slug"`
	} `json:"models"`
}

func prepareCodexOAuthResponsesRequest(request *OpenAIResponsesRequest) *OpenAIResponsesRequest {
	if request != nil {
		request.MaxOutputTokens = nil
	}
	return request
}

func newCodexOAuthManager(client *fasthttp.Client, logger schemas.Logger, provider schemas.ModelProvider, credentialStore schemas.CodexOAuthCredentialStore) *codexOAuthManager {
	return &codexOAuthManager{
		client:          client,
		logger:          logger,
		provider:        provider,
		credentialStore: credentialStore,
		tokens:          make(map[string]*codexOAuthToken),
	}
}

func (provider *OpenAIProvider) chatGPTHeaders(ctx *schemas.BifrostContext, key schemas.Key, includeResponsesBeta bool) (map[string]string, *schemas.BifrostError) {
	if provider.codexOAuth == nil {
		headers := BearerAuthHeader(key)
		if includeResponsesBeta {
			headers["OpenAI-Beta"] = "responses=experimental"
		}
		return headers, nil
	}
	headers, bifrostErr := provider.codexOAuth.authHeaders(ctx, key)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if includeResponsesBeta {
		headers["OpenAI-Beta"] = "responses=experimental"
	}
	return headers, nil
}

func (provider *OpenAIProvider) responsesAuthHeaders(ctx *schemas.BifrostContext, key schemas.Key) (map[string]string, *schemas.BifrostError) {
	if provider.codexOAuth == nil {
		return BearerAuthHeader(key), nil
	}
	return provider.chatGPTHeaders(ctx, key, true)
}

func (manager *codexOAuthManager) authHeaders(ctx *schemas.BifrostContext, key schemas.Key) (map[string]string, *schemas.BifrostError) {
	rawCredential := strings.TrimSpace(key.Value.GetValue())
	if rawCredential == "" {
		return nil, providerUtils.NewConfigurationError("Codex OAuth credentials are empty")
	}

	cacheKey := key.ID
	if cacheKey == "" {
		cacheKey = rawCredential
	}
	manager.mu.Lock()
	token := manager.tokens[cacheKey]
	if token == nil {
		token = &codexOAuthToken{}
		manager.tokens[cacheKey] = token
	}
	manager.mu.Unlock()

	token.mu.Lock()
	defer token.mu.Unlock()
	if !token.initialized {
		if bifrostErr := token.initialize(rawCredential); bifrostErr != nil {
			return nil, bifrostErr
		}
	}

	if token.accessToken == "" || (!token.expiresAt.IsZero() && time.Until(token.expiresAt) <= time.Minute) {
		if token.refreshToken == "" {
			return nil, providerUtils.NewConfigurationError("Codex OAuth access token expired and no refresh token was configured")
		}
		if bifrostErr := manager.refresh(ctx, token, key.ID); bifrostErr != nil {
			return nil, bifrostErr
		}
	}
	if token.accountID == "" {
		return nil, providerUtils.NewConfigurationError("Codex OAuth access token does not contain chatgpt_account_id")
	}

	return map[string]string{
		"Authorization":      "Bearer " + token.accessToken,
		"chatgpt-account-id": token.accountID,
		"originator":         "bifrost",
	}, nil
}

func (token *codexOAuthToken) initialize(rawCredential string) *schemas.BifrostError {
	credentials := schemas.CodexOAuthCredentials{}
	if err := sonic.UnmarshalString(rawCredential, &credentials); err != nil {
		return providerUtils.NewConfigurationError("Codex OAuth key value must contain credentials JSON")
	}

	token.accessToken = strings.TrimSpace(credentials.AccessToken)
	token.refreshToken = strings.TrimSpace(credentials.RefreshToken)
	if token.accessToken == "" && token.refreshToken == "" {
		return providerUtils.NewConfigurationError("Codex OAuth credentials are empty")
	}
	if token.accessToken != "" {
		claims, err := parseCodexOAuthClaims(token.accessToken)
		if (err != nil || claims.Auth.AccountID == "") && token.refreshToken != "" {
			token.accessToken = ""
			token.expiresAt = time.Time{}
		} else if err != nil {
			return providerUtils.NewConfigurationError("Codex OAuth access token is not a valid JWT")
		} else if claims.Auth.AccountID == "" {
			return providerUtils.NewConfigurationError("Codex OAuth access token does not contain chatgpt_account_id")
		} else {
			token.accountID = claims.Auth.AccountID
			if claims.ExpiresAt > 0 {
				token.expiresAt = time.Unix(claims.ExpiresAt, 0)
			}
		}
	}
	token.initialized = true
	return nil
}

func (manager *codexOAuthManager) refresh(ctx *schemas.BifrostContext, token *codexOAuthToken, keyID string) *schemas.BifrostError {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", token.refreshToken)
	form.Set("client_id", CodexOAuthClientID)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(CodexOAuthTokenURL)
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBodyString(form.Encode())

	_, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, manager.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return bifrostErr
	}
	if resp.StatusCode() < fasthttp.StatusOK || resp.StatusCode() >= fasthttp.StatusMultipleChoices {
		statusCode := resp.StatusCode()
		return &schemas.BifrostError{
			IsBifrostError: false,
			StatusCode:     &statusCode,
			Error: &schemas.ErrorField{
				Message: fmt.Sprintf("Codex OAuth token refresh failed with status %d", statusCode),
			},
		}
	}

	response := codexOAuthTokenResponse{}
	if err := sonic.Unmarshal(resp.Body(), &response); err != nil || strings.TrimSpace(response.AccessToken) == "" {
		return providerUtils.NewBifrostOperationError("Codex OAuth token refresh returned an invalid response", err)
	}
	claims, err := parseCodexOAuthClaims(response.AccessToken)
	if err != nil || claims.Auth.AccountID == "" {
		return providerUtils.NewBifrostOperationError("Codex OAuth access token is missing required claims", err)
	}

	token.accessToken = strings.TrimSpace(response.AccessToken)
	if strings.TrimSpace(response.RefreshToken) != "" {
		token.refreshToken = strings.TrimSpace(response.RefreshToken)
	}
	token.accountID = claims.Auth.AccountID
	if response.ExpiresIn > 0 {
		token.expiresAt = time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
	} else if claims.ExpiresAt > 0 {
		token.expiresAt = time.Unix(claims.ExpiresAt, 0)
	} else {
		token.expiresAt = time.Time{}
	}
	if manager.credentialStore != nil && keyID != "" {
		persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.credentialStore.StoreCodexOAuthCredentials(persistCtx, manager.provider, keyID, schemas.CodexOAuthCredentials{
			AccessToken:  token.accessToken,
			RefreshToken: token.refreshToken,
		}); err != nil && manager.logger != nil {
			manager.logger.Warn("Failed to persist rotated Codex OAuth credentials: %v", err)
		}
	}
	return nil
}

func parseCodexOAuthClaims(accessToken string) (*codexOAuthClaims, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT payload: %w", err)
	}
	claims := &codexOAuthClaims{}
	if err := sonic.Unmarshal(payload, claims); err != nil {
		return nil, fmt.Errorf("decode JWT claims: %w", err)
	}
	return claims, nil
}

func (provider *OpenAIProvider) listCodexModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	listModelsByKey := func(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
		authHeaders, bifrostErr := provider.chatGPTHeaders(ctx, key, false)
		if bifrostErr != nil {
			return nil, bifrostErr
		}

		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		modelsPath := "/codex/models?client_version=" + url.QueryEscape(codexOAuthCompatibilityVersion)
		req.SetRequestURI(provider.buildRequestURL(ctx, modelsPath, schemas.ListModelsRequest))
		req.Header.SetMethod(http.MethodGet)
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		for header, value := range authHeaders {
			req.Header.Set(header, value)
		}

		latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		defer wait()
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
		ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerResponseHeaders)
		if resp.StatusCode() != fasthttp.StatusOK {
			return nil, providerUtils.SetErrorLatency(ParseOpenAIError(resp), latency)
		}

		codexResponse := codexListModelsResponse{}
		if err := sonic.Unmarshal(resp.Body(), &codexResponse); err != nil {
			return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err), latency)
		}
		openAIResponse := OpenAIListModelsResponse{Data: make([]OpenAIModel, 0, len(codexResponse.Models))}
		for _, model := range codexResponse.Models {
			if model.Slug != "" {
				openAIResponse.Data = append(openAIResponse.Data, OpenAIModel{ID: model.Slug, Object: "model"})
			}
		}
		response := openAIResponse.ToBifrostListModelsResponse(provider.GetProviderKey(), key.Models, key.BlacklistedModels, key.Aliases, request.Unfiltered)
		response.ExtraFields.Latency = latency.Milliseconds()
		response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
		if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
			var rawResponse any
			if err := sonic.Unmarshal(resp.Body(), &rawResponse); err == nil {
				response.ExtraFields.RawResponse = rawResponse
			}
		}
		return response, nil
	}
	return providerUtils.HandleMultipleListModelsRequests(ctx, keys, request, listModelsByKey)
}
