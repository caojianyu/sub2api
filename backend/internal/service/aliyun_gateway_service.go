package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

const (
	AliyunEndpointFiles               = "/v1/files"
	AliyunEndpointUploads             = "/api/v1/uploads"
	AliyunEndpointTTS                 = "/api/v1/services/audio/tts/SpeechSynthesizer"
	AliyunEndpointASRTranscription    = "/api/v1/services/audio/asr/transcription"
	AliyunEndpointTTSCustomization    = "/api/v1/services/audio/tts/customization"
	AliyunEndpointMultiModalEmbedding = "/api/v1/services/embeddings/multimodal"
	AliyunEndpointCanonicalEmbedding  = "/api/v1/services/embeddings/multimodal-embedding/multimodal-embedding"
	AliyunEndpointTasksPrefix         = "/api/v1/tasks/"

	AliyunModelTTS                 = "cosyvoice-v3-flash"
	AliyunModelASR                 = "qwen3-asr-flash-filetrans"
	AliyunModelVoiceEnrollment     = "voice-enrollment"
	AliyunModelMultiModalEmbedding = "qwen3-vl-embedding"

	AliyunMeterCharacter   = "character"
	AliyunMeterAudioSecond = "audio_second"
	AliyunMeterVoice       = "voice"
	AliyunMeterToken       = "token"

	aliyunMeterHeaderUnit     = "X-Sub2API-Meter-Unit"
	aliyunMeterHeaderQuantity = "X-Sub2API-Meter-Quantity"
)

// SupportsAliyunFilesGatewayPlatform reports whether a group can use the
// OpenAI-compatible Files endpoint backed by model-router or DashScope.
//
// Some deployments represent a DashScope/model-router upstream as an OpenAI
// API-key account. Files supports both representations, while the remaining
// Aliyun-native endpoints stay restricted to Aliyun groups.
func SupportsAliyunFilesGatewayPlatform(platform string) bool {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case PlatformAliyun, PlatformOpenAI:
		return true
	default:
		return false
	}
}

type AliyunGatewayService struct {
	accountRepo      AccountRepository
	usageLogRepo     UsageLogRepository
	usageBillingRepo UsageBillingRepository
	taskRepo         AliyunTaskRepository
	gatewayService   *GatewayService
	billingService   *BillingService
	resolver         *ModelPricingResolver
	httpUpstream     HTTPUpstream
	cfg              *config.Config
}

