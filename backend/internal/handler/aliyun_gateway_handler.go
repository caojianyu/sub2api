package handler

import (
	"errors"
	"net/http"
	"strconv"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type AliyunGatewayHandler struct {
	aliyunService       *service.AliyunGatewayService
	billingCacheService *service.BillingCacheService
	apiKeyService       *service.APIKeyService
}

func NewAliyunGatewayHandler(
	aliyunService *service.AliyunGatewayService,
	billingCacheService *service.BillingCacheService,
	apiKeyService *service.APIKeyService,
) *AliyunGatewayHandler {
	return &AliyunGatewayHandler{
		aliyunService:       aliyunService,
		billingCacheService: billingCacheService,
		apiKeyService:       apiKeyService,
	}
}

func (h *AliyunGatewayHandler) Proxy(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "AUTHENTICATION_ERROR", "message": "Invalid API key"}})
		return
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if h.billingCacheService != nil {
		if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
			status, code, message, retryAfter := billingErrorDetails(err)
			if retryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(retryAfter))
			}
			c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
			return
		}
	}
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"code": "REQUEST_BODY_TOO_LARGE", "message": buildBodyTooLargeMessage(maxErr.Limit)}})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "INVALID_REQUEST", "message": "Failed to read request body"}})
		return
	}
	result, err := h.aliyunService.Forward(c.Request.Context(), service.AliyunGatewayRequest{
		Method:          c.Request.Method,
		Path:            c.Request.URL.Path,
		RawQuery:        c.Request.URL.RawQuery,
		Headers:         c.Request.Header,
		Body:            body,
		APIKey:          apiKey,
		Subscription:    subscription,
		APIKeyService:   h.apiKeyService,
		InboundEndpoint: GetInboundEndpoint(c),
		UserAgent:       c.GetHeader("User-Agent"),
		IPAddress:       ip.GetClientIP(c),
		QuotaPlatform:   service.QuotaPlatform(c.Request.Context(), apiKey),
	})
	if err != nil {
		var gwErr *service.AliyunGatewayError
		if errors.As(err, &gwErr) {
			c.JSON(gwErr.Status, gin.H{"error": gin.H{"code": gwErr.Code, "message": gwErr.Message}})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"code": "UPSTREAM_ERROR", "message": err.Error()}})
		return
	}
	for key, values := range result.Header {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
	c.Data(result.StatusCode, result.ContentType, result.Body)
}
