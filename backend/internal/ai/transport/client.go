package transport

import (
	"context"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

// ServiceTierFallbackFunc observes a flex-to-auto retry before its delay.
type ServiceTierFallbackFunc func(context.Context, string)

// Client owns provider HTTP connections, codecs, retries, and model discovery.
type Client struct {
	api      *httpAPIClient
	model    string
	complete func(context.Context, Request) (*Response, error)
}

// NewClient uses caller-validated provider settings without applying defaults.
func NewClient(config modelprovider.Config, token string, extraHeaders map[string]string, serviceTier string, onFallback ServiceTierFallbackFunc) *Client {
	api := newHTTPAPIClient(config.Endpoint, token, extraHeaders)
	api.serviceTier = serviceTier
	api.onServiceTierFallback = onFallback
	c := &Client{api: api, model: config.Model}
	switch config.API {
	case modelprovider.APIChatCompletions:
		c.complete = newChatCompletionsTransport(api).Complete
	case modelprovider.APIResponses:
		c.complete = newResponsesTransport(api).Complete
	default:
		c.complete = unsupportedTransport{api: config.API}.Complete
	}
	return c
}

// Complete executes one provider turn without application throttling or accounting.
func (c *Client) Complete(ctx context.Context, request Request) (*Response, error) {
	return c.complete(ctx, request)
}