func NewAliyunGatewayService(
	accountRepo AccountRepository,
	usageLogRepo UsageLogRepository,
	usageBillingRepo UsageBillingRepository,
	taskRepo AliyunTaskRepository,
	gatewayService *GatewayService,
	billingService *BillingService,
	resolver *ModelPricingResolver,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *AliyunGatewayService {
	return &AliyunGatewayService{
		accountRepo:      accountRepo,
		usageLogRepo:     usageLogRepo,
		usageBillingRepo: usageBillingRepo,
		taskRepo:         taskRepo,
		gatewayService:   gatewayService,
		billingService:   billingService,
		resolver:         resolver,
		httpUpstream:     httpUpstream,
		cfg:              cfg,
	}
}

type AliyunGatewayRequest struct {
	Method          string
	Path            string
	RawQuery        string
	Headers         http.Header
	Body            []byte
	APIKey          *APIKey
	Subscription    *UserSubscription
	APIKeyService   APIKeyQuotaUpdater
	InboundEndpoint string
	UserAgent       string
	IPAddress       string
	QuotaPlatform   string
}

type AliyunGatewayResult struct {
	StatusCode  int
	Header      http.Header
	Body        []byte
	ContentType string
}

type AliyunGatewayError struct {
	Status  int
	Code    string
	Message string
}

func (e *AliyunGatewayError) Error() string {
	return e.Message
}

func (s *AliyunGatewayService) Forward(ctx context.Context, in AliyunGatewayRequest) (*AliyunGatewayResult, error) {
	if s == nil || s.gatewayService == nil || s.accountRepo == nil || s.httpUpstream == nil || s.billingService == nil || s.resolver == nil {
		return nil, aliyunGatewayError(http.StatusInternalServerError, "ALIYUN_GATEWAY_NOT_CONFIGURED", "Aliyun gateway is not fully configured")
	}
	if in.APIKey == nil || in.APIKey.User == nil || in.APIKey.Group == nil {
		return nil, aliyunGatewayError(http.StatusForbidden, "GROUP_REQUIRED", "API key must be assigned to an Aliyun group")
	}
	if in.APIKey.Group.Platform != PlatformAliyun &&
		!(in.Path == AliyunEndpointFiles && SupportsAliyunFilesGatewayPlatform(in.APIKey.Group.Platform)) {
		return nil, aliyunGatewayError(http.StatusNotFound, "PLATFORM_NOT_SUPPORTED", "Aliyun native APIs are not supported for this group")
	}
	var (
		result *AliyunGatewayResult
		err    error
	)
	switch {
	case strings.HasPrefix(in.Path, AliyunEndpointTasksPrefix):
		result, err = s.forwardASRTask(ctx, in)
	default:
		result, err = s.forwardSubmitOrSync(ctx, in)
	}
	if result != nil {
		result.Header = stripAliyunInternalHeaders(result.Header)
	}
	return result, err
}

func (s *AliyunGatewayService) forwardSubmitOrSync(ctx context.Context, in AliyunGatewayRequest) (*AliyunGatewayResult, error) {
	spec, err := s.resolveRequestSpec(in.Path, in.Body)
	if err != nil {
		return nil, err
	}
	if spec.kind == "passthrough" && spec.model == "" && in.RawQuery != "" {
		if query, parseErr := url.ParseQuery(in.RawQuery); parseErr == nil {
			spec.model = strings.TrimSpace(query.Get("model"))
		}
	}
	resolvedPricing, err := s.preflightRequestPricing(ctx, in.APIKey, spec)
	if err != nil {
		return nil, err
	}
	account, release, err := s.selectAndAcquireAccount(ctx, in.APIKey.GroupID, spec.model)
	if err != nil {
		return nil, err
	}
	if release != nil {
		defer release()
	}
	result, err := s.forwardToUpstream(ctx, account, in)
	if err != nil {
		return nil, err
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return result, nil
	}

	switch spec.kind {
	case "asr_submit":
		if s.taskRepo != nil {
			taskID := extractTaskID(result.Body)
			if taskID != "" {
				_ = s.taskRepo.UpsertSubmitted(ctx, &AliyunTaskRecord{
					TaskID:         taskID,
					UserID:         in.APIKey.User.ID,
					APIKeyID:       in.APIKey.ID,
					AccountID:      account.ID,
					GroupID:        in.APIKey.GroupID,
					Model:          spec.model,
					Status:         "pending",
					RequestHash:    aliyunStringPtr(HashUsageRequestPayload(in.Body)),
					SubmitResponse: jsonObjectFromBytes(result.Body),
				})
			}
		}
	case "unit":
		if err := s.billUnit(ctx, aliyunUnitBillInput{
			RequestID:        s.requestID(ctx, spec.model),
			APIKey:           in.APIKey,
			Account:          account,
			Subscription:     in.Subscription,
			APIKeyService:    in.APIKeyService,
			Model:            spec.model,
			MeterUnit:        spec.unit,
			MeterQuantity:    spec.quantity,
			InboundEndpoint:  in.InboundEndpoint,
			UpstreamEndpoint: in.Path,
			UserAgent:        in.UserAgent,
			IPAddress:        in.IPAddress,
			QuotaPlatform:    in.QuotaPlatform,
			MediaType:        spec.mediaType,
			MeterDetail:      spec.detail,
			PayloadHash:      HashUsageRequestPayload(in.Body),
			ResolvedPricing:  resolvedPricing,
		}); err != nil {
			return nil, err
		}
	case "embedding":
		unit, quantity := s.extractMeterFromHeadersOrBody(result.Header, result.Body, AliyunMeterToken)
		if quantity <= 0 {
			return nil, aliyunGatewayError(http.StatusBadGateway, "METER_USAGE_MISSING", "Aliyun embedding response did not include billable usage")
		}
		if err := s.billUnit(ctx, aliyunUnitBillInput{
			RequestID:        s.requestID(ctx, spec.model),
			APIKey:           in.APIKey,
			Account:          account,
			Subscription:     in.Subscription,
			APIKeyService:    in.APIKeyService,
			Model:            spec.model,
			MeterUnit:        unit,
			MeterQuantity:    quantity,
			InboundEndpoint:  in.InboundEndpoint,
			UpstreamEndpoint: in.Path,
			UserAgent:        in.UserAgent,
			IPAddress:        in.IPAddress,
			QuotaPlatform:    in.QuotaPlatform,
			MediaType:        "video",
			MeterDetail: map[string]any{
				"endpoint": in.Path,
				"source":   "embedding_response",
			},
			PayloadHash:     HashUsageRequestPayload(in.Body),
			ResolvedPricing: resolvedPricing,
		}); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *AliyunGatewayService) forwardASRTask(ctx context.Context, in AliyunGatewayRequest) (*AliyunGatewayResult, error) {
	if s.taskRepo == nil {
		return nil, aliyunGatewayError(http.StatusInternalServerError, "ALIYUN_TASK_REPOSITORY_MISSING", "Aliyun task repository is not configured")
	}
	taskID := strings.TrimPrefix(in.Path, AliyunEndpointTasksPrefix)
	taskID = strings.Trim(taskID, "/")
	if taskID == "" {
		return nil, aliyunGatewayError(http.StatusBadRequest, "INVALID_TASK_ID", "task_id is required")
	}
	task, err := s.taskRepo.GetByTaskID(ctx, taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, aliyunGatewayError(http.StatusNotFound, "TASK_NOT_FOUND", "Aliyun task is not tracked by this gateway")
		}
		return nil, err
	}
	if task.UserID != in.APIKey.User.ID || task.APIKeyID != in.APIKey.ID {
		return nil, aliyunGatewayError(http.StatusForbidden, "TASK_FORBIDDEN", "Aliyun task does not belong to this API key")
	}
	var resolvedPricing *ResolvedPricing
	if task.BilledAt == nil {
		resolvedPricing, err = s.resolveUnitPricing(ctx, in.APIKey, task.Model, AliyunMeterAudioSecond)
		if err != nil {
			return nil, err
		}
	}
	account, err := s.accountRepo.GetByID(ctx, task.AccountID)
	if err != nil {
		return nil, err
	}
	release, err := s.acquireAccountSlot(ctx, account)
	if err != nil {
		return nil, err
	}
	if release != nil {
		defer release()
	}

	result, err := s.forwardToUpstream(ctx, account, in)
	if err != nil {
		return nil, err
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return result, nil
	}

	status := extractTaskStatus(result.Body)
	unit, quantity := s.extractMeterFromHeadersOrBody(result.Header, result.Body, AliyunMeterAudioSecond)
	if status != "" {
		_ = s.taskRepo.UpdateFinal(ctx, taskID, status, stringPtrIfNotEmpty(unit), float64PtrIfPositive(quantity), jsonObjectFromBytes(result.Body))
	}
	if !isTerminalSuccessStatus(status) || task.BilledAt != nil {
		return result, nil
	}
	if quantity <= 0 {
		return nil, aliyunGatewayError(http.StatusBadGateway, "METER_USAGE_MISSING", "Aliyun ASR task response did not include billable duration")
	}
	if err := s.billUnit(ctx, aliyunUnitBillInput{
		RequestID:        "aliyun:asr:" + taskID,
		APIKey:           in.APIKey,
		Account:          account,
		Subscription:     in.Subscription,
		APIKeyService:    in.APIKeyService,
		Model:            task.Model,
		MeterUnit:        unit,
		MeterQuantity:    quantity,
		InboundEndpoint:  in.InboundEndpoint,
		UpstreamEndpoint: in.Path,
		UserAgent:        in.UserAgent,
		IPAddress:        in.IPAddress,
		QuotaPlatform:    in.QuotaPlatform,
		MediaType:        "audio",
		MeterDetail: map[string]any{
			"task_id":  taskID,
			"status":   status,
			"endpoint": in.Path,
		},
		PayloadHash:     taskID,
		ResolvedPricing: resolvedPricing,
	}); err != nil {
		return nil, err
	}
	_ = s.taskRepo.MarkBilled(ctx, taskID, nil)
	return result, nil
}

type aliyunRequestSpec struct {
	kind      string
	model     string
	unit      string
	quantity  float64
	mediaType string
	detail    map[string]any
}

func (s *AliyunGatewayService) resolveRequestSpec(path string, body []byte) (aliyunRequestSpec, error) {
	obj := jsonObjectFromBytes(body)
	model := firstNonEmptyString(jsonStringAt(obj, "model"))
	switch path {
	case AliyunEndpointFiles:
		return aliyunRequestSpec{kind: "passthrough", model: "qwen-long"}, nil
	case AliyunEndpointUploads:
		return aliyunRequestSpec{kind: "passthrough"}, nil
	case AliyunEndpointTTS:
		if model == "" {
			model = AliyunModelTTS
		}
		text := extractTextForTTS(obj)
		if strings.TrimSpace(text) == "" {
			return aliyunRequestSpec{}, aliyunGatewayError(http.StatusBadRequest, "TEXT_REQUIRED", "TTS text is required")
		}
		return aliyunRequestSpec{
			kind:      "unit",
			model:     model,
			unit:      AliyunMeterCharacter,
			quantity:  float64(utf8.RuneCountInString(text)),
			mediaType: "audio",
			detail: map[string]any{
				"text_chars": utf8.RuneCountInString(text),
				"endpoint":   path,
			},
		}, nil
	case AliyunEndpointASRTranscription:
		if model == "" {
			model = AliyunModelASR
		}
		return aliyunRequestSpec{kind: "asr_submit", model: model}, nil
	case AliyunEndpointTTSCustomization:
		if model == "" {
			model = AliyunModelVoiceEnrollment
		}
		action := strings.ToLower(firstNonEmptyString(
			jsonStringAt(obj, "input.action"),
			jsonStringAt(obj, "action"),
		))
		if action == "" {
			return aliyunRequestSpec{}, aliyunGatewayError(http.StatusBadRequest, "ACTION_REQUIRED", "voice customization action is required")
		}
		detail := map[string]any{
			"endpoint": path,
			"action":   action,
		}
		switch action {
		case "create_voice", "create":
			return aliyunRequestSpec{
				kind:      "unit",
				model:     model,
				unit:      AliyunMeterVoice,
				quantity:  1,
				mediaType: "audio",
				detail:    detail,
			}, nil
		case "query_voice", "delete_voice", "list_voice", "query", "delete", "list":
			// Management operations do not create a billable voice. They are
			// intentionally forwarded without a usage charge.
			return aliyunRequestSpec{
				kind:      "passthrough",
				model:     model,
				mediaType: "audio",
				detail:    detail,
			}, nil
		default:
			return aliyunRequestSpec{}, aliyunGatewayError(http.StatusBadRequest, "ACTION_NOT_SUPPORTED", "unsupported voice customization action: "+action)
		}
	case AliyunEndpointMultiModalEmbedding, AliyunEndpointCanonicalEmbedding:
		if model == "" {
			model = AliyunModelMultiModalEmbedding
		}
		return aliyunRequestSpec{kind: "embedding", model: model}, nil
	default:
		return aliyunRequestSpec{}, aliyunGatewayError(http.StatusNotFound, "ENDPOINT_NOT_SUPPORTED", "Aliyun endpoint is not supported")
	}
}

func (s *AliyunGatewayService) preflightRequestPricing(ctx context.Context, apiKey *APIKey, spec aliyunRequestSpec) (*ResolvedPricing, error) {
	switch spec.kind {
	case "unit":
		return s.resolveUnitPricing(ctx, apiKey, spec.model, spec.unit)
	case "asr_submit":
		return s.resolveUnitPricing(ctx, apiKey, spec.model, AliyunMeterAudioSecond)
	case "embedding":
		return s.resolveUnitPricing(ctx, apiKey, spec.model, AliyunMeterToken)
	default:
		return nil, nil
	}
}

func (s *AliyunGatewayService) resolveUnitPricing(ctx context.Context, apiKey *APIKey, model, meterUnit string) (*ResolvedPricing, error) {
	if s == nil || s.resolver == nil || apiKey == nil || apiKey.Group == nil {
		return nil, aliyunGatewayError(http.StatusInternalServerError, "BILLING_CONTEXT_MISSING", "unit billing context is incomplete")
	}
	groupID := apiKey.Group.ID
	resolved := s.resolver.Resolve(ctx, PricingInput{Model: model, GroupID: &groupID})
	if err := validateAliyunUnitPricing(resolved, model, meterUnit); err != nil {
		return nil, err
	}
	return resolved, nil
}

func validateAliyunUnitPricing(resolved *ResolvedPricing, model, meterUnit string) error {
	if resolved == nil || resolved.Source != PricingSourceChannel || resolved.Mode != BillingModeUnit || resolved.channelPricing == nil {
		return aliyunGatewayError(http.StatusInternalServerError, "UNIT_PRICING_MISSING", "unit pricing is not configured for model "+model)
	}
	configuredUnit := strings.TrimSpace(resolved.MeterUnit)
	if configuredUnit == "" || !strings.EqualFold(configuredUnit, strings.TrimSpace(meterUnit)) {
		return aliyunGatewayError(http.StatusInternalServerError, "UNIT_PRICING_INVALID", fmt.Sprintf("unit pricing mismatch for model %s: request=%q pricing=%q", model, meterUnit, configuredUnit))
	}
	// A nil pointer means the administrator did not configure a price. It is
	// distinct from an explicit zero, which is valid only for a known free unit.
	if resolved.channelPricing.MeterUnitPrice == nil {
		return aliyunGatewayError(http.StatusInternalServerError, "UNIT_PRICING_MISSING", "unit price is not configured for model "+model)
	}
	price := *resolved.channelPricing.MeterUnitPrice
	if price < 0 || (price == 0 && !allowsFreeAliyunUnitPrice(model, meterUnit)) {
		return aliyunGatewayError(http.StatusInternalServerError, "UNIT_PRICING_INVALID", fmt.Sprintf("unit price must be positive for model %s unit %s", model, meterUnit))
	}
	return nil
}

func allowsFreeAliyunUnitPrice(model, meterUnit string) bool {
	return strings.EqualFold(strings.TrimSpace(model), AliyunModelVoiceEnrollment) &&
		strings.EqualFold(strings.TrimSpace(meterUnit), AliyunMeterVoice)
}

func (s *AliyunGatewayService) selectAndAcquireAccount(ctx context.Context, groupID *int64, model string) (*Account, func(), error) {
	if s.gatewayService == nil {
		return nil, nil, fmt.Errorf("gateway service is not configured")
	}
	account, err := s.gatewayService.SelectAccountForModelWithExclusions(ctx, groupID, "", model, nil)
	if err != nil {
		return nil, nil, err
	}
	release, err := s.acquireAccountSlot(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	return account, release, nil
}

func (s *AliyunGatewayService) acquireAccountSlot(ctx context.Context, account *Account) (func(), error) {
	if s.gatewayService == nil || account == nil {
		return nil, fmt.Errorf("account selection failed")
	}
	acq, err := s.gatewayService.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
	if err != nil {
		return nil, err
	}
	if acq == nil || !acq.Acquired {
		return nil, ErrNoAvailableAccounts
	}
	return acq.ReleaseFunc, nil
}

func (s *AliyunGatewayService) forwardToUpstream(ctx context.Context, account *Account, in AliyunGatewayRequest) (*AliyunGatewayResult, error) {
	if account == nil {
		return nil, aliyunGatewayError(http.StatusBadGateway, "ACCOUNT_PLATFORM_MISMATCH", "selected account is missing")
	}

	var apiKey, baseURL string
	if in.Path == AliyunEndpointFiles {
		if !account.IsOpenAI() && !account.IsAliyun() {
			return nil, aliyunGatewayError(http.StatusBadGateway, "ACCOUNT_PLATFORM_MISMATCH", "selected account is not OpenAI-compatible")
		}
		apiKey = strings.TrimSpace(account.GetOpenAIApiKey())
		baseURL = account.GetOpenAIBaseURL()
	} else {
		if !account.IsAliyun() {
			return nil, aliyunGatewayError(http.StatusBadGateway, "ACCOUNT_PLATFORM_MISMATCH", "selected account is not an Aliyun account")
		}
		apiKey = strings.TrimSpace(account.GetAliyunAPIKey())
		baseURL = account.GetAliyunBaseURL()
	}
	if apiKey == "" {
		return nil, aliyunGatewayError(http.StatusBadGateway, "ACCOUNT_CREDENTIAL_MISSING", "upstream account api_key is missing")
	}
	target, err := buildAliyunTargetURL(baseURL, in.Path, in.RawQuery)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, in.Method, target, bytes.NewReader(in.Body))
	if err != nil {
		return nil, err
	}
	copyAliyunRequestHeaders(req.Header, in.Headers)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if req.Header.Get("Content-Type") == "" && len(in.Body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readUpstreamResponseBodyLimited(resp.Body, resolveUpstreamResponseReadLimit(s.cfg))
	if err != nil {
		return nil, err
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	return &AliyunGatewayResult{
		StatusCode:  resp.StatusCode,
		Header:      cloneHTTPHeader(resp.Header),
		Body:        body,
		ContentType: contentType,
	}, nil
}

func buildAliyunTargetURL(baseURL, path, rawQuery string) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "", fmt.Errorf("aliyun base_url is empty")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u, err := url.Parse(baseURL + path)
	if err != nil {
		return "", err
	}
	u.RawQuery = rawQuery
	return u.String(), nil
}

func copyAliyunRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		switch strings.ToLower(canonical) {
		case "authorization", "host", "content-length", "connection":
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func stripAliyunInternalHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		if strings.HasPrefix(strings.ToLower(key), "x-sub2api-") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	return dst
}

type aliyunUnitBillInput struct {
	RequestID        string
	APIKey           *APIKey
	Account          *Account
	Subscription     *UserSubscription
	APIKeyService    APIKeyQuotaUpdater
	Model            string
	MeterUnit        string
	MeterQuantity    float64
	InboundEndpoint  string
	UpstreamEndpoint string
	UserAgent        string
	IPAddress        string
	QuotaPlatform    string
	MediaType        string
	MeterDetail      map[string]any
	PayloadHash      string
	ResolvedPricing  *ResolvedPricing
}

func (s *AliyunGatewayService) billUnit(ctx context.Context, in aliyunUnitBillInput) error {
	if in.APIKey == nil || in.APIKey.User == nil || in.APIKey.Group == nil || in.Account == nil {
		return aliyunGatewayError(http.StatusInternalServerError, "BILLING_CONTEXT_MISSING", "billing context is incomplete")
	}
	groupID := in.APIKey.Group.ID
	resolved := in.ResolvedPricing
	if resolved == nil {
		var err error
		resolved, err = s.resolveUnitPricing(ctx, in.APIKey, in.Model, in.MeterUnit)
		if err != nil {
			return err
		}
	} else if err := validateAliyunUnitPricing(resolved, in.Model, in.MeterUnit); err != nil {
		return err
	}
	multiplier := 1.0
	if s.cfg != nil {
		multiplier = s.cfg.Default.RateMultiplier
	}
	if in.APIKey.GroupID != nil {
		multiplier = s.gatewayService.getUserGroupRateMultiplier(ctx, in.APIKey.User.ID, *in.APIKey.GroupID, in.APIKey.Group.RateMultiplier)
	}
	cost, err := s.billingService.CalculateCostUnified(CostInput{
		Ctx:                ctx,
		Model:              in.Model,
		GroupID:            &groupID,
		MeterUnit:          in.MeterUnit,
		MeterQuantity:      in.MeterQuantity,
		AllowZeroUnitPrice: allowsFreeAliyunUnitPrice(in.Model, in.MeterUnit),
		RateMultiplier:     multiplier,
		Resolver:           s.resolver,
		Resolved:           resolved,
	})
	if err != nil {
		return err
	}
	billingMode := string(BillingModeUnit)
	meterUnit := cost.MeterUnit
	meterQuantity := cost.MeterQuantity
	meterUnitPrice := cost.MeterUnitPrice
	accountRateMultiplier := in.Account.BillingRateMultiplier()
	billingType := BillingTypeBalance
	isSubscriptionBilling := in.Subscription != nil && in.APIKey.Group.IsSubscriptionType()
	if isSubscriptionBilling {
		billingType = BillingTypeSubscription
	}
	durationMs := 0
	now := time.Now()
	usageLog := &UsageLog{
		UserID:                in.APIKey.User.ID,
		APIKeyID:              in.APIKey.ID,
		AccountID:             in.Account.ID,
		RequestID:             strings.TrimSpace(in.RequestID),
		Model:                 in.Model,
		RequestedModel:        in.Model,
		GroupID:               in.APIKey.GroupID,
		SubscriptionID:        optionalSubscriptionID(in.Subscription),
		InputCost:             cost.InputCost,
		OutputCost:            cost.OutputCost,
		MeterCost:             cost.MeterCost,
		CacheCreationCost:     cost.CacheCreationCost,
		CacheReadCost:         cost.CacheReadCost,
		TotalCost:             cost.TotalCost,
		ActualCost:            cost.ActualCost,
		MeterUnit:             &meterUnit,
		MeterQuantity:         &meterQuantity,
		MeterUnitPrice:        &meterUnitPrice,
		MeterDetail:           in.MeterDetail,
		RateMultiplier:        multiplier,
		AccountRateMultiplier: &accountRateMultiplier,
		BillingType:           billingType,
		RequestType:           RequestTypeSync,
		DurationMs:            &durationMs,
		UserAgent:             optionalTrimmedStringPtr(in.UserAgent),
		IPAddress:             optionalTrimmedStringPtr(in.IPAddress),
		MediaType:             optionalTrimmedStringPtr(in.MediaType),
		InboundEndpoint:       optionalTrimmedStringPtr(in.InboundEndpoint),
		UpstreamEndpoint:      optionalTrimmedStringPtr(in.UpstreamEndpoint),
		BillingMode:           &billingMode,
		CreatedAt:             now,
	}
	applied, billingErr := applyUsageBilling(ctx, usageLog.RequestID, usageLog, &postUsageBillingParams{
		Cost:                  cost,
		User:                  in.APIKey.User,
		APIKey:                in.APIKey,
		Account:               in.Account,
		Subscription:          in.Subscription,
		RequestPayloadHash:    strings.TrimSpace(in.PayloadHash),
		IsSubscriptionBill:    isSubscriptionBilling,
		AccountRateMultiplier: accountRateMultiplier,
		APIKeyService:         in.APIKeyService,
		Platform:              firstNonEmptyString(in.QuotaPlatform, PlatformAliyun),
	}, s.gatewayService.billingDeps(), s.usageBillingRepo)
	if billingErr != nil {
		return billingErr
	}
	if applied {
		writeUsageLogBestEffort(ctx, s.usageLogRepo, usageLog, "service.aliyun_gateway")
	}
	return nil
}

func (s *AliyunGatewayService) requestID(ctx context.Context, model string) string {
	if ctx != nil {
		if id, _ := ctx.Value(ctxkey.ClientRequestID).(string); strings.TrimSpace(id) != "" {
			return "aliyun:" + strings.TrimSpace(model) + ":" + strings.TrimSpace(id)
		}
	}
	return fmt.Sprintf("aliyun:%s:%d", strings.TrimSpace(model), time.Now().UnixNano())
}

func (s *AliyunGatewayService) extractMeterFromHeadersOrBody(headers http.Header, body []byte, defaultUnit string) (string, float64) {
	unit := strings.TrimSpace(headers.Get(aliyunMeterHeaderUnit))
	if unit == "" {
		unit = defaultUnit
	}
	if q, err := strconv.ParseFloat(strings.TrimSpace(headers.Get(aliyunMeterHeaderQuantity)), 64); err == nil && q > 0 {
		return unit, q
	}
	obj := jsonObjectFromBytes(body)
	if q := extractUsageQuantity(obj); q > 0 {
		return unit, q
	}
	return unit, 0
}

func jsonObjectFromBytes(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	return obj
}

func extractTaskID(body []byte) string {
	obj := jsonObjectFromBytes(body)
	return firstNonEmptyString(
		jsonStringAt(obj, "output.task_id"),
		jsonStringAt(obj, "task_id"),
		jsonStringAt(obj, "id"),
	)
}

func extractTaskStatus(body []byte) string {
	obj := jsonObjectFromBytes(body)
	return strings.TrimSpace(firstNonEmptyString(
		jsonStringAt(obj, "output.task_status"),
		jsonStringAt(obj, "output.status"),
		jsonStringAt(obj, "task_status"),
		jsonStringAt(obj, "status"),
	))
}

func isTerminalSuccessStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "success", "completed", "done":
		return true
	default:
		return false
	}
}

