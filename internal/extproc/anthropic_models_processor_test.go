// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package extproc

import (
	"log/slog"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
)

func TestAnthropicModels_List(t *testing.T) {
	now := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	cfg := &filterapi.RuntimeConfig{DeclaredModels: []filterapi.Model{
		{Name: "my-model", OwnedBy: "self-hosted", CreatedAt: now},
		{Name: "other-model", OwnedBy: "self-hosted", CreatedAt: now},
	}}

	p, err := NewAnthropicModelsProcessor(cfg, nil, slog.Default(), false, false)
	require.NoError(t, err)

	res, err := p.ProcessRequestHeaders(t.Context(), &corev3.HeaderMap{})
	require.NoError(t, err)

	ir, ok := res.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
	require.True(t, ok)
	require.Equal(t, typev3.StatusCode(200), ir.ImmediateResponse.Status.Code)

	respHeaders := headers(ir.ImmediateResponse.Headers.SetHeaders)
	require.Equal(t, "application/json", respHeaders["content-type"])

	var list anthropicModelList
	require.NoError(t, json.Unmarshal(ir.ImmediateResponse.Body, &list))
	require.Len(t, list.Data, 2)
	require.False(t, list.HasMore)
	require.Equal(t, "my-model", list.FirstID)
	require.Equal(t, "other-model", list.LastID)

	require.Equal(t, "my-model", list.Data[0].ID)
	require.Equal(t, "model", list.Data[0].Type)
	require.Equal(t, "my-model", list.Data[0].DisplayName)
	require.Equal(t, "2026-04-22T00:00:00Z", list.Data[0].CreatedAt)
}

func TestAnthropicModels_ListDeduplicates(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := &filterapi.RuntimeConfig{DeclaredModels: []filterapi.Model{
		{Name: "my-model", OwnedBy: "self-hosted", CreatedAt: now},
		{Name: "my-model", OwnedBy: "self-hosted", CreatedAt: now},
		{Name: "other-model", OwnedBy: "self-hosted", CreatedAt: now},
	}}

	p, err := NewAnthropicModelsProcessor(cfg, nil, slog.Default(), false, false)
	require.NoError(t, err)

	res, err := p.ProcessRequestHeaders(t.Context(), &corev3.HeaderMap{})
	require.NoError(t, err)

	ir := res.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
	var list anthropicModelList
	require.NoError(t, json.Unmarshal(ir.ImmediateResponse.Body, &list))
	require.Len(t, list.Data, 2, "duplicate 'my-model' should be collapsed")
	require.Equal(t, "my-model", list.Data[0].ID)
	require.Equal(t, "other-model", list.Data[1].ID)
}

func TestAnthropicModels_ListEmpty(t *testing.T) {
	cfg := &filterapi.RuntimeConfig{DeclaredModels: []filterapi.Model{}}

	p, err := NewAnthropicModelsProcessor(cfg, nil, slog.Default(), false, false)
	require.NoError(t, err)

	res, err := p.ProcessRequestHeaders(t.Context(), &corev3.HeaderMap{})
	require.NoError(t, err)

	ir := res.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
	var list anthropicModelList
	require.NoError(t, json.Unmarshal(ir.ImmediateResponse.Body, &list))
	require.Empty(t, list.Data)
	require.False(t, list.HasMore)
	require.Empty(t, list.FirstID)
	require.Empty(t, list.LastID)
}

func TestAnthropicModels_ListUpstreamPassthrough(t *testing.T) {
	cfg := &filterapi.RuntimeConfig{}
	p, err := NewAnthropicModelsProcessor(cfg, nil, slog.Default(), true, false)
	require.NoError(t, err)
	_, ok := p.(passThroughProcessor)
	require.True(t, ok)
}

