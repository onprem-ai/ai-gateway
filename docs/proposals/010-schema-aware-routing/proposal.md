# Schema-Aware Routing

## Table of Contents

<!-- toc -->

- [Summary](#summary)
- [Motivation](#motivation)
- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Background](#background)
- [Design](#design)
  - [New Header: `x-ai-eg-schema`](#new-header-x-ai-eg-schema)
  - [Request Flow](#request-flow)
  - [AIGatewayRoute Example](#aigatewayroute-example)
- [Implementation](#implementation)
  - [Phase 1: Schema Header](#phase-1-schema-header-implemented)
  - [Phase 2: Anthropic Models Endpoints](#phase-2-anthropic-models-endpoints)
- [Usage](#usage)
  - [AIGatewayRoute with Schema Matching](#aigatewayroute-with-schema-matching)
  - [Client Usage](#client-usage)
  - [Backwards Compatibility](#backwards-compatibility)
- [Alternatives Considered](#alternatives-considered)
- [Prior Art](#prior-art)

<!-- /toc -->

## Summary

This proposal adds a second routing dimension — the client's API schema — to the ext-proc
filter. Today the ext-proc sets only `x-ai-eg-model` after parsing the request body.
A new `x-ai-eg-schema` header (e.g. `openai`, `anthropic`, `cohere`) is set alongside
the model header, enabling AIGatewayRoute rules to differentiate requests for the same
model across different API schemas.

## Motivation

Modern self-hosted inference engines like vLLM (v0.11+) serve both the OpenAI and Anthropic
API schemas on the same port. This means a single model deployment can accept requests at
`/v1/chat/completions` (OpenAI) and `/v1/messages` (Anthropic) simultaneously — clients
can use whichever SDK they prefer.

The Envoy AI Gateway doesn't support this out of the box. Its routing is single-dimensional:
it matches on the model name (`x-ai-eg-model`) but has no concept of which API schema the
client is using. When two AIGatewayRoute rules exist for the same model — one for OpenAI
and one for Anthropic — they're indistinguishable to the router.

**Without schema-aware routing**, operators must work around this by creating artificial
model name variants (e.g. `my-model` for OpenAI and `my-model-anthropic` for Anthropic).
This pollutes the `/v1/models` list, confuses SDK clients that expect the real model name,
and requires clients to know about the naming convention.

**With schema-aware routing**, the same model name works across both SDKs transparently —
clients use their preferred SDK, and the gateway routes to the correct backend based on
both the model name and the API schema. See [Usage](#usage) for concrete examples.

The Anthropic SDK also expects `client.models.list()` and `client.models.retrieve("id")`
to work. Phase 2 adds these endpoints so the Anthropic SDK's model discovery works the
same way the OpenAI SDK's already does.

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

## Usage

Schema-aware routing requires no additional infrastructure — the ext-proc sets the
`x-ai-eg-schema` header automatically based on the request path. The only change
needed is in the AIGatewayRoute definition.

### AIGatewayRoute with Schema Matching

Add `x-ai-eg-schema` header matches to route the same model name to different
backends (or the same backend) depending on which API schema the client uses:

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
        - name: my-model-openai-backend
    - matches:
        - headers:
            - type: Exact
              name: x-ai-eg-model
              value: my-model
            - type: Exact
              name: x-ai-eg-schema
              value: anthropic
      backendRefs:
        - name: my-model-anthropic-backend
```

If the inference engine (e.g. vLLM v0.11+) serves both OpenAI and Anthropic schemas
on the same port, both rules can reference the same AIServiceBackend.

### Client Usage

Clients use the standard SDK paths — no special configuration beyond `base_url`:

```python
# OpenAI SDK — requests go to /v1/chat/completions
from openai import OpenAI
client = OpenAI(base_url="https://gateway.example.com/v1", api_key="sk-...")
client.models.list()
client.chat.completions.create(model="my-model", messages=[...])

# Anthropic SDK — requests go to /anthropic/v1/messages
from anthropic import Anthropic
client = Anthropic(base_url="https://gateway.example.com/anthropic", api_key="sk-...")
client.models.list()
client.messages.create(model="my-model", messages=[...])
```

Both SDKs use the same model name. The ext-proc determines the schema from the
request path and sets `x-ai-eg-schema` accordingly:

| Request path | Schema header value |
|---|---|
| `/v1/chat/completions` | `openai` |
| `/v1/completions` | `openai` |
| `/v1/embeddings` | `openai` |
| `/anthropic/v1/messages` | `anthropic` |
| `/v2/rerank` | `cohere` |

### Backwards Compatibility

Single-schema routes that only match on `x-ai-eg-model` continue to work unchanged.
The `x-ai-eg-schema` header is always set, but only matters when referenced in
AIGatewayRoute match rules.

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