func extractTextForTTS(obj map[string]any) string {
	return firstNonEmptyString(
		jsonStringAt(obj, "input.text"),
		jsonStringAt(obj, "text"),
		jsonStringAt(obj, "input"),
	)
}

func jsonStringAt(obj map[string]any, path string) string {
	if obj == nil || path == "" {
		return ""
	}
	var current any = obj
	for _, part := range strings.Split(path, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = m[part]
	}
	if s, ok := current.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func extractUsageQuantity(obj map[string]any) float64 {
	if obj == nil {
		return 0
	}
	paths := []string{
		"usage.duration",
		"usage.audio_duration",
		"usage.total_duration",
		"usage.total_tokens",
		"usage.input_tokens",
		"output.duration",
		"output.audio_duration",
		"output.total_duration",
	}
	for _, path := range paths {
		if value := jsonNumberAt(obj, path); value > 0 {
			if strings.Contains(path, "duration") {
				return normalizeDurationSeconds(path, value)
			}
			return value
		}
	}
	return recursiveUsageQuantity(obj)
}

func recursiveUsageQuantity(v any) float64 {
	switch typed := v.(type) {
	case map[string]any:
		for key, value := range typed {
			lower := strings.ToLower(key)
			if number := numericAny(value); number > 0 {
				switch {
				case strings.Contains(lower, "duration_ms"), strings.Contains(lower, "milliseconds"):
					return number / 1000
				case strings.Contains(lower, "duration"), strings.Contains(lower, "audio_second"):
					return normalizeDurationSeconds(lower, number)
				case strings.Contains(lower, "total_tokens"), strings.Contains(lower, "input_tokens"):
					return number
				}
			}
			if nested := recursiveUsageQuantity(value); nested > 0 {
				return nested
			}
		}
	case []any:
		for _, item := range typed {
			if nested := recursiveUsageQuantity(item); nested > 0 {
				return nested
			}
		}
	}
	return 0
}

func jsonNumberAt(obj map[string]any, path string) float64 {
	if obj == nil || path == "" {
		return 0
	}
	var current any = obj
	for _, part := range strings.Split(path, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return 0
		}
		current = m[part]
	}
	return numericAny(current)
}

func numericAny(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		out, _ := n.Float64()
		return out
	case string:
		out, _ := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return out
	default:
		return 0
	}
}

func normalizeDurationSeconds(key string, value float64) float64 {
	if value <= 0 {
		return 0
	}
	lower := strings.ToLower(key)
	if strings.Contains(lower, "ms") || strings.Contains(lower, "millisecond") || value > 360000 {
		return math.Ceil(value/1000*1000) / 1000
	}
	return value
}

func aliyunStringPtr(v string) *string {
	return &v
}

func stringPtrIfNotEmpty(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	value := strings.TrimSpace(v)
	return &value
}

func float64PtrIfPositive(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	return &v
}

func aliyunGatewayError(status int, code, message string) *AliyunGatewayError {
	return &AliyunGatewayError{Status: status, Code: code, Message: message}
}