func TestAnthropicModels_Retrieve(t *testing.T) {
	now := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	cfg := &filterapi.RuntimeConfig{DeclaredModels: []filterapi.Model{
		{Name: "my-model", OwnedBy: "self-hosted", CreatedAt: now},
		{Name: "other-model", OwnedBy: "self-hosted", CreatedAt: now},
	}}

	p, err := NewAnthropicModelRetrieveProcessor(cfg, map[string]string{":path": "/anthropic/v1/models/my-model"}, slog.Default(), false, false)
	require.NoError(t, err)

	t.Run("returns existing model", func(t *testing.T) {
		res, err := p.ProcessRequestHeaders(t.Context(), &corev3.HeaderMap{
			Headers: []*corev3.HeaderValue{{Key: ":path", RawValue: []byte("/anthropic/v1/models/my-model")}},
		})
		require.NoError(t, err)

		ir := res.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
		require.Equal(t, typev3.StatusCode(200), ir.ImmediateResponse.Status.Code)

		var model anthropicModelInfo
		require.NoError(t, json.Unmarshal(ir.ImmediateResponse.Body, &model))
		require.Equal(t, "my-model", model.ID)
		require.Equal(t, "model", model.Type)
		require.Equal(t, "my-model", model.DisplayName)
		require.Equal(t, "2026-04-22T00:00:00Z", model.CreatedAt)
	})

	t.Run("returns 404 for unknown model", func(t *testing.T) {
		res, err := p.ProcessRequestHeaders(t.Context(), &corev3.HeaderMap{
			Headers: []*corev3.HeaderValue{{Key: ":path", RawValue: []byte("/anthropic/v1/models/nonexistent")}},
		})
		require.NoError(t, err)

		ir := res.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
		require.Equal(t, typev3.StatusCode(404), ir.ImmediateResponse.Status.Code)

		var errResp anthropicErrorResponse
		require.NoError(t, json.Unmarshal(ir.ImmediateResponse.Body, &errResp))
		require.Equal(t, "error", errResp.Type)
		require.Equal(t, "not_found_error", errResp.Error.Type)
		require.Contains(t, errResp.Error.Message, "nonexistent")
	})

	t.Run("returns 404 for path with no model ID", func(t *testing.T) {
		res, err := p.ProcessRequestHeaders(t.Context(), &corev3.HeaderMap{
			Headers: []*corev3.HeaderValue{{Key: ":path", RawValue: []byte("/anthropic/v1/models/")}},
		})
		require.NoError(t, err)

		ir := res.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
		require.Equal(t, typev3.StatusCode(404), ir.ImmediateResponse.Status.Code)
	})

	t.Run("strips query params from path", func(t *testing.T) {
		res, err := p.ProcessRequestHeaders(t.Context(), &corev3.HeaderMap{
			Headers: []*corev3.HeaderValue{{Key: ":path", RawValue: []byte("/anthropic/v1/models/my-model?version=1")}},
		})
		require.NoError(t, err)

		ir := res.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
		require.Equal(t, typev3.StatusCode(200), ir.ImmediateResponse.Status.Code)
	})

	t.Run("deduplicates models", func(t *testing.T) {
		dupCfg := &filterapi.RuntimeConfig{DeclaredModels: []filterapi.Model{
			{Name: "dup-model", OwnedBy: "a", CreatedAt: now},
			{Name: "dup-model", OwnedBy: "b", CreatedAt: now},
		}}
		dp, err := NewAnthropicModelRetrieveProcessor(dupCfg, map[string]string{":path": "/anthropic/v1/models/dup-model"}, slog.Default(), false, false)
		require.NoError(t, err)

		res, err := dp.ProcessRequestHeaders(t.Context(), &corev3.HeaderMap{
			Headers: []*corev3.HeaderValue{{Key: ":path", RawValue: []byte("/anthropic/v1/models/dup-model")}},
		})
		require.NoError(t, err)

		ir := res.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
		require.Equal(t, typev3.StatusCode(200), ir.ImmediateResponse.Status.Code)
	})
}

func TestAnthropicModels_RetrieveUpstreamPassthrough(t *testing.T) {
	cfg := &filterapi.RuntimeConfig{}
	p, err := NewAnthropicModelRetrieveProcessor(cfg, nil, slog.Default(), true, false)
	require.NoError(t, err)
	_, ok := p.(passThroughProcessor)
	require.True(t, ok)
}
