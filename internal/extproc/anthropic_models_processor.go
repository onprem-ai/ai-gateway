// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
)

// --- Response types matching the Anthropic API format ---

type anthropicModelInfo struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

type anthropicModelList struct {
	Data    []anthropicModelInfo `json:"data"`
	HasMore bool                 `json:"has_more"`
	FirstID string               `json:"first_id,omitempty"`
	LastID  string               `json:"last_id,omitempty"`
}

type anthropicErrorResponse struct {
	Type  string             `json:"type"`
	Error anthropicErrorBody `json:"error"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// --- List processor: GET /anthropic/v1/models ---

// anthropicModelsListProcessor returns the list of declared models in Anthropic API format.
// Duplicates (from multi-schema AIServiceBackends) are collapsed by model name.
type anthropicModelsListProcessor struct {
	passThroughProcessor
	logger   *slog.Logger
	response []byte
}

var _ Processor = (*anthropicModelsListProcessor)(nil)

// NewAnthropicModelsProcessor creates a processor that returns declared models in Anthropic format.
func NewAnthropicModelsProcessor(config *filterapi.RuntimeConfig, _ map[string]string, logger *slog.Logger, isUpstreamFilter bool, _ bool) (Processor, error) {
	if isUpstreamFilter {
		return passThroughProcessor{}, nil
	}

	models := deduplicateModels(config.DeclaredModels)

	list := anthropicModelList{
		Data:    models,
		HasMore: false,
	}
	if len(models) > 0 {
		list.FirstID = models[0].ID
		list.LastID = models[len(models)-1].ID
	}

	body, err := json.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal anthropic models list: %w", err)
	}

	return &anthropicModelsListProcessor{logger: logger, response: body}, nil
}

// ProcessRequestHeaders implements [Processor.ProcessRequestHeaders].
func (p *anthropicModelsListProcessor) ProcessRequestHeaders(_ context.Context, _ *corev3.HeaderMap) (*extprocv3.ProcessingResponse, error) {
	return immediateJSONResponse(typev3.StatusCode_OK, p.response), nil
}

// --- Retrieve processor: GET /anthropic/v1/models/{model_id} ---

// anthropicModelRetrieveProcessor returns a single model by ID from the :path header.
// Registered with a trailing slash (e.g. "/anthropic/v1/models/") and matched via
// the server's prefix-match fallback.
type anthropicModelRetrieveProcessor struct {
	passThroughProcessor
	logger *slog.Logger
	// Pre-serialized JSON responses keyed by model name.
	modelResponses map[string][]byte
}

var _ Processor = (*anthropicModelRetrieveProcessor)(nil)

// NewAnthropicModelRetrieveProcessor creates a processor for GET /anthropic/v1/models/{model_id}.
func NewAnthropicModelRetrieveProcessor(config *filterapi.RuntimeConfig, _ map[string]string, logger *slog.Logger, isUpstreamFilter bool, _ bool) (Processor, error) {
	if isUpstreamFilter {
		return passThroughProcessor{}, nil
	}

	models := deduplicateModels(config.DeclaredModels)

	modelResponses := make(map[string][]byte, len(models))
	for _, m := range models {
		body, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal anthropic model %q: %w", m.ID, err)
		}
		modelResponses[m.ID] = body
	}

	return &anthropicModelRetrieveProcessor{
		logger:         logger,
		modelResponses: modelResponses,
	}, nil
}

// ProcessRequestHeaders implements [Processor.ProcessRequestHeaders].
func (p *anthropicModelRetrieveProcessor) ProcessRequestHeaders(_ context.Context, headerMap *corev3.HeaderMap) (*extprocv3.ProcessingResponse, error) {
	// Extract model ID from the :path header.
	var requestPath string
	for _, h := range headerMap.Headers {
		if h.Key == ":path" {
			requestPath = string(h.RawValue)
			if requestPath == "" {
				requestPath = h.Value
			}
			break
		}
	}

	// Strip query parameters.
	if idx := strings.Index(requestPath, "?"); idx != -1 {
		requestPath = requestPath[:idx]
	}

	// The model ID is the last path segment.
	// e.g. "/anthropic/v1/models/my-model" → "my-model"
	lastSlash := strings.LastIndex(requestPath, "/")
	if lastSlash < 0 || lastSlash == len(requestPath)-1 {
		return immediateErrorResponse(typev3.StatusCode_NotFound, "not_found_error", "model ID required in path"), nil
	}
	modelID := requestPath[lastSlash+1:]

	body, ok := p.modelResponses[modelID]
	if !ok {
		return immediateErrorResponse(typev3.StatusCode_NotFound, "not_found_error", fmt.Sprintf("model not found: %s", modelID)), nil
	}
	return immediateJSONResponse(typev3.StatusCode_OK, body), nil
}

// --- Shared helpers ---

// deduplicateModels converts filterapi.Model entries to anthropicModelInfo,
// collapsing duplicates by name. The controller may create duplicate entries
// when multiple AIServiceBackends exist for the same model (multi-schema routing).
func deduplicateModels(declared []filterapi.Model) []anthropicModelInfo {
	seen := make(map[string]struct{}, len(declared))
	models := make([]anthropicModelInfo, 0, len(declared))
	for _, m := range declared {
		if _, dup := seen[m.Name]; dup {
			continue
		}
		seen[m.Name] = struct{}{}
		models = append(models, anthropicModelInfo{
			ID:          m.Name,
			Type:        "model",
			DisplayName: m.Name,
			CreatedAt:   m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return models
}

func immediateJSONResponse(status typev3.StatusCode, body []byte) *extprocv3.ProcessingResponse {
	headerMutation := &extprocv3.HeaderMutation{}
	setHeader(headerMutation, "content-length", fmt.Sprintf("%d", len(body)))
	setHeader(headerMutation, "content-type", "application/json")

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status:     &typev3.HttpStatus{Code: status},
				Headers:    headerMutation,
				Body:       body,
				GrpcStatus: &extprocv3.GrpcStatus{Status: uint32(codes.OK)},
			},
		},
	}
}

func immediateErrorResponse(status typev3.StatusCode, errorType string, message string) *extprocv3.ProcessingResponse {
	errResp := anthropicErrorResponse{
		Type:  "error",
		Error: anthropicErrorBody{Type: errorType, Message: message},
	}
	body, _ := json.Marshal(errResp)
	return immediateJSONResponse(status, body)
}
