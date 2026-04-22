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
  - [Deployment](#deployment)
- [Usage](#usage)
  - [Option A: Custom Ext-Proc (Recommended)](#option-a-custom-ext-proc-recommended)
  - [Option B: Lua Filter (Without Fork)](#option-b-lua-filter-without-fork)
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

**With schema-aware routing**, the same model name works across both SDKs transparently:

```python
# OpenAI SDK — uses /v1/chat/completions
from openai import OpenAI
client = OpenAI(base_url="https://gateway.example.com/v1", api_key="sk-...")
client.chat.completions.create(model="my-model", messages=[...])

# Anthropic SDK — uses /anthropic/v1/messages
from anthropic import Anthropic
client = Anthropic(base_url="https://gateway.example.com/anthropic", api_key="sk-...")
client.messages.create(model="my-model", messages=[...])

# Both route to the same backend, same model name, no workarounds.
```

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

### Deployment

The ext-proc changes require a custom binary. The image is built from this fork and
published to `ghcr.io/onprem-ai/ai-gateway-extproc`. The AI Gateway Helm values are
updated to reference this image for the ext-proc container.

## Usage

### Option A: Custom Ext-Proc (Recommended)

This approach builds the schema header directly into the ext-proc binary, so it's set
atomically alongside the model header in `ProcessRequestBody`. No additional Envoy
filters needed.

1. **Build the fork** — clone the `feat/schema-aware-routing` branch from this repo
   and build the ext-proc Docker image:

   ```bash
   git clone https://github.com/onprem-ai/ai-gateway.git -b feat/schema-aware-routing
   cd ai-gateway
   docker build -f Dockerfile.extproc \
     --build-arg VERSION=v0.5.0-0-g0000000 \
     -t your-registry/ai-gateway-extproc:schema-routing .
   docker push your-registry/ai-gateway-extproc:schema-routing
   ```

2. **Override the ext-proc image** in Helm values:

   ```yaml
   # helmrelease values for envoy-ai-gateway
   extProc:
     image:
       repository: your-registry/ai-gateway-extproc
       tag: "schema-routing"
   ```

3. **Create an AIGatewayRoute with dual-header matching** — one rule per schema,
   both pointing to the same model name but different AIServiceBackends (or the same
   backend if the inference engine handles both schemas):

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

   If the inference engine (e.g. vLLM) serves both schemas on the same port, both
   rules can reference the same AIServiceBackend.

### Option B: Lua Filter (Without Fork)

If you can't use the custom ext-proc image, you can approximate schema-aware routing
with a Lua filter injected via EnvoyPatchPolicy. The Lua filter runs before the ext-proc
and sets a custom header based on the request path.

**Important caveat:** The ext-proc's `ClearRouteCache: true` causes Envoy to re-evaluate
routing after body processing. Headers set by Lua before the ext-proc *may not survive*
this re-evaluation depending on your Envoy version and filter chain configuration. Test
thoroughly before relying on this in production.

1. **Create an EnvoyPatchPolicy** that injects a Lua filter at position [0]:

   ```yaml
   apiVersion: gateway.envoyproxy.io/v1alpha1
   kind: EnvoyPatchPolicy
   metadata:
     name: schema-header-injection
     namespace: envoy-gateway-system
   spec:
     type: JSONPatch
     targetRef:
       group: gateway.networking.k8s.io
       kind: Gateway
       name: ai-gateway
     jsonPatches:
       - type: "type.googleapis.com/envoy.config.listener.v3.Listener"
         name: "envoy-gateway-system/ai-gateway/http"
         operation:
           op: add
           path: "/default_filter_chain/filters/0/typed_config/http_filters/0"
           value:
             name: "envoy.filters.http.lua"
             typed_config:
               "@type": "type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua"
               default_source_code:
                 inline_string: |
                   function envoy_on_request(request_handle)
                     local path = request_handle:headers():get(":path")
                     if path and path:find("^/anthropic/") then
                       request_handle:headers():add("x-ai-eg-schema", "anthropic")
                     elseif path and path:find("^/v1/") then
                       request_handle:headers():add("x-ai-eg-schema", "openai")
                     end
                   end
   ```

2. **Create AIGatewayRoute rules** with `x-ai-eg-schema` header matching (same as
   Option A, step 3).

3. **Requires** `enableEnvoyPatchPolicy: true` in the Envoy Gateway Helm config.

**Why Option A is preferred:** The ext-proc sets `x-ai-eg-schema` in the same gRPC
response as `x-ai-eg-model` and `ClearRouteCache`, so both headers are guaranteed
to be present when Envoy re-evaluates routing. The Lua approach sets the header in
a separate filter stage, creating a timing dependency on whether Envoy preserves
pre-ext-proc headers across route cache clears.

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
