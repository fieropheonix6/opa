---
title: "Decision Logs"
---

OPA can periodically report decision logs to remote HTTP servers, using custom
plugins, or to the console output; or any combination thereof.
The decision logs contain events that describe policy queries. Each event includes
the policy that was queried, the input to the query, bundle metadata, and other
information that enables auditing and offline debugging of policy decisions.

When decision logging is enabled the OPA server will include a `decision_id`
field in API calls that return policy decisions.

See the [Configuration Reference](./configuration) for configuration details.

### Decision Log Service API

OPA expects the service to expose an API endpoint that will receive decision logs.

```http
POST /[<decision_logs.resource>] HTTP/1.1
Content-Encoding: gzip
Content-Type: application/json
```

The resource field is an optional configuration that can be used to route logs
to a specific endpoint in the service by defining the full path. If the resource path is not configured on the agent,
updates will be sent to `/logs`.

The message body contains a gzip compressed JSON array. Each array element (event)
represents a policy decision returned by OPA.

<EvergreenCodeBlock>
```json
[
  {
    "labels": {
      "app": "my-example-app",
      "id": "1780d507-aea2-45cc-ae50-fa153c8e4a5a",
      "version": "{{ current_version }}"
    },
    "decision_id": "4ca636c1-55e4-417a-b1d8-4aceb67960d1",
    "bundles": {
      "authz": {
        "revision": "W3sibCI6InN5cy9jYXRhbG9nIiwicyI6NDA3MX1d"
      }
    },
    "path": "http/example/authz/allow",
    "input": {
      "method": "GET",
      "path": "/salary/bob"
    },
    "result": "true",
    "requested_by": "[::1]:59943",
    "timestamp": "2018-01-01T00:00:00.000000Z"
  }
]
```
</EvergreenCodeBlock>

Decision log updates contain the following fields:

