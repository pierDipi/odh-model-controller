# Plan: Add header-based model authorization to AuthPolicy

## Context

The `authpolicy_llm_isvc_userdefined.yaml` template currently authorizes inference requests by extracting namespace/name from the URL path and checking a SAR on `llminferenceservices` (verb `get`). We need an alternative authorization path based on the `X-Gateway-Model-Name` request header, which contains `publishers/<namespace>/models/<model-name>`. When this header is present, authorization should check a SAR on `llminferenceservices-models` (verb `post`) instead.

Kuadrant AuthPolicy uses AND semantics between authorization rules — all firing rules must pass. To achieve OR (mutual exclusion), the path-based and header-based rules use complementary `when` predicates so only one set fires per request.

## Files to modify

1. `internal/controller/resources/template/authpolicy_llm_isvc_userdefined.yaml` — the template
2. `internal/controller/resources/authpolicy_test.go` — unit tests

No changes needed to `authpolicy.go` or `options.go` (no new template variables).

## Changes

### 1. Template: add `when` predicates + two new rules

**Modify `inference-access`** — add predicate to skip when header is present:
```yaml
- predicate: "!('x-gateway-model-name' in request.headers)"
```

**Modify `inference-access-delegate`** — add same exclusion predicate (3rd predicate):
```yaml
- predicate: "!('x-gateway-model-name' in request.headers)"
```

**Add `inference-access-model`** — header-based SAR check:
- `when`: batch path exclusion + `'x-gateway-model-name' in request.headers`
- SAR: group `serving.kserve.io`, resource `llminferenceservices-models`, verb `post`
- namespace: `request.headers['x-gateway-model-name'].split('/')[1]`
- name: `request.headers['x-gateway-model-name'].split('/', 4)[3]` (robust: handles slashes in model name)
- user/groups: same conditional x-maas-user logic as `inference-access`

**Add `inference-access-model-delegate`** — header-based delegate SAR check:
- `when`: batch path exclusion + header present + `x-maas-user` present
- SAR: group `serving.kserve.io`, resource `llminferenceservices-models/delegate`, verb `post-delegate`
- namespace/name: from header (same expressions as `inference-access-model`)
- user/groups: authenticated caller identity (same as `inference-access-delegate`)

### 2. Scenario matrix

| Scenario | `inference-access` | `inference-access-delegate` | `inference-access-model` | `inference-access-model-delegate` | Result |
|---|---|---|---|---|---|
| No header, no delegation | Check path SAR | Skip | Skip | Skip | Pass if path `get` ok |
| No header, delegation | Check path SAR | Check delegate | Skip | Skip | Both must pass |
| Header, no delegation | Skip | Skip | Check header SAR | Skip | Pass if model `post` ok |
| Header, delegation | Skip | Skip | Check header SAR | Check model-delegate | Both must pass |
| Batch path (any combo) | Skip | Skip | Skip | Skip | Authn-only |

### 3. Tests

Add 5 test cases in `authpolicy_test.go` inside existing `AuthPolicyTemplateLoader` describe block:
1. Verify 4 authorization rule keys exist
2. Verify `inference-access` has model-name header exclusion predicate
3. Verify `inference-access-delegate` has 3 predicates including header exclusion
4. Verify `inference-access-model` SAR config (resource, namespace/name expressions, verb)
5. Verify `inference-access-model-delegate` SAR config

### 4. CEL expression for model name extraction

`split('/', 4)[3]` uses the two-argument CEL strings extension `split(separator, limit)`. For `publishers/my-ns/models/org/my-model`, this produces `["publishers", "my-ns", "models", "org/my-model"]` — index 3 gives the full model name with slashes preserved. The same extension library that provides `split(string)` (already used in the template) provides `split(string, int)`.

## Verification

1. `go test ./internal/controller/resources/...` — unit tests pass
2. Deploy to OCP cluster and verify the AuthPolicy is accepted by Kuadrant
3. Test the `split('/', 4)` CEL expression works in Authorino's CEL evaluator