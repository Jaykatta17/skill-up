package observation

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var markerTools = []map[string]any{
	{
		"name":        "mark_skill_invocation",
		"description": "Declare the Skill responsible for the current interaction.",
		"inputSchema": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"skill_name":    map[string]any{"type": "string"},
				"skill_version": map[string]any{"type": "string"},
			},
			"required": []string{"skill_name"},
		},
	},
	{
		"name":        "attach_skill_evidence",
		"description": "Attach a local reference or summary as evidence for the current Skill interaction.",
		"inputSchema": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"kind":      map[string]any{"type": "string"},
				"reference": map[string]any{"type": "string"},
				"summary":   map[string]any{"type": "string"},
			},
			"required": []string{"kind"},
		},
	},
	{
		"name":        "record_skill_feedback",
		"description": "Record user feedback for the current Skill interaction.",
		"inputSchema": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"sentiment": map[string]any{"type": "string", "enum": []string{"positive", "negative", "mixed", "neutral"}},
				"comment":   map[string]any{"type": "string"},
			},
		},
	},
}

// ServeMCP runs the observer's stateless marker-tool MCP server over stdio.
// State is captured by the matching Codex PostToolUse hook, keeping this server
// side-effect free and auditable.
func ServeMCP(reader io.Reader, writer io.Writer) error {
	scanner := bufio.NewScanner(reader)
	const maxMessageBytes = 1024 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxMessageBytes)
	encoder := json.NewEncoder(writer)
	for scanner.Scan() {
		var req rpcRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			if err := encoder.Encode(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}}); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		resp := handleRPC(req)
		if err := encoder.Encode(resp); err != nil {
			return fmt.Errorf("write MCP response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read MCP request: %w", err)
	}
	return nil
}

func handleRPC(req rpcRequest) rpcResponse {
	response := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		protocolVersion := "2025-06-18"
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &params) == nil && params.ProtocolVersion != "" {
			protocolVersion = params.ProtocolVersion
		}
		response.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "skill-up-observer", "version": "0.1.0"},
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": markerTools}
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			response.Error = &rpcError{Code: -32602, Message: "invalid tools/call params"}
			return response
		}
		if !knownTool(params.Name) {
			response.Error = &rpcError{Code: -32602, Message: "unknown observer tool"}
			return response
		}
		if err := validateToolArguments(params.Name, params.Arguments); err != nil {
			response.Error = &rpcError{Code: -32602, Message: err.Error()}
			return response
		}
		response.Result = map[string]any{
			"content":           []map[string]any{{"type": "text", "text": "Observation marker accepted; the Codex hook will capture it locally."}},
			"structuredContent": map[string]any{"accepted": true, "tool": params.Name},
		}
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	return response
}

func validateToolArguments(name string, arguments map[string]any) error {
	switch name {
	case "mark_skill_invocation":
		value, ok := arguments["skill_name"].(string)
		if !ok || !skillNamePattern.MatchString(strings.ToLower(strings.TrimSpace(value))) {
			return errors.New("mark_skill_invocation requires a valid skill_name")
		}
		return nil
	case "attach_skill_evidence":
		value, ok := arguments["kind"].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return errors.New("attach_skill_evidence requires a non-empty kind")
		}
		return nil
	case "record_skill_feedback":
		value, ok := arguments["sentiment"]
		if !ok {
			return nil
		}
		sentiment, ok := value.(string)
		if !ok || !validFeedbackSentiment(sentiment) {
			return errors.New("record_skill_feedback requires a valid sentiment")
		}
		return nil
	}
	return errors.New("unknown observer tool")
}

func knownTool(name string) bool {
	for _, tool := range markerTools {
		if tool["name"] == name {
			return true
		}
	}
	return false
}
