package anthropic

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestToAnthropicResponsesStreamResponseFunctionCallStopsForToolUse(t *testing.T) {
	t.Parallel()

	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	defer cancel()

	ToAnthropicResponsesStreamResponse(ctx, &schemas.BifrostResponsesStreamResponse{
		Type: schemas.ResponsesStreamResponseTypeOutputItemAdded,
		Item: &schemas.ResponsesMessage{
			ID:   schemas.Ptr("fc_123"),
			Type: schemas.Ptr(schemas.ResponsesMessageTypeFunctionCall),
			ResponsesToolMessage: &schemas.ResponsesToolMessage{
				CallID:    schemas.Ptr("call_123"),
				Name:      schemas.Ptr("bash"),
				Arguments: schemas.Ptr(`{"command":"pwd"}`),
			},
		},
	})

	events := ToAnthropicResponsesStreamResponse(ctx, &schemas.BifrostResponsesStreamResponse{
		Type: schemas.ResponsesStreamResponseTypeCompleted,
		Response: &schemas.BifrostResponsesResponse{
			ID:     schemas.Ptr("resp_test"),
			Model:  "gpt-5.6-sol",
			Output: []schemas.ResponsesMessage{},
		},
	})

	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	if events[0].Type != AnthropicStreamEventTypeMessageDelta {
		t.Fatalf("event type = %q, want %q", events[0].Type, AnthropicStreamEventTypeMessageDelta)
	}
	if events[0].Delta == nil || events[0].Delta.StopReason == nil {
		t.Fatalf("message_delta stop reason is missing")
	}
	if *events[0].Delta.StopReason != AnthropicStopReasonToolUse {
		t.Fatalf("message_delta stop reason = %q, want %q", *events[0].Delta.StopReason, AnthropicStopReasonToolUse)
	}
}
