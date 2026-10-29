package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// beginRequestDiagnostics returns a completion hook shared by ordinary and
// paginated operations. It never changes the API result or consumes a body
// that the request still needs.
func beginRequestDiagnostics(cmd *cobra.Command, req *http.Request) func(*http.Response, []byte, error) {
	level := flagVerbose
	if level == 0 {
		return func(*http.Response, []byte, error) {}
	}
	started := time.Now()
	return func(resp *http.Response, body []byte, err error) {
		url := diagnosticURL(req)
		record := map[string]any{
			"method": req.Method, "url": url,
			"duration_ms": float64(time.Since(started)) / float64(time.Millisecond),
		}
		if resp != nil {
			record["status"] = resp.StatusCode
			if id := captureRequestID(resp.Header); id != "" {
				record["request_id"] = id
			}
		}
		if err != nil {
			message := strings.ReplaceAll(err.Error(), req.URL.String(), url)
			record["error"] = secretValueRe.ReplaceAllString(message, "[REDACTED]")
		}
		if level >= 2 {
			addDiagnosticDetails(record, req, resp, body)
		}
		// A failed diagnostic write must not change the API operation's result.
		_ = json.NewEncoder(cmd.ErrOrStderr()).Encode(record)
	}
}

func addDiagnosticDetails(record map[string]any, req *http.Request, resp *http.Response, body []byte) {
	record["request_headers"] = diagnosticHeaders(req.Header)
	record["request_body"] = diagnosticRequestBody(req)
	if resp != nil {
		record["response_headers"] = diagnosticHeaders(resp.Header)
		record["response_body"] = diagnosticBody(body)
	}
}

func diagnosticURL(req *http.Request) string {
	url := *req.URL
	query := url.Query()
	for key, values := range query {
		if sensitiveHeaderRe.MatchString(key) {
			query.Set(key, "[REDACTED]")
			continue
		}
		for i, value := range values {
			values[i] = secretValueRe.ReplaceAllString(value, "[REDACTED]")
		}
	}
	url.RawQuery = query.Encode()
	return url.String()
}

func diagnosticHeaders(headers http.Header) map[string]string {
	out := redactHeaders(headers)
	for key := range out {
		if sensitiveHeaderRe.MatchString(key) {
			out[key] = "[REDACTED]"
		} else {
			out[key] = secretValueRe.ReplaceAllString(out[key], "[REDACTED]")
		}
	}
	return out
}

func diagnosticRequestBody(req *http.Request) any {
	if req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	if req.GetBody == nil {
		return "[body unavailable]"
	}
	copy, err := req.GetBody()
	if err != nil {
		return "[body unavailable]"
	}
	defer copy.Close()
	raw, err := io.ReadAll(io.LimitReader(copy, maxResponseBytes+1))
	if err != nil {
		return "[body unavailable]"
	}
	return diagnosticBody(raw)
}

func diagnosticBody(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > maxResponseBytes {
		return "[body exceeds diagnostic limit]"
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return secretValueRe.ReplaceAllString(string(raw), "[REDACTED]")
	}
	return redactDiagnosticValue(value)
}

func redactDiagnosticValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if sensitiveHeaderRe.MatchString(key) {
				value[key] = "[REDACTED]"
			} else {
				value[key] = redactDiagnosticValue(child)
			}
		}
	case []any:
		for i, child := range value {
			value[i] = redactDiagnosticValue(child)
		}
	case string:
		return secretValueRe.ReplaceAllString(value, "[REDACTED]")
	}
	return value
}
