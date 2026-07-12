package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
