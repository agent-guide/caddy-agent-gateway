# Anthropic Messages Stream Transition Table

Status: migration contract

This table freezes the stream lifecycle required by
`anthropic-protocol-fidelity.md`. The executable copy lives in
`pkg/dispatcher/llmapi/anthropic/stream_contract_test.go`; adding an input kind
or state requires defining every cell before production code changes.

The response states are:

- `uncommitted`: provider execution has not opened;
- `stream_opened`: the provider stream exists, but no committing event was
  emitted;
- `idle`: `message_start` was emitted and no content block is open;
- `reasoning`, `text`, `tool_use`, `native_block`: exactly one downstream block
  of that kind is open;
- `completed` and `failed`: terminal states.

The transition classes are:

- `accept`: consume the input without changing response or block state;
- `start_message`: emit the sole `message_start` and enter `idle`;
- `open_*`: open the named block after closing any legally closable current
  block; incomplete tools or bounded buffers make this fail instead;
- `continue_block`: emit a delta for the currently open block;
- `close_block`: close the current block and enter `idle`;
- `complete`: close every legally closable block, emit the terminal message
  events, and finalize exactly once;
- `fail_http`: fail before commitment and retain the HTTP error path;
- `fail_sse`: report a post-commit protocol error and finalize exactly once;
- `drop_ping`: discard a pre-commit keepalive without committing;
- `emit_ping`: emit a post-commit keepalive without changing block state;
- `invalid_state`: fail closed and name the state/input conflict;
- `terminal_error`: reject any input after finalization.

Global rules applied in addition to the executable matrix:

1. `provider_opened` is the only normal transition out of `uncommitted`.
2. `message_start` is the only committing input in relay mode; normalized mode
   synthesizes it on the first meaningful generic output.
3. `ping` never commits a response.
4. At most one downstream block is open. A new block input must close the
   current block or fail; it may never overlap it.
5. Native deltas and stops must reference the currently open source index.
6. EOF is normal completion only after a message has started. Before commitment
   it is an HTTP failure.
7. Provider errors, cancellation, invalid state, and sink errors select exactly
   one terminal path. Last-known usage is retained; absent usage is not
   fabricated.
8. Deferred text and tool arguments are bounded. Overflow is `invalid_state`
   and the payload is never logged.

Phase 0 deliberately characterizes two existing violations rather than
changing production output: provider-open failure currently follows a committed
SSE error, and unmappable native block events are currently dropped. Phase 1
must replace those characterizations with the target transitions above.
