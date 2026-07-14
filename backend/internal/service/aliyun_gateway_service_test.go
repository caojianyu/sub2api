//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type aliyunUsageLogRepoStub struct {
	UsageLogRepository
	lastLog *UsageLog
}

type aliyunUsageBillingRepoStub struct {
	UsageBillingRepository
	calls   int
	lastCmd *UsageBillingCommand
}

func (r *aliyunUsageBillingRepoStub) Apply(_ context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	r.calls++
	r.lastCmd = cmd
	return &UsageBillingApplyResult{Applied: true}, nil
}

func (r *aliyunUsageLogRepoStub) Create(_ context.Context, log *UsageLog) (bool, error) {
	r.lastLog = log
	return true, nil
}

type aliyunHTTPUpstreamSpy struct {
	HTTPUpstream
	calls int
}

func (s *aliyunHTTPUpstreamSpy) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	s.calls++
	return nil, nil
}

func aliyunTestAPIKey() *APIKey {
	return &APIKey{
		ID:   20,
		User: &User{ID: 10},
		Group: &Group{
			ID:             100,
			Platform:       PlatformAliyun,
			RateMultiplier: 1,
		},
	}
}

func aliyunUnitResolver(t *testing.T, model, unit string, price *float64) *ModelPricingResolver {
	t.Helper()
	unitCopy := unit
	return newResolverWithChannel(t, []ChannelModelPricing{{
		Platform:       "anthropic", // newResolverWithChannel's test group platform
		Models:         []string{model},
		BillingMode:    BillingModeUnit,
		MeterUnit:      &unitCopy,
		MeterUnitPrice: price,
	}})
}

func TestAliyunResolveRequestSpec_VoiceCustomizationActions(t *testing.T) {
	svc := &AliyunGatewayService{}
	tests := []struct {
		name     string
		action   string
		wantKind string
		wantQty  float64
	}{
		{name: "create voice", action: "create_voice", wantKind: "unit", wantQty: 1},
		{name: "query voice", action: "query_voice", wantKind: "passthrough"},
		{name: "delete voice", action: "delete_voice", wantKind: "passthrough"},
		{name: "list voices", action: "list_voice", wantKind: "passthrough"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"voice-enrollment","input":{"action":"` + tt.action + `"}}`)
			spec, err := svc.resolveRequestSpec(AliyunEndpointTTSCustomization, body)
			require.NoError(t, err)
			require.Equal(t, tt.wantKind, spec.kind)
			require.Equal(t, AliyunModelVoiceEnrollment, spec.model)
			require.Equal(t, tt.wantQty, spec.quantity)
			require.Equal(t, tt.action, spec.detail["action"])
			if tt.wantKind == "unit" {
				require.Equal(t, AliyunMeterVoice, spec.unit)
			}
		})
	}
}

func TestAliyunResolveRequestSpec_RejectsMissingOrUnknownVoiceAction(t *testing.T) {
	svc := &AliyunGatewayService{}
	tests := []struct {
		name     string
		body     string
		wantCode string
	}{
		{name: "missing", body: `{"model":"voice-enrollment","input":{}}`, wantCode: "ACTION_REQUIRED"},
		{name: "unknown", body: `{"model":"voice-enrollment","input":{"action":"update_voice"}}`, wantCode: "ACTION_NOT_SUPPORTED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.resolveRequestSpec(AliyunEndpointTTSCustomization, []byte(tt.body))
			var gatewayErr *AliyunGatewayError
			require.ErrorAs(t, err, &gatewayErr)
			require.Equal(t, http.StatusBadRequest, gatewayErr.Status)
			require.Equal(t, tt.wantCode, gatewayErr.Code)
		})
	}
}

func TestAliyunUnitPricing_ExplicitZeroAllowedOnlyForVoiceEnrollment(t *testing.T) {
	zero := 0.0
	apiKey := aliyunTestAPIKey()

	voiceResolver := aliyunUnitResolver(t, AliyunModelVoiceEnrollment, AliyunMeterVoice, &zero)
	voiceSvc := &AliyunGatewayService{resolver: voiceResolver}
	resolved, err := voiceSvc.resolveUnitPricing(context.Background(), apiKey, AliyunModelVoiceEnrollment, AliyunMeterVoice)
	require.NoError(t, err)
	require.NotNil(t, resolved)
	require.Zero(t, resolved.MeterUnitPrice)

	ttsResolver := aliyunUnitResolver(t, AliyunModelTTS, AliyunMeterCharacter, &zero)
	ttsSvc := &AliyunGatewayService{resolver: ttsResolver}
	_, err = ttsSvc.resolveUnitPricing(context.Background(), apiKey, AliyunModelTTS, AliyunMeterCharacter)
	var gatewayErr *AliyunGatewayError
	require.ErrorAs(t, err, &gatewayErr)
	require.Equal(t, "UNIT_PRICING_INVALID", gatewayErr.Code)
}

func TestAliyunUnitPricing_NilPriceIsMissingEvenForFreeModel(t *testing.T) {
	resolver := aliyunUnitResolver(t, AliyunModelVoiceEnrollment, AliyunMeterVoice, nil)
	svc := &AliyunGatewayService{resolver: resolver}

	_, err := svc.resolveUnitPricing(context.Background(), aliyunTestAPIKey(), AliyunModelVoiceEnrollment, AliyunMeterVoice)
	var gatewayErr *AliyunGatewayError
	require.ErrorAs(t, err, &gatewayErr)
	require.Equal(t, "UNIT_PRICING_MISSING", gatewayErr.Code)
}

func TestAliyunForwardSubmitOrSync_ValidatesPricingBeforeUpstream(t *testing.T) {
	resolver := newResolverWithChannel(t, nil)
	billing := resolver.billingService
	spy := &aliyunHTTPUpstreamSpy{}
	svc := &AliyunGatewayService{
		billingService: billing,
		resolver:       resolver,
		httpUpstream:   spy,
	}
	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "tts", path: AliyunEndpointTTS, body: `{"model":"cosyvoice-v3-flash","input":{"text":"hello"}}`},
		{name: "asr submit", path: AliyunEndpointASRTranscription, body: `{"model":"qwen3-asr-flash-filetrans"}`},
		{name: "embedding", path: AliyunEndpointMultiModalEmbedding, body: `{"model":"qwen3-vl-embedding"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.forwardSubmitOrSync(context.Background(), AliyunGatewayRequest{
				Path:   tt.path,
				Body:   []byte(tt.body),
				APIKey: aliyunTestAPIKey(),
			})
			var gatewayErr *AliyunGatewayError
			require.ErrorAs(t, err, &gatewayErr)
			require.Equal(t, "UNIT_PRICING_MISSING", gatewayErr.Code)
		})
	}
	require.Zero(t, spy.calls, "upstream must not be called when unit pricing is missing")
}

