package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type aliyunCompatibleHTTPUpstreamStub struct {
	HTTPUpstream
	request *http.Request
}

type aliyunCompatibleAccountRepositoryStub struct {
	AccountRepository
}

func (s *aliyunCompatibleHTTPUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	s.request = req
	return &http.Response{
		StatusCode: http.StatusCreated,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"file-test"}`)),
	}, nil
}

func TestAliyunOpenAICompatibleBaseURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{
			name: "official dashscope",
			base: "https://dashscope.aliyuncs.com",
			want: "https://dashscope.aliyuncs.com/compatible-mode",
		},
		{
			name: "workspace endpoint",
			base: "https://workspace.cn-beijing.maas.aliyuncs.com/",
			want: "https://workspace.cn-beijing.maas.aliyuncs.com/compatible-mode",
		},
		{
			name: "already compatible",
			base: "https://dashscope.aliyuncs.com/compatible-mode/v1",
			want: "https://dashscope.aliyuncs.com/compatible-mode/v1",
		},
		{
			name: "model router",
			base: "http://model-router:10030/",
			want: "http://model-router:10030",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, aliyunOpenAICompatibleBaseURL(tt.base))
		})
	}
}

func TestAliyunAccountParticipatesInOpenAICompatibleGateway(t *testing.T) {
	account := &Account{
		Platform: PlatformAliyun,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-aliyun-test",
			"base_url": "http://model-router:10030",
		},
	}

	require.True(t, account.IsOpenAICompatible())
	require.Equal(t, "sk-aliyun-test", account.GetOpenAIApiKey())
	require.Equal(t, "http://model-router:10030", account.GetOpenAIBaseURL())
	require.Equal(t, PlatformAliyun, normalizeOpenAICompatiblePlatform(PlatformAliyun))
}

func TestAliyunAccountIsEligibleOnlyForAliyunScheduling(t *testing.T) {
	account := &Account{
		Platform:    PlatformAliyun,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-aliyun-test"},
	}

	require.True(t, isOpenAICompatibleAccountEligibleForRequest(
		context.Background(),
		account,
		PlatformAliyun,
		"qwen-plus",
		false,
		OpenAIEndpointCapabilityChatCompletions,
	))
	require.False(t, isOpenAICompatibleAccountEligibleForRequest(
		context.Background(),
		account,
		PlatformOpenAI,
		"qwen-plus",
		false,
		OpenAIEndpointCapabilityChatCompletions,
	))
}

func TestAliyunFilesGatewaySupportsOpenAIAndAliyunGroups(t *testing.T) {
	require.True(t, SupportsAliyunFilesGatewayPlatform(PlatformOpenAI))
	require.True(t, SupportsAliyunFilesGatewayPlatform(PlatformAliyun))
	require.False(t, SupportsAliyunFilesGatewayPlatform(PlatformAnthropic))
	require.False(t, SupportsAliyunFilesGatewayPlatform(PlatformGemini))
	require.False(t, SupportsAliyunFilesGatewayPlatform(PlatformGrok))
}

func TestAliyunDefaultModelCandidates(t *testing.T) {
	candidates := defaultModelsListCandidateIDs(PlatformAliyun)
	require.ElementsMatch(t, []string{
		"qwen-long",
		"qwen-plus",
		AliyunModelTTS,
		AliyunModelASR,
		AliyunModelVoiceEnrollment,
		AliyunModelMultiModalEmbedding,
	}, candidates)
}

func TestAliyunNativeGatewayDoesNotGateByGroupPlatform(t *testing.T) {
	service := &AliyunGatewayService{
		accountRepo:    &aliyunCompatibleAccountRepositoryStub{},
		gatewayService: &GatewayService{},
		billingService: &BillingService{},
		resolver:       &ModelPricingResolver{},
		httpUpstream:   &aliyunCompatibleHTTPUpstreamStub{},
	}
	apiKey := &APIKey{
		User:  &User{ID: 10},
		Group: &Group{ID: 20, Platform: PlatformOpenAI},
	}

	_, err := service.Forward(context.Background(), AliyunGatewayRequest{
		Method: http.MethodPost,
		Path:   "/api/v1/not-supported",
		APIKey: apiKey,
	})

	var gatewayErr *AliyunGatewayError
	require.ErrorAs(t, err, &gatewayErr)
	require.Equal(t, "ENDPOINT_NOT_SUPPORTED", gatewayErr.Code)
}

func TestAliyunFilesForwardUsesOpenAICompatibleAccountCredentials(t *testing.T) {
	upstream := &aliyunCompatibleHTTPUpstreamStub{}
	service := &AliyunGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          42,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 3,
		Credentials: map[string]any{
			"api_key":  "sk-dashscope-test",
			"base_url": "http://model-router:10030/v1",
		},
	}

	result, err := service.forwardToUpstream(context.Background(), account, AliyunGatewayRequest{
		Method:  http.MethodPost,
		Path:    AliyunEndpointFiles,
		Headers: http.Header{"Content-Type": []string{"multipart/form-data; boundary=test"}},
		Body:    []byte("multipart-body"),
	})

	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, result.StatusCode)
	require.NotNil(t, upstream.request)
	require.Equal(t, "http://model-router:10030/v1/files", upstream.request.URL.String())
	require.Equal(t, "Bearer sk-dashscope-test", upstream.request.Header.Get("Authorization"))
	require.Equal(t, "multipart/form-data; boundary=test", upstream.request.Header.Get("Content-Type"))
	forwardedBody, err := io.ReadAll(upstream.request.Body)
	require.NoError(t, err)
	require.Equal(t, []byte("multipart-body"), forwardedBody)
}

func TestAliyunFilesForwardKeepsAliyunAccountCompatibility(t *testing.T) {
	upstream := &aliyunCompatibleHTTPUpstreamStub{}
	service := &AliyunGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          43,
		Platform:    PlatformAliyun,
		Type:        AccountTypeAPIKey,
		Concurrency: 2,
		Credentials: map[string]any{
			"api_key":  "sk-aliyun-test",
			"base_url": "https://dashscope.aliyuncs.com",
		},
	}

	result, err := service.forwardToUpstream(context.Background(), account, AliyunGatewayRequest{
		Method: http.MethodPost,
		Path:   AliyunEndpointFiles,
		Body:   []byte("multipart-body"),
	})

	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, result.StatusCode)
	require.NotNil(t, upstream.request)
	require.Equal(t, "https://dashscope.aliyuncs.com/compatible-mode/v1/files", upstream.request.URL.String())
	require.Equal(t, "Bearer sk-aliyun-test", upstream.request.Header.Get("Authorization"))
}

func TestAliyunNativeForwardUsesOpenAICompatibleAccountCredentials(t *testing.T) {
	upstream := &aliyunCompatibleHTTPUpstreamStub{}
	service := &AliyunGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          44,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 2,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "http://model-router:10030/v1",
		},
	}

	result, err := service.forwardToUpstream(context.Background(), account, AliyunGatewayRequest{
		Method: http.MethodPost,
		Path:   AliyunEndpointTTS,
		Body:   []byte(`{"model":"cosyvoice-v3-flash"}`),
	})

	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, result.StatusCode)
	require.NotNil(t, upstream.request)
	require.Equal(t, "http://model-router:10030/api/v1/services/audio/tts/SpeechSynthesizer", upstream.request.URL.String())
	require.Equal(t, "Bearer sk-test", upstream.request.Header.Get("Authorization"))
}

func TestAliyunPassthroughAndCanonicalEmbeddingSpecs(t *testing.T) {
	service := &AliyunGatewayService{}

	files, err := service.resolveRequestSpec(AliyunEndpointFiles, nil)
	require.NoError(t, err)
	require.Equal(t, "passthrough", files.kind)
	require.Equal(t, "qwen-long", files.model)

	uploads, err := service.resolveRequestSpec(AliyunEndpointUploads, nil)
	require.NoError(t, err)
	require.Equal(t, "passthrough", uploads.kind)

	embedding, err := service.resolveRequestSpec(
		AliyunEndpointCanonicalEmbedding,
		[]byte(`{"model":"qwen3-vl-embedding"}`),
	)
	require.NoError(t, err)
	require.Equal(t, "embedding", embedding.kind)
	require.Equal(t, AliyunModelMultiModalEmbedding, embedding.model)
}

func TestAliyunOfficialFilesURLUsesCompatibleMode(t *testing.T) {
	baseURL := aliyunOpenAICompatibleBaseURL("https://dashscope.aliyuncs.com")
	target, err := buildAliyunTargetURL(baseURL, AliyunEndpointFiles, "")
	require.NoError(t, err)
	require.Equal(t, "https://dashscope.aliyuncs.com/compatible-mode/v1/files", target)
}

func TestAliyunFilesURLNormalizesOpenAICompatibleBases(t *testing.T) {
	tests := []struct {
		name string
		base string
	}{
		{name: "bare router root", base: "http://model-router:10030"},
		{name: "versioned router root", base: "http://model-router:10030/v1"},
		{name: "complete files endpoint", base: "http://model-router:10030/v1/files"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := buildAliyunTargetURL(tt.base, AliyunEndpointFiles, "purpose=file-extract")
			require.NoError(t, err)
			require.Equal(t, "http://model-router:10030/v1/files?purpose=file-extract", target)
		})
	}
}

func TestAliyunNativeURLNormalizesOpenAICompatibleBases(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{name: "bare router root", base: "http://model-router:10030", want: "http://model-router:10030"},
		{name: "versioned router root", base: "http://model-router:10030/v1", want: "http://model-router:10030"},
		{name: "chat endpoint", base: "http://model-router:10030/v1/chat/completions", want: "http://model-router:10030"},
		{name: "responses endpoint", base: "http://model-router:10030/v1/responses", want: "http://model-router:10030"},
		{name: "official compatible endpoint", base: "https://dashscope.aliyuncs.com/compatible-mode/v1", want: "https://dashscope.aliyuncs.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, aliyunNativeBaseURL(tt.base))
			target, err := buildAliyunTargetURL(tt.base, AliyunEndpointTTS, "")
			require.NoError(t, err)
			require.Equal(t, tt.want+AliyunEndpointTTS, target)
		})
	}
}
