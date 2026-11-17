# Codex integration streaming results

The Codex integration keeps the provider HTTP status. An HTTP-200 SSE failure
returns `success: false`; consumers must check `success`, not just `status`.
The existing `data.error` string remains available for older callers. New
callers can inspect `data.error_details` without matching prose:

```json
{
  "success": false,
  "status": 200,
  "data": {
    "error": "Our servers are currently overloaded. Please try again later.",
    "error_details": {
      "event_type": "error",
      "code": "server_is_overloaded",
      "type": "service_unavailable_error",
      "message": "Our servers are currently overloaded. Please try again later.",
      "param": null,
      "request_id": "req_example",
      "retry_headers": {"Retry-After": "3"}
    },
    "timing": {"request_duration_ms": 125.5, "terminal_event_ms": 124.8}
  },
  "headers": {"Retry-After": "3", "X-Request-Id": "req_example"}
}
```

## Diagnostic fields

`error_details` preserves available `event_type`, upstream `code`, `type`,
`message`, `param`/`parameter`, `request_id`, `response_id`, `response_status`,
and `incomplete_reason`. Optional scalar retry/reset hints keep their upstream
names (`retry_after`, `retry_after_ms`, `retry_after_seconds`, `retry_at`,
`reset_at`, `reset_after`, `reset_after_ms`, `reset_after_seconds`, `resets_at`,
`resets_in_seconds`, `reset_seconds`).
Retry/reset headers are allowlisted in `headers` and `error_details.retry_headers`.
Provider values are not converted into a retry decision or a synthetic HTTP status.

The SDK already carries `data` as JSON and forwards `headers`, so these fields
are available without a new SDK version. Failure data excludes partial output,
prompts, credentials, and arbitrary response fields. The adapter adds no request
or response-body logging.

## Completion and timing

Streaming success requires a valid `response.completed` event with a response
object. A present status must be `completed`, and an error cannot coexist with
success. Partial text, `response.failed`, `response.incomplete`, malformed frames,
missing completion, and interrupted reads remain failures. The 10 MB limit stays
in place. Multiline SSE data and CRLF are supported.

`data.timing.request_duration_ms` measures the HTTP call including body consumption
and decoding. `terminal_event_ms` measures time from the same start to the first
explicit terminal frame, before waiting for EOF. It is omitted when no terminal
frame was observed. Timing also survives successful chat/image normalization.

## Consumer retry policy

The adapter does not add retries. Media can retain its existing bounded attempts,
backoff, `gpt-6.1-sol` model and low reasoning setting:

1. Check `success`; on failure inspect `error_details.code` and `type`.
2. Treat explicit overload/service-unavailable codes as transient. Apply bounded,
   jittered backoff within the consumer's existing attempts and elapsed-time budget.
3. Honor provider retry/reset hints when valid. `Retry-After` may be seconds or
   an HTTP date; `retry_after_ms` is milliseconds. Reset headers retain provider
   units and need provider-aware parsing. If the required wait exceeds the budget,
   stop/defer rather than retrying earlier than requested.
4. Explicit authentication, exhausted quota, invalid-input, and content-filter
   failures stop the current retry loop even when they also carry a retry hint.
   For example, `invalid_api_key`, `insufficient_quota`, `invalid_request_error`,
   and `subscription_sharing_usage_limit_exceeded` need user/account/input action.
5. Do not treat incomplete or missing-completion output as a completed description.

This change preserves diagnosis and recovery information; it does not remedy
provider capacity shortages. It does not change Media code or deployed configuration.

The local Media checkout is version 0.13.57, while the reported production version
is 0.14.18; production retry behavior was not validated from this checkout.

Reference: [official OpenAI streaming events](https://developers.openai.com/api/reference/resources/responses/streaming-events).
