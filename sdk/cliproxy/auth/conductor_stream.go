package auth

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	return &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func (m *Manager) firstChunkTimeout() time.Duration {
	if m == nil {
		return internalconfig.ResolveFirstChunkTimeout(0)
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return internalconfig.ResolveFirstChunkTimeout(0)
	}
	return internalconfig.ResolveFirstChunkTimeout(cfg.Streaming.FirstChunkTimeoutSeconds)
}

func validateStreamResult(result *cliproxyexecutor.StreamResult, err error) (*cliproxyexecutor.StreamResult, error) {
	if err != nil {
		return result, err
	}
	if result == nil || result.Chunks == nil {
		return result, &Error{Code: "empty_stream", Message: "upstream stream has no source", Retryable: true}
	}
	return result, nil
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	return readStreamBootstrapWithTimeout(ctx, ch, 0)
}

func readStreamBootstrapWithTimeout(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk, timeout time.Duration) ([]cliproxyexecutor.StreamChunk, bool, error) {
	return readStreamBootstrapWithTimeoutFor(ctx, ch, timeout, "")
}

// readStreamBootstrapWithTimeoutFor is like readStreamBootstrapWithTimeout but,
// when format is known, keeps buffering protocol-metadata payloads (e.g. Anthropic
// message_start, OpenAI role deltas) until the stream produces client-visible
// content. A stream that dies before producing content is therefore reported as a
// retryable bootstrap failure instead of being committed to the client.
func readStreamBootstrapWithTimeoutFor(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk, timeout time.Duration, format sdktranslator.Format) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	var timeoutC <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timeoutC = timer.C
		defer timer.Stop()
	}
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-timeoutC:
				return nil, false, &Error{
					Code:       "first_chunk_timeout",
					Message:    fmt.Sprintf("upstream stream produced no payload within %s", timeout),
					Retryable:  true,
					HTTPStatus: http.StatusGatewayTimeout,
				}
			case chunk, ok = <-ch:
			}
		} else if timeoutC != nil {
			select {
			case <-timeoutC:
				return nil, false, &Error{
					Code:       "first_chunk_timeout",
					Message:    fmt.Sprintf("upstream stream produced no payload within %s", timeout),
					Retryable:  true,
					HTTPStatus: http.StatusGatewayTimeout,
				}
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			if format != "" && len(buffered) > 0 && !streamContentBuffered(format, buffered) {
				// The stream ended after protocol metadata but before producing any
				// content; report a retryable bootstrap failure instead of committing
				// an empty response to the client.
				return nil, true, &Error{
					Code:       "empty_stream",
					Message:    "upstream stream closed before producing content",
					Retryable:  true,
					HTTPStatus: http.StatusBadGateway,
				}
			}
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 && isStreamContentPayload(format, chunk.Payload) {
			return buffered, false, nil
		}
	}
}

// isStreamContentPayload reports whether a stream payload carries client-visible
// model output for the given response format. Payloads that only carry protocol
// metadata (Anthropic message_start, OpenAI role/usage deltas, etc.) are not
// content, so a stream that dies before producing content can be retried without
// duplicating anything the client has already seen. Unknown formats fall back to
// treating the first payload as content (legacy behavior).
func isStreamContentPayload(format sdktranslator.Format, payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	switch format {
	case sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGemini:
		data := sseDataPayload(payload)
		if data == nil {
			// No parseable JSON data line (raw passthrough, [DONE], split SSE
			// frames); treat the payload as content so the retry window is never
			// widened on unclassifiable data.
			return true
		}
		payload = data
	}
	switch format {
	case sdktranslator.FormatClaude:
		switch gjson.GetBytes(payload, "type").String() {
		case "content_block_delta", "message_stop":
			// Content deltas and the protocol terminal marker both commit the
			// stream; a completed (even empty) upstream response must not be
			// re-executed.
			return true
		}
		return false
	case sdktranslator.FormatOpenAI:
		return gjson.GetBytes(payload, "choices.0.delta.content").String() != "" ||
			gjson.GetBytes(payload, "choices.0.delta.reasoning_content").String() != ""
	case sdktranslator.FormatOpenAIResponse:
		switch gjson.GetBytes(payload, "type").String() {
		case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.completed", "response.done":
			return true
		}
		return false
	case sdktranslator.FormatGemini:
		return gjson.GetBytes(payload, "candidates.0.content.parts.0.text").String() != ""
	default:
		return true
	}
}

