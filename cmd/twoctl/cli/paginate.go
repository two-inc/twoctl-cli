package cli

// Cursor-style pagination. Operations that expose a `page_cursor` query
// parameter and return a body with `next_page_cursor` get an extra `--all`
// flag that follows the cursor until exhaustion, concatenating each page's
// `items` array into a single JSON output.
//
// Falls back gracefully: if the response shape doesn't include a cursor
// field, --all completes after the first page (same as running without it).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/spf13/cobra"

	"github.com/two-inc/twoctl-cli/internal/httpx"
	"github.com/two-inc/twoctl-cli/internal/output"
)

const (
	cursorParamName    = "page_cursor" // spec name; flag is the kebab variant
	cursorFlagName     = "page-cursor"
	cursorResponseName = "next_page_cursor"
	itemsResponseName  = "items"
	maxPaginatedPages  = 10_000 // hard ceiling on --all to prevent runaway loops
)

func hasPaginationParam(op *openapi3.Operation) bool {
	for _, ref := range op.Parameters {
		if ref.Value != nil && ref.Value.Name == cursorParamName {
			return true
		}
	}
	return false
}

// runPaginated drives the cursor loop. We re-call the same operation with an
// updated `page_cursor` flag value until the response stops yielding a
// next_page_cursor.
func runPaginated(c *cobra.Command, method, path string, op *openapi3.Operation, b *opBindings) error {
	if missing := missingRequired(b); len(missing) > 0 {
		return &apiError{
			code:    "missing_required_flag",
			message: fmt.Sprintf("missing required flag(s): %s", strings.Join(missing, ", ")),
			exit:    ExitUsage,
		}
	}
	b.snapshotChanged(c)
	resolved, err := resolveForRun(false)
	if err != nil {
		return err
	}
	client := httpx.New(resolved.APIKey)

	var combined []any
	cursorPtr := b.stringFlags[cursorFlagName]
	if cursorPtr == nil {
		// shouldn't happen because hasPaginationParam guarded the
		// caller, but be defensive: degrade to a single call.
		return runOperation(c, method, path, op, b, false)
	}

	// Read the request body once, outside the loop. For `--file -` (stdin)
	// the second read would otherwise return empty bytes and silently
	// downgrade page 2's POST.
	var bodyBytes []byte
	if b.bodyFile != nil || b.bodyData != nil {
		bodyBytes, err = readBody(*b.bodyFile, *b.bodyData)
		if err != nil {
			return err
		}
	}

	if shouldPromptPagination(c, b) && !flagBool(c, "all", "twoctl-all") {
		return runInteractivePages(c, client, resolved.BaseURL, method, path, op, b, bodyBytes, cursorPtr)
	}

	combined, err = fetchAllPages(c, client, resolved.BaseURL, method, path, op, b, bodyBytes, cursorPtr)
	if err != nil {
		return err
	}

	wrapped := map[string]any{itemsResponseName: combined}
	rendered, err := json.Marshal(wrapped)
	if err != nil {
		return err
	}
	format, err := resolveOutputFormat(c)
	if err != nil {
		return err
	}
	// For table mode the most useful shape is the flattened item list, so
	// we render the items array directly when there's nothing else worth
	// showing.
	if format == output.FormatTable {
		flat, _ := json.Marshal(combined)
		return output.Render(c.OutOrStdout(), format, flat)
	}
	return output.Render(c.OutOrStdout(), format, rendered)
}

// fetchAllPages walks the cursor loop, applying the seen-set, empty-items,
// and max-page guards. Extracted from runPaginated so the cyclomatic
// complexity of the caller stays under the lint threshold.
func fetchAllPages(c *cobra.Command, client *http.Client, baseURL, method, path string, op *openapi3.Operation, b *opBindings, bodyBytes []byte, cursorPtr *string) ([]any, error) {
	return fetchPages(c, client, baseURL, method, path, op, b, bodyBytes, cursorPtr, nil)
}

// fetchPages shares request construction and safety guards between --all and
// interactive browsing. A visitor streams each page; nil collects all items.
func fetchPages(c *cobra.Command, client *http.Client, baseURL, method, path string, op *openapi3.Operation, b *opBindings, bodyBytes []byte, cursorPtr *string, visit func([]byte, string) (bool, error)) ([]any, error) {
	originalCursor := *cursorPtr
	defer func() { *cursorPtr = originalCursor }()
	var combined []any
	seen := map[string]struct{}{originalCursor: {}}
	for page := 0; page < maxPaginatedPages; page++ {
		req, err := buildPageRequest(c, baseURL, method, path, op, b, bodyBytes)
		if err != nil {
			return nil, err
		}
		raw, err := fetchPage(client, req)
		if err != nil {
			return nil, err
		}
		items, next := splitPage(raw)
		if visit == nil {
			combined = append(combined, items...)
		} else if more, err := visit(raw, next); err != nil || !more {
			return nil, err
		}
		if next == "" {
			return combined, nil
		}
		if _, ok := seen[next]; ok {
			return nil, fmt.Errorf("pagination loop detected: server returned the same next_page_cursor %q twice", next)
		}
		seen[next] = struct{}{}
		if len(items) == 0 {
			return nil, fmt.Errorf("server returned empty items with a non-empty next_page_cursor; aborting --all")
		}
		*cursorPtr = next
	}
	return nil, fmt.Errorf("--all hit %d-page safety cap; narrow the query or paginate manually", maxPaginatedPages)
}

func buildPageRequest(c *cobra.Command, baseURL, method, path string, op *openapi3.Operation, b *opBindings, bodyBytes []byte) (*http.Request, error) {
	endpoint, headers, err := buildEndpoint(baseURL, path, op, b)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if bodyBytes != nil {
		body = bytes.NewReader(bodyBytes)
		headers.Set("Content-Type", "application/json")
	}
	req, err := http.NewRequestWithContext(c.Context(), strings.ToUpper(method), endpoint, body)
	if err != nil {
		return nil, err
	}
	copyHeaders(req, headers)
	return req, nil
}

func fetchPage(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, &apiError{code: "network", message: err.Error(), exit: ExitNetwork}
	}
	defer resp.Body.Close()
	body, err := readCappedBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &apiError{
			code:      fmt.Sprintf("http_%d", resp.StatusCode),
			status:    resp.StatusCode,
			message:   friendlyHTTPMessage(resp.StatusCode),
			body:      body,
			requestID: captureRequestID(resp.Header),
			exit:      exitForStatus(resp.StatusCode),
		}
	}
	return body, nil
}

// splitPage extracts items + next_page_cursor from a page response. Returns
// (nil, "") if the body doesn't carry the expected shape. A JSON `null` in
// either field is treated as absent.
func splitPage(raw []byte) ([]any, string) {
	var page map[string]any
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, ""
	}
	var items []any
	switch v := page[itemsResponseName].(type) {
	case []any:
		items = v
	case nil:
		// null or missing - leave items empty.
	}
	var next string
	if c, ok := page[cursorResponseName].(string); ok {
		next = c
	}
	return items, next
}
