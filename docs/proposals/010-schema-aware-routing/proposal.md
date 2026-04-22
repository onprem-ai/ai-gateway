# Schema-Aware Routing

## Table of Contents

<!-- toc -->

- [Summary](#summary)
- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Background](#background)
- [Design](#design)
  - [New Header: `x-ai-eg-schema`](#new-header-x-ai-eg-schema)
  - [Request Flow](#request-flow)
  - [AIGatewayRoute Example](#aigatewayroute-example)
- [Implementation](#implementation)
  - [Ext-Proc Changes](#ext-proc-changes)
  - [Deployment](#deployment)
- [Alternatives Considered](#alternatives-considered)
- [Prior Art](#prior-art)

<!-- /toc -->

## Summary

This proposal adds a second routing dimension — the client's API schema — to the ext-proc
filter. Today the ext-proc sets only `x-ai-eg-model` after parsing the request body.
A new `x-ai-eg-schema` header (e.g. `openai`, `anthropic`, `cohere`) is set alongside
the model header, enabling AIGatewayRoute rules to differentiate requests for the same
model across different API schemas.

## Goals

- Allow the same model name to be exposed via multiple API schemas (e.g. OpenAI and Anthropic)
  through a single AIGatewayRoute, each routing to a different AIServiceBackend.
- Preserve full backwards compatibility — the schema header is additive and existing
  single-schema routes continue to work without changes.
- Keep the change surface minimal by reusing information the ext-proc already has
  (the processor factory selected by request path).

## Non-Goals

- Modifying the controller's filter config model registry to be schema-aware.
  The `/v1/models` endpoint will continue to list models without schema differentiation.
- Automatic schema detection from request body content. Schema is determined by the
  request path (which processor factory is selected), not by inspecting the JSON payload.
- Supporting schema-based routing without the endpoint prefix convention. Clients must
  still use the appropriate URL path (e.g. `/v1/chat/completions` for OpenAI,
  `/anthropic/v1/messages` for Anthropic).

## Background

The ext-proc registers a separate processor factory per API endpoint path:

- `/v1/chat/completions` → `ChatCompletionsEndpointSpec` (OpenAI)
- `/v1/messages` → `MessagesEndpointSpec` (Anthropic)
- `/v2/rerank` → `RerankEndpointSpec` (Cohere)

Each factory knows which API schema it handles. During `ProcessRequestBody`, the ext-proc
extracts the model name from the request and sets `x-ai-eg-model`, then returns
`ClearRouteCache: true` so Envoy re-evaluates routing with the new header.

The problem: routing operates on a single dimension (`x-ai-eg-model`). When a self-hosted
backend like vLLM handles both OpenAI and Anthropic natively on the same port, two
AIGatewayRoute rules for the same model name are indistinguishable. Users must create
separate model names (e.g. `my-model` and `my-model-anthropic`), which pollutes the
`/v1/models` list and confuses consumers.

The HTTPRoute generation already passes all header matches through to the generated
HTTPRoute, so adding a second header dimension requires no controller changes for routing.

## Design

### New Header: `x-ai-eg-schema`

A new header following the existing `x-ai-eg-*` convention:

| Header | Set by | Value | Purpose |
|--------|--------|-------|---------|
| `x-ai-eg-model` | ext-proc (existing) | Model name from request body | Model-based routing |
| `x-ai-eg-schema` | ext-proc (new) | `openai`, `anthropic`, `cohere` | Schema-based routing |

Both headers are set atomically in the same `ProcessRequestBody` response, eliminating
timing or filter-ordering issues.

### Request Flow

```
Client Request
     │
     ▼
┌─────────────────────────────────────┐
│  Envoy receives request             │
│  Path: /v1/chat/completions         │
│  or:   /anthropic/v1/messages       │
└──────────────┬──────────────────────┘
               │
               ▼
┌─────────────────────────────────────┐
│  Ext-Proc: ProcessRequestBody       │
│  1. Select processor by path        │
│  2. Parse body, extract model name  │
│  3. Set x-ai-eg-model: my-model     │
│  4. Set x-ai-eg-schema: openai      │  ← NEW
│  5. ClearRouteCache: true           │
└──────────────┬──────────────────────┘
               │
               ▼
┌─────────────────────────────────────┐
│  Envoy re-evaluates HTTPRoute       │
│  Match on BOTH headers:             │
│  x-ai-eg-model + x-ai-eg-schema    │
│  → Routes to correct backend        │
└─────────────────────────────────────┘
```

### AIGatewayRoute Example

```yaml
apiVersion: aigateway.envoyproxy.io/v1alpha1
kind: AIGatewayRoute
metadata:
  name: my-model
spec:
  rules:
    - matches:
        - headers:
            - type: Exact
              name: x-ai-eg-model
              value: my-model
            - type: Exact
              name: x-ai-eg-schema
              value: openai
      backendRefs:
        - name: my-model-openai
    - matches:
        - headers:
            - type: Exact
              name: x-ai-eg-model
              value: my-model
            - type: Exact
              name: x-ai-eg-schema
              value: anthropic
      backendRefs:
        - name: my-model-anthropic
```

Single-schema routes (matching only on `x-ai-eg-model`) continue to work — the schema
header is present but ignored when not referenced in match rules.

## Implementation

### Phase 1: Schema Header (Implemented)

1. **New constant** `SchemaHeaderKey` (`x-ai-eg-schema`) in the `internalapi` package.
2. **Factory option** `WithSchemaName(name)` on `NewFactory` — passes the schema name
   through to the router processor via the functional options pattern.
3. **Header injection** in `ProcessRequestBody` — when a schema name is configured, append
   it as an additional header alongside `x-ai-eg-model`.
4. **Registration** — each `server.Register` call passes the appropriate schema name
   (`openai`, `anthropic`, `cohere`).

The change is backwards compatible: `WithSchemaName` is a variadic option, so existing
callers without it produce processors that omit the schema header.

### Phase 2: Anthropic Models Endpoints

The ext-proc already handles `GET /v1/models` by returning an `ImmediateResponse` with
the model list from the filter config. Two new processors extend this to the Anthropic
API schema:

1. **`GET /anthropic/v1/models`** — returns the model list in Anthropic's paginated format:
   ```json
   {
     "data": [
       {
         "id": "my-model",
         "type": "model",
         "display_name": "my-model",
         "created_at": "2026-01-01T00:00:00Z"
       }
     ],
     "has_more": false,
     "first_id": "my-model",
     "last_id": "my-model"
   }
   ```

2. **`GET /anthropic/v1/models/{model_id}`** — returns a single model's info, or
   HTTP 404 if the model is not in the filter config.

Both endpoints:
- Reuse `config.DeclaredModels` (same source as the OpenAI `/v1/models` processor).
- Return `ImmediateResponse` — no backend routing needed.
- Deduplicate models by name (the controller may list the same model multiple times
  when multiple AIServiceBackends exist for different schemas).
- Follow the existing `modelsProcessor` pattern (build response at instantiation,
  return on `ProcessRequestHeaders`).

### Deployment

The ext-proc changes require a custom binary. The image is built from this fork and
published to `ghcr.io/onprem-ai/ai-gateway-extproc`. The AI Gateway Helm values are
updated to reference this image for the ext-proc container.

## Alternatives Considered

**Composite model name** — Encode the schema into the model header value (e.g.
`my-model::anthropic`). Rejected because it pollutes the `/v1/models` endpoint, requires
body mutation to restore the original model name, and adds parsing complexity.

**Path-based pre-routing** — Route by URL path prefix before the ext-proc runs, using
separate HTTPRoute rules per schema. Rejected because it requires major architectural
changes to AIGatewayRoute and breaks the model-based routing paradigm.

**Lua filter injection** — Use an EnvoyPatchPolicy Lua filter to set a schema header
before the ext-proc. Tested and failed — headers set by Lua may not survive the
ext-proc's `ClearRouteCache` re-evaluation. Also fragile due to filter ordering
dependencies.

## Prior Art

- [GitHub Issue #695](https://github.com/envoyproxy/ai-gateway/issues/695) —
  "Request override based on model + tenant in AIGatewayRoute" describes the same
  need for additional routing dimensions beyond model name. Open since June 2025.

- **vLLM** (v0.11+) handles both OpenAI and Anthropic APIs on the same process by
  dispatching to schema-specific handlers based on URL path — the same pattern the
  ext-proc already uses internally. vLLM's `AnthropicServingMessages` extends
  `OpenAIServingChat` and converts between formats transparently. This proposal
  surfaces the same path-derived schema information that the ext-proc already has,
  bridging the gap between internal knowledge and routing capability.