func TestAliyunExtractMeter_DashScopeASRUsageSeconds(t *testing.T) {
	svc := &AliyunGatewayService{}
	body := []byte(`{"output":{"task_status":"SUCCEEDED"},"usage":{"seconds":12.5}}`)

	unit, quantity := svc.extractMeterFromHeadersOrBody(nil, body, AliyunMeterAudioSecond)

	require.Equal(t, AliyunMeterAudioSecond, unit)
	require.Equal(t, 12.5, quantity)
}

func TestAliyunBillUnit_RecordsFreeVoiceEnrollmentWithoutDeduction(t *testing.T) {
	zero := 0.0
	resolver := aliyunUnitResolver(t, AliyunModelVoiceEnrollment, AliyunMeterVoice, &zero)
	usageRepo := &aliyunUsageLogRepoStub{}
	billingRepo := &aliyunUsageBillingRepoStub{}
	svc := &AliyunGatewayService{
		usageLogRepo:     usageRepo,
		usageBillingRepo: billingRepo,
		gatewayService: &GatewayService{
			deferredService: &DeferredService{},
		},
		billingService: resolver.billingService,
		resolver:       resolver,
	}

	err := svc.billUnit(context.Background(), aliyunUnitBillInput{
		RequestID:       "aliyun:voice:test",
		APIKey:          aliyunTestAPIKey(),
		Account:         &Account{ID: 30},
		Model:           AliyunModelVoiceEnrollment,
		MeterUnit:       AliyunMeterVoice,
		MeterQuantity:   1,
		InboundEndpoint: AliyunEndpointTTSCustomization,
		MeterDetail: map[string]any{
			"action": "create_voice",
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, billingRepo.calls)
	require.NotNil(t, billingRepo.lastCmd)
	require.Zero(t, billingRepo.lastCmd.BalanceCost)
	require.Zero(t, billingRepo.lastCmd.SubscriptionCost)
	require.Zero(t, billingRepo.lastCmd.MeterCost)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, AliyunModelVoiceEnrollment, usageRepo.lastLog.Model)
	require.Zero(t, usageRepo.lastLog.MeterCost)
	require.Zero(t, usageRepo.lastLog.TotalCost)
	require.Zero(t, usageRepo.lastLog.ActualCost)
	require.NotNil(t, usageRepo.lastLog.MeterUnitPrice)
	require.Zero(t, *usageRepo.lastLog.MeterUnitPrice)
	require.NotNil(t, usageRepo.lastLog.MeterQuantity)
	require.Equal(t, 1.0, *usageRepo.lastLog.MeterQuantity)
	require.Equal(t, "create_voice", usageRepo.lastLog.MeterDetail["action"])
}