| Field                              | Type            | Description                                                                                                                                                                                                                                                                                                                                                                                             |
| ---------------------------------- | --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `[_].labels`                       | `object`        | Set of key-value pairs that uniquely identify the OPA instance.                                                                                                                                                                                                                                                                                                                                         |
| `[_].decision_id`                  | `string`        | Unique identifier generated for each decision for traceability.                                                                                                                                                                                                                                                                                                                                         |
| `[_].trace_id`                     | `string`        | Unique identifier of a trace generated for each incoming request for traceability. This is a hex string representation compliant with the W3C trace-context specification. See more at https://www.w3.org/TR/trace-context/#trace-id.                                                                                                                                                                   |
| `[_].span_id`                      | `string`        | Unique identifier of a span in a trace to assist traceability. This is a hex string representation compliant with the W3C trace-context specification. See more at https://www.w3.org/TR/trace-context/#parent-id.                                                                                                                                                                                      |
| `[_].bundles`                      | `object`        | Set of key-value pairs describing the bundles which contained policy used to produce the decision.                                                                                                                                                                                                                                                                                                      |
| `[_].bundles[_].revision`          | `string`        | Revision of the bundle at the time of evaluation.                                                                                                                                                                                                                                                                                                                                                       |
| `[_].path`                         | `string`        | Hierarchical policy decision path, e.g., `/http/example/authz/allow`. Receivers should tolerate slash-prefixed paths.                                                                                                                                                                                                                                                                                   |
| `[_].query`                        | `string`        | Ad-hoc Rego query received by Query API.                                                                                                                                                                                                                                                                                                                                                                |
| `[_].input`                        | `any`           | Input data provided in the policy query.                                                                                                                                                                                                                                                                                                                                                                |
| `[_].result`                       | `any`           | Policy decision returned to the client, e.g., `true` or `false`.                                                                                                                                                                                                                                                                                                                                        |
| `[_].requested_by`                 | `string`        | Identifier for client that executed policy query, e.g., the client address.                                                                                                                                                                                                                                                                                                                             |
| `[_].request_context.http.headers` | `object`        | Set of key-value pairs describing HTTP headers and their corresponding values. The header keys in this object are specified by the user as part of the decision log configuration. The values in this object represent a list of values associated with the given header key.                                                                                                                           |
| `[_].timestamp`                    | `string`        | RFC3999 timestamp of policy decision.                                                                                                                                                                                                                                                                                                                                                                   |
| `[_].metrics`                      | `object`        | Key-value pairs of [performance metrics](./rest-api#performance-metrics).                                                                                                                                                                                                                                                                                                                               |
| `[_].erased`                       | `array[string]` | Set of JSON Pointers specifying fields in the event that were erased.                                                                                                                                                                                                                                                                                                                                   |
| `[_].masked`                       | `array[string]` | Set of JSON Pointers specifying fields in the event that were masked.                                                                                                                                                                                                                                                                                                                                   |
| `[_].nd_builtin_cache`             | `object`        | Key-value pairs of non-deterministic builtin names, paired with objects specifying the input/output mappings for each unique invocation of that builtin during policy evaluation. Intended for use in debugging and decision replay. Receivers will need to decode the JSON using Rego's JSON decoders.                                                                                                 |
| `[_].req_id`                       | `number`        | Incremental request identifier, and unique only to the OPA instance, for the request that started the policy query. The attribute value is the same as the value present in others logs (request, response, and print) and could be used to correlate them all. This attribute will be included just when OPA runtime is initialized in server mode and the log level is equal to or greater than info. |

If the decision log was successfully uploaded to the remote service, it should respond with an HTTP 2xx status. If the
service responds with a non-2xx status, OPA will requeue the last chunk containing decision log events and upload it
during the next upload event. OPA also performs an exponential backoff to calculate the delay in uploading the next chunk
when the remote service responds with a non-2xx status.

OPA periodically uploads decision logs to the remote service. In order to conserve network and memory resources, OPA
attempts to fill up each message body with as many events as possible while respecting the user-specified
`upload_size_limit_bytes` config option. Each message body is a gzip compressed JSON array and the `upload_size_limit_bytes`
config option represents the gzip compressed size, it can be referred to as the compressed limit. To avoid compressing 
each incoming event to get its compressed size to see if the compressed limit is reached, OPA tries to make an educated
guess what the uncompressed limit could be. It does so by using an adaptive limit, referred to as the uncompressed limit,
that gets adjusted by measuring incoming events. This does mean that initially the chunk sizes will most likely be smaller
than the compressed limit, but as OPA consumes more decision events it will adjust the adaptive uncompressed limit to
optimize the messages. The algorithm to adjust the uncompressed limit uses the following criteria:

`Scale Up`: If the current chunk size is below 90% of the user-configured compressed limit, exponentially increase the
uncompressed limit. The exponential function is 2^x where x has a minimum value of 1

`Scale Down`: If the current chunk size exceeds the compressed limit, decrease the uncompressed limit and re-encode the 
decisions in the last chunk.

`Equilibrium`: If the chunk size is between 90% and 100% of the user-configured limit, maintain uncompressed limit value.

When an event containing `nd_builtin_cache` cannot fit into a chunk smaller than `upload_size_limit_bytes`, OPA will
drop the `nd_builtin_cache` key from the event, and will retry encoding the chunk without the non-deterministic
builtins cache information. This best-effort approach ensures that OPA reports decision log events as much as possible,
and bounds how large decision log events can get. This size-bounding is necessary, because some non-deterministic builtins
(such as `http.send`) can increase the decision log event size by a potentially unbounded amount.

### Local Decision Logs

Local console logging of decisions can be enabled via the `console` config option.
This does not require any remote server. Example of minimal config to enable:

```yaml
decision_logs:
  console: true
```

This will dump all decisions to the console. See
[Configuration Reference](./configuration) for more details.

### Masking Sensitive Data

Policy queries may contain sensitive information in the `input` document that
must be removed or modified before decision logs are uploaded to the remote API
(e.g., usernames, passwords, etc.) Similarly, parts of the policy decision itself may
be considered sensitive.

By default, OPA queries the `data.system.log.mask` path prior to encoding and
uploading decision logs or calling custom decision log plugins.

OPA provides the decision log event as input to the policy query and expects
the query to return a set of JSON Pointers that refer to fields in the decision
log event to either **erase** or **modify**.

For example, assume OPA is queried with the following `input` document:

```json
{
  "resource": "user",
  "name": "bob",
  "password": "passw0rd"
}
```

To **remove** the `password` field from decision log events related to "user"
resources, supply the following policy to OPA:

```ruby
package system.log

mask contains "/input/password" if {
	# OPA provides the entire decision log event as input to the masking policy.
	# Refer to the original input document under input.input.
	input.input.resource == "user"
}

# To mask certain fields unconditionally, omit the rule body.
mask contains "/input/ssn"
```

When the masking policy generates one or more JSON Pointers, they will be erased
from the decision log event. The erased paths are recorded on the event itself:

```json
{
  "decision_id": "b4638167-7fcb-4bc7-9e80-31f5f87cb738",
  "erased": [
    "/input/password",
    "/input/ssn"
  ],
  "input": {
    "name": "bob",
    "resource": "user"
  },
------------------------- 8< -------------------------
  "path": "system/main",
  "requested_by": "127.0.0.1:36412",
  "result": true,
  "timestamp": "2019-06-03T20:07:16.939402185Z"
}
```

There are a few restrictions on the JSON Pointers that OPA will erase:

- Pointers must be prefixed with `/input`, `/result`, or `/nd_builtin_cache`.
- Pointers may point to undefined data. For example `/input/name/first` in the
  example above would be undefined. Masking operations on undefined pointers are
  ignored.
- Pointers can also refer to arrays both as part of the path and as the last
  element in the path. For example, both `/input/users/0/name` and
  `/input/users/0` would be valid.

In order to **modify** the contents of an input field, the **mask** rule may utilize the following format.

- `"op"` -- The operation to apply when masking. All operations are done at the
  path specified. Valid options include:

| op         | Description                                                                                                                                                     |
| ---------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `"remove"` | The `"path"` specified will be removed from the resulting log message. The `"value"` mask field is ignored for `"remove"` operations.                           |
| `"upsert"` | The `"value"` will be set at the specified `"path"`. If the field exists it is overwritten, if it does not exist it will be added to the resulting log message. |

- `"path"` -- A JSON pointer path to the field to perform the operation on.

Optional Fields:

- `"value"` -- Only required for `"upsert"` operations.

> This is processed for every decision being logged, so be mindful of
> performance when performing complex operations in the mask body, e.g. crypto
> operations

```ruby
package system.log

mask contains {"op": "upsert", "path": "/input/password", "value": "**REDACTED**"} if {
	# conditionally upsert password if it existed in the original event
	input.input.password
}
```

To always **upsert** a value, even if it didn't exist in the original event,
the following rule format can be used.

```ruby
package system.log

# always upsert, no conditions in rule body
mask contains {"op": "upsert", "path": "/input/password", "value": "**REDACTED**"}
```

The result of this mask operation on the decision log event produces
the following output. Notice that the **mask** event field exists
to track **remove** vs **upsert** mask operations.

```json
{
  "decision_id": "b4638167-7fcb-4bc7-9e80-31f5f87cb738",
  "erased": [
    "/input/ssn"
  ],
  "masked": [
    "/input/password"
  ],
  "input": {
    "name": "bob",
    "resource": "user",
    "password": "**REDACTED**"
  },
------------------------- 8< -------------------------
  "path": "system/main",
  "requested_by": "127.0.0.1:36412",
  "result": true,
  "timestamp": "2019-06-03T20:07:16.939402185Z"
}
```

### Drop Decision Logs

Drop rules filters all decisions from logging where the rule evaluates to `true`.

This rule will drop all requests to the _allow_ rule in the _kafka_ package, that returned _true_:

```rego
package system.log

drop if {
	input.path == "kafka/allow"
	input.result == true
}
```

Log only requests for _delete_ and _alter_ operations
(Kafka with the [opa-kafka-plugin](https://github.com/StyraInc/opa-kafka-plugin)):

```rego
package system.log

drop if {
	input.path == "kafka/allow"
	not input.input.action.operation in {"DELETE", "ALTER"}
}
```

The name of the drop rules by default is `drop` in the package `system.log`. It can be changed with the configuration
property `decision_logs.drop_decision`.

```yaml
decision_logs:
  drop_decision: /system/log/drop
```

### Rate Limiting Decision Logs

There are scenarios where OPA may be uploading decisions faster than what the remote service is able to consume. Although
OPA provides a user-specified buffer size limit in bytes, it may be difficult to determine the ideal buffer size that will
allow the service to consume logs without being overwhelmed. The `max_decisions_per_second` config option allows users
to set the maximum number of decision log events to buffer per second. OPA will drop events if the rate limit is exceeded.
This option provides users more control over how OPA buffers log events and is an effective mechanism to make sure the
service can successfully process incoming log events.

## Ecosystem Projects

Decision Logging is an important feature of OPA which supports, in particular, auditing and debugging. The following OPA
ecosystem projects implement functionality related to Decision Logging:

<EcosystemEmbed feature="decision-logging">
These projects implement decision logging functionality.
</EcosystemEmbed>
