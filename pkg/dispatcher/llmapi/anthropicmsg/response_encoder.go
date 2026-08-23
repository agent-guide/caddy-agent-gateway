package anthropicmsg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
	"github.com/cloudwego/eino/schema"
)

type responseMode string

const (
	responseModeNormalized  responseMode = "normalized"
	responseModeNativeRelay responseMode = "native_relay"
)

type rewriteSet struct {
	ClientModel string
}

type responseOpen struct {
	Candidate             provider.ServedCandidate
	Mode                  responseMode
	RewriteSet            rewriteSet
	RelayIneligibleReason string
}

type responseBody struct {
	Status      int
	ContentType string
	Payload     []byte
}

type responseBodySink interface {
	Emit(context.Context, responseBody) error
}

type anthropicResponseEncoder struct {
	lifecycle responseLifecycle
}

func newAnthropicResponseEncoder(lifecycle responseLifecycle) *anthropicResponseEncoder {
	return &anthropicResponseEncoder{lifecycle: lifecycle}
}

func (e *anthropicResponseEncoder) Emit(ctx context.Context, open responseOpen, response *provider.ChatResponse, sink responseBodySink) error {
	messageIDSource := "gateway"
	usageSource := "provider_projection"
	if open.Mode == responseModeNativeRelay {
		messageIDSource = "upstream"
		usageSource = "native_body"
	}
	e.lifecycle.ObserveResponse(responseObservation{Mode: string(open.Mode), RelayIneligibleReason: open.RelayIneligibleReason, MessageIDSource: messageIDSource, UsageSource: usageSource})
	if response == nil || response.Message == nil {
		err := fmt.Errorf("provider returned an empty response")
		_ = e.lifecycle.Fail(responseFailure{StatusCode: http.StatusBadGateway, Outcome: "invalid_state", ErrorType: "invalid_state"})
		return err
	}
	var payload []byte
	var err error
	switch open.Mode {
	case responseModeNativeRelay:
		payload, err = encodeRelayedResponse(response.Message, open.RewriteSet)
	default:
		payload, err = json.Marshal((&Converter{}).FromInternal(response, open.RewriteSet.ClientModel))
	}
	if err != nil {
		_ = e.lifecycle.Fail(responseFailure{StatusCode: http.StatusBadGateway, Outcome: "invalid_state", ErrorType: "response_encode_failed"})
		return err
	}
	tokens := provider.UsageFromMessage(response.Message)
	e.lifecycle.ObserveUsage(usageObservation{InputTokens: tokens.InputTokens, OutputTokens: tokens.OutputTokens, CachedTokens: tokens.CachedTokens, ReasoningTokens: tokens.ReasoningTokens, Final: true})
	if err := sink.Emit(ctx, responseBody{Status: http.StatusOK, ContentType: "application/json", Payload: payload}); err != nil {
		_ = e.lifecycle.Fail(responseFailure{StatusCode: http.StatusOK, Outcome: "sink_error", ErrorType: "response_write_failed"})
		return err
	}
	e.lifecycle.Committed()
	return e.lifecycle.Finish(responseFinish{StatusCode: http.StatusOK, Outcome: "completed"})
}

func encodeRelayedResponse(message *schema.Message, rewrites rewriteSet) ([]byte, error) {
	raw := anthropicbase.AnthropicResponseBodyFromMessage(message)
	if len(raw) == 0 {
		return nil, fmt.Errorf("native response relay selected without response body")
	}
	var response anthropicbase.ModeledObject
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode native response body: %w", err)
	}
	id, _ := response["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("native response body has empty id")
	}
	if _, ok := response["content"].([]any); !ok {
		return nil, fmt.Errorf("native response body has invalid content")
	}
	codec, err := provider.DialectCodecFor(provider.ProtocolDialectAnthropic)
	if err != nil {
		return nil, err
	}
	envelope, err := codec.Capture(provider.NativeCaptureInput{
		Scope: provider.NativeScopeResponseEphemeral, Kind: provider.NativeKindResponseBody,
		Raw: raw, Modeled: response,
	})
	if err != nil {
		return nil, err
	}
	current := make(anthropicbase.ModeledObject, len(response))
	for key, value := range response {
		current[key] = value
	}
	current["model"] = rewrites.ClientModel
	return codec.Overlay(provider.NativeOverlayInput{Envelope: envelope, Current: current})
}

type httpResponseBodySink struct {
	w http.ResponseWriter
}

func (s httpResponseBodySink) Emit(_ context.Context, body responseBody) error {
	if body.ContentType != "" {
		s.w.Header().Set("Content-Type", body.ContentType)
	}
	s.w.WriteHeader(body.Status)
	_, err := s.w.Write(body.Payload)
	return err
}
