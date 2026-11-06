// Package testutil contains fixtures shared by the CLI and provider tests.
package testutil

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
)

func WriteJSONStream(w http.ResponseWriter, protocol, body string) error {
	var response map[string]any
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		return err
	}
	return WriteInferenceStream(w, protocol, response)
}

// WriteInferenceStream emits a native SSE fixture from a complete test response.
// This is test data generation, not an inference protocol adapter.
func WriteInferenceStream(w http.ResponseWriter, protocol string, response map[string]any) error {
	w.Header().Set("Content-Type", "text/event-stream")
	sequence := 0
	emit := func(event string, value map[string]any) error {
		if protocol == "responses" {
			value["sequence_number"] = sequence
			sequence++
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if event != "" {
			if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", data)
		return err
	}
	switch protocol {
	case "anthropic":
		start := maps.Clone(response)
		start["content"], start["stop_reason"] = []any{}, nil
		usage := maps.Clone(response["usage"].(map[string]any))
		usage["output_tokens"] = 0
		start["usage"] = usage
		if err := emit("message_start", map[string]any{"type": "message_start", "message": start}); err != nil {
			return err
		}
		for index, raw := range response["content"].([]any) {
			block := raw.(map[string]any)
			startBlock := maps.Clone(block)
			input, err := json.Marshal(block["input"])
			if err != nil {
				return err
			}
			startBlock["input"] = map[string]any{}
			for _, event := range []map[string]any{
				{"type": "content_block_start", "index": index, "content_block": startBlock},
				{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}},
				{"type": "content_block_stop", "index": index},
			} {
				if err := emit(event["type"].(string), event); err != nil {
					return err
				}
			}
		}
		if err := emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": response["stop_reason"], "stop_sequence": nil}, "usage": response["usage"]}); err != nil {
			return err
		}
		return emit("message_stop", map[string]any{"type": "message_stop"})
	case "responses":
		start := maps.Clone(response)
		start["output"], start["status"] = []any{}, "in_progress"
		if err := emit("response.created", map[string]any{"type": "response.created", "response": start}); err != nil {
			return err
		}
		for index, raw := range response["output"].([]any) {
			item := raw.(map[string]any)
			startItem := maps.Clone(item)
			startItem["arguments"], startItem["status"] = "", "in_progress"
			for _, event := range []map[string]any{
				{"type": "response.output_item.added", "output_index": index, "item": startItem},
				{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": item["id"], "delta": item["arguments"]},
				{"type": "response.function_call_arguments.done", "output_index": index, "item_id": item["id"], "arguments": item["arguments"]},
				{"type": "response.output_item.done", "output_index": index, "item": item},
			} {
				if err := emit(event["type"].(string), event); err != nil {
					return err
				}
			}
		}
		return emit("response.completed", map[string]any{"type": "response.completed", "response": response})
	case "chat":
		var choices, finishes []any
		for index, raw := range response["choices"].([]any) {
			choice := raw.(map[string]any)
			delta := maps.Clone(choice["message"].(map[string]any))
			var tools []any
			for toolIndex, rawTool := range delta["tool_calls"].([]any) {
				tool := maps.Clone(rawTool.(map[string]any))
				tool["index"] = toolIndex
				tools = append(tools, tool)
			}
			delta["tool_calls"] = tools
			choices = append(choices, map[string]any{"index": index, "delta": delta, "finish_reason": nil})
			finishes = append(finishes, map[string]any{"index": index, "delta": map[string]any{}, "finish_reason": choice["finish_reason"]})
		}
		for _, chunk := range []map[string]any{
			{"id": response["id"], "object": "chat.completion.chunk", "created": 1, "model": response["model"], "choices": choices},
			{"id": response["id"], "object": "chat.completion.chunk", "created": 1, "model": response["model"], "choices": finishes},
			{"id": response["id"], "object": "chat.completion.chunk", "created": 1, "model": response["model"], "choices": []any{}, "usage": response["usage"]},
		} {
			if err := emit("", chunk); err != nil {
				return err
			}
		}
		_, err := fmt.Fprint(w, "data: [DONE]\n\n")
		return err
	default:
		return fmt.Errorf("unsupported test protocol %q", protocol)
	}
}