// sseDataPayload extracts the JSON payload of the first SSE data line from a
// chunk that may carry event:/id:/data: lines (a complete event frame or raw
// upstream lines). Returns nil when the chunk carries no parseable JSON data.
func sseDataPayload(chunk []byte) []byte {
	for _, rawLine := range bytes.Split(chunk, []byte("\n")) {
		line := bytes.TrimSpace(rawLine)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) > 0 && gjson.ValidBytes(data) {
			return data
		}
	}
	return nil
}

func streamContentBuffered(format sdktranslator.Format, buffered []cliproxyexecutor.StreamChunk) bool {
	for _, chunk := range buffered {
		if isStreamContentPayload(format, chunk.Payload) {
			return true
		}
	}
	return false
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, aliasResult OAuthModelAliasResult, ephemeralResult bool) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var failed bool
		forward := true
		var rewriter *StreamRewriter
		if aliasResult.ForceMapping && strings.TrimSpace(aliasResult.OriginalAlias) != "" {
			rewriter = NewStreamRewriter(StreamRewriteOptions{RewriteModel: aliasResult.OriginalAlias})
		}
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				rerr := resultErrorFromError(chunk.Err)
				m.recordExecutionResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}, auth, ephemeralResult)
			}
			if !forward {
				return false
			}
			if chunk.Err != nil {
				if ctx == nil {
					out <- chunk
					return true
				}
				select {
				case <-ctx.Done():
					forward = false
					return false
				case out <- chunk:
					return true
				}
			}
			if len(chunk.Payload) == 0 {
				return true
			}
			payload := rewriteForceMappedStreamChunk(rewriter, chunk.Payload)
			if len(payload) == 0 {
				return true
			}
			chunk.Payload = payload
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for chunk := range remaining {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if tail := finishForceMappedStreamChunks(rewriter); len(tail) > 0 {
			tailChunk := cliproxyexecutor.StreamChunk{Payload: tail}
			if !emit(tailChunk) {
				return
			}
		}
		if !failed && (ephemeralResult || claudeOAuthRequestCancellation(ctx, auth, nil) == nil) {
			m.recordExecutionResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: true}, auth, ephemeralResult)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func (m *Manager) replaceHomeExecutionLifecycleAuth(lifecycle cliproxyexecutor.ExecutionLifecycle, auth *Auth) {
	selection, ok := lifecycle.(*HomeDispatchSelection)
	if !ok || selection == nil {
		return
	}
	m.replaceHomeSelectionAuth(selection, auth)
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel, executionModel string, execModels []string, pooled bool, aliasResult OAuthModelAliasResult, routing *apiKeyModelRoutingSnapshot, allowRetry bool, ephemeralResult bool, unauthorizedRefreshTried map[string]struct{}) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	var lastErr error
	didRefreshOnUnauthorized := false
	if auth != nil && unauthorizedRefreshTried != nil {
		_, didRefreshOnUnauthorized = unauthorizedRefreshTried[auth.ID]
	}
	for idx, execModel := range execModels {
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		if executionModel != "" {
			execReq.Model = executionModel
		}
		execOpts := opts
		var errIntercept error
		execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(ctx, executor, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
		if errIntercept != nil {
			return nil, errIntercept
		}
		if executionModel == "" {
			execReq = attachResolvedAPIKeyModelInfo(routing, execReq, auth, routeModel, execModel)
		}
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
		streamResult, errStream := executor.ExecuteStream(ctx, auth, execReq, execOpts)
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
			if allowRetry {
				alreadyTried := didRefreshOnUnauthorized
				willAttemptHomeRefresh := ephemeralResult && !alreadyTried && auth != nil && auth.AuthKind() == AuthKindOAuth && isUnauthorizedError(errStream)
				refreshed, okRefresh, errRefresh := m.tryRefreshExecutionAuthAfterUnauthorized(ctx, executor, auth, errStream, alreadyTried, ephemeralResult)
				if willAttemptHomeRefresh {
					didRefreshOnUnauthorized = true
					if unauthorizedRefreshTried != nil {
						unauthorizedRefreshTried[auth.ID] = struct{}{}
					}
				}
				if errRefresh != nil {
					errStream = errRefresh
				} else if okRefresh {
					auth = refreshed
					m.replaceHomeExecutionLifecycleAuth(execOpts.ExecutionLifecycle, auth)
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					streamResult, errStream = executor.ExecuteStream(ctx, auth, execReq, execOpts)
					if errStream != nil {
						if errCtx := ctx.Err(); errCtx != nil {
							return nil, errCtx
						}
					}
				}
			}
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, errStream); errCancel != nil {
				return nil, errCancel
			}
		}
		streamResult, errStream = validateStreamResult(streamResult, errStream)
		if errStream != nil {
			rerr := resultErrorFromError(errStream)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
			result.RetryAfter = retryAfterFromError(errStream)
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			lastErr = errStream
			continue
		}

		firstChunkTimeout := m.firstChunkTimeout()
		bootstrapFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
		buffered, closed, bootstrapErr := readStreamBootstrapWithTimeoutFor(ctx, streamResult.Chunks, firstChunkTimeout, bootstrapFormat)
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				discardStreamChunks(streamResult.Chunks)
				return nil, errCtx
			}
			if allowRetry {
				alreadyTried := didRefreshOnUnauthorized
				willAttemptHomeRefresh := ephemeralResult && !alreadyTried && auth != nil && auth.AuthKind() == AuthKindOAuth && isUnauthorizedError(bootstrapErr)
				refreshed, okRefresh, errRefresh := m.tryRefreshExecutionAuthAfterUnauthorized(ctx, executor, auth, bootstrapErr, alreadyTried, ephemeralResult)
				if willAttemptHomeRefresh {
					didRefreshOnUnauthorized = true
					if unauthorizedRefreshTried != nil {
						unauthorizedRefreshTried[auth.ID] = struct{}{}
					}
				}
				if errRefresh != nil {
					discardStreamChunks(streamResult.Chunks)
					bootstrapErr = errRefresh
					streamResult = &cliproxyexecutor.StreamResult{}
				} else if okRefresh {
					discardStreamChunks(streamResult.Chunks)
					auth = refreshed
					m.replaceHomeExecutionLifecycleAuth(execOpts.ExecutionLifecycle, auth)
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					retryStream, retryErr := executor.ExecuteStream(ctx, auth, execReq, execOpts)
					retryStream, retryErr = validateStreamResult(retryStream, retryErr)
					if retryErr != nil {
						if errCtx := ctx.Err(); errCtx != nil {
							return nil, errCtx
						}
						bootstrapErr = retryErr
						streamResult = &cliproxyexecutor.StreamResult{}
					} else {
						streamResult = retryStream
						buffered, closed, bootstrapErr = readStreamBootstrapWithTimeoutFor(ctx, streamResult.Chunks, firstChunkTimeout, bootstrapFormat)
					}
				}
			}
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, bootstrapErr); errCancel != nil {
				discardStreamChunks(streamResult.Chunks)
				return nil, errCancel
			}
		}
		if bootstrapErr != nil {
			if idx < len(execModels)-1 {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				continue
			}
			rerr := resultErrorFromError(bootstrapErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			discardStreamChunks(streamResult.Chunks)
			return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}

		if closed && len(buffered) == 0 {
			emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: emptyErr}
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			return nil, newStreamBootstrapError(emptyErr, streamResult.Headers)
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		attemptAliasResult := resolveAttemptAliasResult(routing, auth, routeModel, execModel, aliasResult)
		return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, streamResult.Headers, buffered, remaining, attemptAliasResult, ephemeralResult), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, lastErr
}
