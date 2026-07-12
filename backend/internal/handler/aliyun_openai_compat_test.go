package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAICompatibleRequestPlatformPreservesAliyun(t *testing.T) {
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformAliyun}}
	require.Equal(t, service.PlatformAliyun, openAICompatibleRequestPlatform(apiKey))
}
