package cli

// Operation runner shared by the resource-first verb tree (verbs.go). Owns
// flag binding from OpenAPI parameters, URL templating, body handling, and
// HTTP execution. verbs.go is the only init() that registers commands from
// these specs; this file no longer hosts its own command tree.

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/two-inc/twoctl-cli/internal/config"
	"github.com/two-inc/twoctl-cli/internal/httpx"
	"github.com/two-inc/twoctl-cli/internal/output"
)

//go:embed specs/*.processed.yaml
var specFS embed.FS

// apiMeta lists the embedded spec files in the order they should be walked
// at startup. The list is derived from the contents of specs/ at codegen
// time; verbs.go reads only the file basename.
var apiMeta = []struct {
	file string // basename in specs/
}{
	{"checkout-api.processed.yaml"},
	{"billing-account-api.processed.yaml"},
	{"repay-api.processed.yaml"},
	{"recourse-api.processed.yaml"},
	{"company-api.processed.yaml"},
	{"limits-api.processed.yaml"},
	{"autofill-api.processed.yaml"},
	{"business-registration-api.processed.yaml"},
	{"marketplace-api.processed.yaml"},
	{"trade-account-v2-api.processed.yaml"},
	{"trade-account-v3-api.processed.yaml"},
	{"webhooks-api.processed.yaml"},
}

type opEntry struct {
	name, path, method string
	op                 *openapi3.Operation
}

// walkOperations returns the document's operations in a stable order and
// resolves any naming collisions by appending a counter suffix.
func walkOperations(doc *openapi3.T) []opEntry {
	var ops []opEntry
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op == nil {
				continue
			}
			ops = append(ops, opEntry{path: path, method: method, op: op})
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].path != ops[j].path {
			return ops[i].path < ops[j].path
		}
		return ops[i].method < ops[j].method
	})
	used := map[string]bool{}
	for i := range ops {
		base := commandNameFor(ops[i].op, ops[i].method, ops[i].path)
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		used[name] = true
		ops[i].name = name
	}
	return ops
}

// stripSuffixes are trimmed from operationId-derived command names. They
// add no information (the HTTP method is already implied by the verb in the
// remaining name).
var stripSuffixes = []string{
	"-handler-get", "-handler-post", "-handler-put",
	"-handler-delete", "-handler-patch",
	"-handler", "-v1-handler", "-v2-handler",
	"-get", "-post", "-put", "-delete", "-patch",
}

// commandNameFor derives a kebab-case command name from the operationId if
// present, falling back to a method+path derivation.
func commandNameFor(op *openapi3.Operation, method, path string) string {
	if op.OperationID != "" {
		name := toKebab(op.OperationID)
		for _, s := range stripSuffixes {
			if strings.HasSuffix(name, s) {
				name = strings.TrimSuffix(name, s)
				break
			}
		}
		return name
	}
	parts := []string{strings.ToLower(method)}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || strings.HasPrefix(seg, "{") {
			continue
		}
		parts = append(parts, seg)
	}
	return toKebab(strings.Join(parts, "-"))
}

// toKebab converts CamelCase / snake_case / mixedCase to kebab-case.
func toKebab(s string) string {
	var out strings.Builder
	for i, r := range s {
		switch {
		case r == '_' || r == ' ' || r == '/' || r == '.':
			out.WriteByte('-')
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				prev := rune(s[i-1])
				if prev != '_' && prev != '-' && !(prev >= 'A' && prev <= 'Z') {
					out.WriteByte('-')
				}
			}
			out.WriteRune(r + ('a' - 'A'))
		default:
			out.WriteRune(r)
		}
	}
	return strings.Trim(strings.ReplaceAll(out.String(), "--", "-"), "-")
}

// opBindings holds the runtime flag values for one operation. It is populated
// in bindParameters and read by runOperation.
type opBindings struct {
	stringFlags map[string]*string
	boolFlags   map[string]*bool
	intFlags    map[string]*int64
	paramKinds  map[string]string // flag -> "path"|"query"|"header"
	required    []string          // kebab-case names of required flags
	bodyFile    *string
	bodyData    *string

	// changed is populated at RunE time from cobra's Flags().Changed view.
	// We use it instead of value-equality checks so a deliberate `--limit 0`
	// or `--include-archived=false` is sent, not silently dropped.
	changed map[string]bool
}

// registerBoolUnlessTaken adds a bool flag under `name`, falling back to
// `fallback` if `name` is already claimed by a spec parameter. Prevents
// startup panics from `pflag.Flag redefined` collisions.
func registerBoolUnlessTaken(cmd *cobra.Command, name, fallback, desc string) {
	if cmd.Flags().Lookup(name) == nil {
		cmd.Flags().Bool(name, false, desc)
		return
	}
	cmd.Flags().Bool(fallback, false, desc+" (renamed from --"+name+" to avoid collision with this operation's parameter)")
}

// flagBool reads a bool flag by checking either the preferred name or the
// fallback alias. Used to read describe/dry-run/all after the rename above.
func flagBool(cmd *cobra.Command, preferred, fallback string) bool {
	if v, err := cmd.Flags().GetBool(preferred); err == nil {
		return v
	}
	v, _ := cmd.Flags().GetBool(fallback)
	return v
}

// snapshotChanged records which flags the user actually set, so
// readFlagValue can distinguish "value is 0/false because the user said so"
// from "value is the zero default because the flag wasn't passed".
func (b *opBindings) snapshotChanged(cmd *cobra.Command) {
	b.changed = map[string]bool{}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		b.changed[f.Name] = true
	})
}

// buildOperationCommand emits a single cobra command bound to one operation.
func buildOperationCommand(name, path, method string, op *openapi3.Operation) *cobra.Command {
	cmd := &cobra.Command{
		Use:   name,
		Short: firstLine(op.Summary, op.Description, fmt.Sprintf("%s %s", method, path)),
		Long:  buildOpLong(method, path, op),
	}
	bindings := &opBindings{
		stringFlags: map[string]*string{},
		boolFlags:   map[string]*bool{},
		intFlags:    map[string]*int64{},
		paramKinds:  map[string]string{},
	}
	bindParameters(cmd, op, bindings)
	bindBodyFlags(cmd, op, bindings)

	// Built-in flags. Skip registration if a spec parameter has already
	// claimed the name (would otherwise panic on duplicate). The user can
	// reach the built-in via its longer alias.
	registerBoolUnlessTaken(cmd, "describe", "twoctl-describe",
		"print this operation's spec (method, path, parameters, request body schema, response schemas) as JSON and exit")
	registerBoolUnlessTaken(cmd, "dry-run", "twoctl-dry-run",
		"print the request that would be sent and exit without calling the API")
	if hasPaginationParam(op) {
		registerBoolUnlessTaken(cmd, "all", "twoctl-all",
			"follow next_page_cursor and emit every page concatenated")
		registerBoolUnlessTaken(cmd, "no-paginate", "twoctl-no-paginate",
			"fetch only one page without prompting")
	}

	cmd.RunE = func(c *cobra.Command, args []string) error {
		// Read flags each invocation rather than closing over local vars,
		// so state never leaks between executions inside the same process
		// (relevant for tests; in production each run is a fresh process).
		describe := flagBool(c, "describe", "twoctl-describe")
		dryRun := flagBool(c, "dry-run", "twoctl-dry-run")
		all := flagBool(c, "all", "twoctl-all")
		if describe {
			return printDescribe(c, method, path, op)
		}
		if all && flagBool(c, "no-paginate", "twoctl-no-paginate") {
			return &apiError{code: "conflicting_pagination_flags", message: "--all and --no-paginate cannot be combined", exit: ExitUsage}
		}
		if !dryRun && (all || (hasPaginationParam(op) && shouldPromptPagination(c, bindings))) {
			return runPaginated(c, method, path, op, bindings)
		}
		return runOperation(c, method, path, op, bindings, dryRun)
	}
	return cmd
}

// bindParameters registers a cobra flag per OpenAPI parameter and stores the
// binding into b so runOperation can read it back. Required parameters are
// recorded but not marked with cobra.MarkFlagRequired - that fires before
// RunE and would break --describe / --dry-run which both need to run with
// missing inputs. runOperation enforces required-ness instead.
func bindParameters(cmd *cobra.Command, op *openapi3.Operation, b *opBindings) {
	for _, ref := range op.Parameters {
		p := ref.Value
		if p == nil {
			continue
		}
		flagName := toKebab(p.Name)
		b.paramKinds[flagName] = p.In
		if p.Required {
			b.required = append(b.required, flagName)
		}
		registerFlag(cmd, flagName, p, b)
		registerEnumCompletion(cmd, flagName, p)
	}
}

func registerFlag(cmd *cobra.Command, flagName string, p *openapi3.Parameter, b *opBindings) {
	desc := paramDescription(p)
	sch := schemaOf(p)
	switch {
	case sch != nil && sch.Type != nil && sch.Type.Is("boolean"):
		v := new(bool)
		b.boolFlags[flagName] = v
		cmd.Flags().BoolVar(v, flagName, false, desc)
	case sch != nil && sch.Type != nil && (sch.Type.Is("integer") || sch.Type.Is("number")):
		v := new(int64)
		b.intFlags[flagName] = v
		cmd.Flags().Int64Var(v, flagName, 0, desc)
	default:
		v := new(string)
		b.stringFlags[flagName] = v
		cmd.Flags().StringVar(v, flagName, "", desc)
	}
}

func registerEnumCompletion(cmd *cobra.Command, flagName string, p *openapi3.Parameter) {
	sch := schemaOf(p)
	if sch == nil || len(sch.Enum) == 0 {
		return
	}
	vals := make([]string, 0, len(sch.Enum))
	for _, e := range sch.Enum {
		vals = append(vals, fmt.Sprintf("%v", e))
	}
	_ = cmd.RegisterFlagCompletionFunc(flagName, func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return vals, cobra.ShellCompDirectiveNoFileComp
	})
}

func schemaOf(p *openapi3.Parameter) *openapi3.Schema {
	if p == nil || p.Schema == nil {
		return nil
	}
	return p.Schema.Value
}

func paramDescription(p *openapi3.Parameter) string {
	tag := p.In
	if p.Required {
		tag = p.In + ", required"
	}
	desc := strings.TrimSpace(p.Description)
	if desc == "" {
		return fmt.Sprintf("(%s)", tag)
	}
	return fmt.Sprintf("%s (%s)", desc, tag)
}

func bindBodyFlags(cmd *cobra.Command, op *openapi3.Operation, b *opBindings) {
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		return
	}
	b.bodyFile = new(string)
	b.bodyData = new(string)
	fileFlag, dataFlag := "file", "data"
	if cmd.Flags().Lookup("file") != nil {
		fileFlag = "twoctl-file"
	}
	if cmd.Flags().Lookup("data") != nil {
		dataFlag = "twoctl-data"
	}
	cmd.Flags().StringVar(b.bodyFile, fileFlag, "", "path to JSON request body (use - for stdin)")
	cmd.Flags().StringVar(b.bodyData, dataFlag, "", "inline JSON request body")
	if op.RequestBody.Value.Required {
		cmd.MarkFlagsOneRequired(fileFlag, dataFlag)
	}
}

// runOperation executes the bound operation. dryRun short-circuits before the
// network call and prints the request that would have been sent.
func runOperation(c *cobra.Command, method, path string, op *openapi3.Operation, b *opBindings, dryRun bool) error {
	b.snapshotChanged(c)
	if !dryRun {
		if missing := missingRequired(b); len(missing) > 0 {
			return &apiError{
				code:    "missing_required_flag",
				message: fmt.Sprintf("missing required flag(s): %s", strings.Join(missing, ", ")),
				exit:    ExitUsage,
			}
		}
	}
	resolved, err := resolveForRun(dryRun)
	if err != nil {
		return err
	}
	endpoint, headers, err := buildEndpoint(resolved.BaseURL, path, op, b)
	if err != nil {
		return err
	}
	body, err := readRequestBody(b, headers)
	if err != nil {
		return err
	}
	// Inherit the root command's context so SIGINT cancels in-flight requests.
	req, err := http.NewRequestWithContext(c.Context(), strings.ToUpper(method), endpoint, body)
	if err != nil {
		return err
	}
	copyHeaders(req, headers)
	if dryRun {
		return printDryRun(c, req, body != nil)
	}
	return executeRequest(c, httpx.New(resolved.APIKey), req)
}

func resolveForRun(dryRun bool) (*config.Resolved, error) {
	resolved, err := config.Resolve(flagAPIKey, activeEnv(), flagURL)
	if err == nil {
		return resolved, nil
	}
	if dryRun {
		return &config.Resolved{BaseURL: dryRunBaseURL()}, nil
	}
	return nil, err
}

// dryRunBaseURL picks a sensible URL for --dry-run when no context is set.
func dryRunBaseURL() string {
	if flagURL != "" {
		return flagURL
	}
	if activeEnv() != "" {
		// Best-effort: infer from name; matches what `config.Resolve` would do.
		return "https://api." + activeEnv() + ".two.inc"
	}
	return "https://api.sandbox.two.inc"
}

// buildEndpoint expands path templates and assembles the URL + headers from
// the bound parameter flag values.
func buildEndpoint(baseURL, pathTemplate string, op *openapi3.Operation, b *opBindings) (string, http.Header, error) {
	built := pathTemplate
	query := url.Values{}
	headers := http.Header{}
	for flag, kind := range b.paramKinds {
		val, present := readFlagValue(flag, b)
		if !present {
			continue
		}
		// Always substitute by the spec's original parameter name (not by
		// a kebab→snake guess) so {order-id} and {tradeAccountId} survive
		// unchanged.
		original := originalParamName(op, flag)
		switch kind {
		case "path":
			built = strings.ReplaceAll(built, "{"+original+"}", url.PathEscape(val))
		case "query":
			query.Set(original, val)
		case "header":
			headers.Set(original, val)
		}
	}
	endpoint := baseURL + built
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	return endpoint, headers, nil
}

// missingRequired returns the kebab-case names of required flags that have
// no value bound.
func missingRequired(b *opBindings) []string {
	var missing []string
	for _, name := range b.required {
		if _, ok := readFlagValue(name, b); !ok {
			missing = append(missing, "--"+name)
		}
	}
	sort.Strings(missing)
	return missing
}

func readFlagValue(flag string, b *opBindings) (string, bool) {
	// Presence is determined by cobra's Changed view (populated in
	// snapshotChanged) so a deliberate zero value isn't mistaken for
	// "not set". Fall back to value-emptiness only when changed hasn't
	// been populated yet (legacy callers, e.g. tests of helpers in
	// isolation).
	present := func(name string) bool {
		if b.changed != nil {
			return b.changed[name]
		}
		return false
	}
	if v, ok := b.stringFlags[flag]; ok {
		if present(flag) || *v != "" {
			return *v, true
		}
	}
	if v, ok := b.intFlags[flag]; ok {
		if present(flag) || *v != 0 {
			return fmt.Sprintf("%d", *v), true
		}
	}
	if v, ok := b.boolFlags[flag]; ok {
		if present(flag) || *v {
			return fmt.Sprintf("%t", *v), true
		}
	}
	return "", false
}

func readRequestBody(b *opBindings, headers http.Header) (io.Reader, error) {
	if b.bodyFile == nil && b.bodyData == nil {
		return nil, nil
	}
	raw, err := readBody(*b.bodyFile, *b.bodyData)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	headers.Set("Content-Type", "application/json")
	return bytes.NewReader(raw), nil
}

func copyHeaders(req *http.Request, headers http.Header) {
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

func printDryRun(c *cobra.Command, req *http.Request, hasBody bool) error {
	dump := map[string]any{
		"method":   req.Method,
		"url":      req.URL.String(),
		"headers":  redactHeaders(req.Header),
		"has_body": hasBody,
	}
	enc := json.NewEncoder(c.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(dump)
}

// maxResponseBytes caps how much of the response we'll read into memory.
// Stops a misbehaving server from OOMing the CLI.
const maxResponseBytes = 4 << 20 // 4 MiB

// readCappedBody reads up to maxResponseBytes+1 from r. If the body would
// exceed the cap, returns a clear apiError rather than silently truncating
// (which would leave json.Unmarshal staring at a malformed half-document
// with no actionable error).
func readCappedBody(r io.Reader) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > maxResponseBytes {
		return nil, &apiError{
			code:    "response_too_large",
			message: fmt.Sprintf("response body exceeded %d byte cap; re-run with narrower filters or fetch a single resource", maxResponseBytes),
			exit:    ExitGeneric,
		}
	}
	return buf, nil
}

func executeRequest(c *cobra.Command, client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return &apiError{code: "network", message: err.Error(), exit: ExitNetwork}
	}
	defer resp.Body.Close()

	respBody, err := readCappedBody(resp.Body)
	if err != nil {
		return err
	}
	if err := printResponse(c, resp.StatusCode, respBody); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return &apiError{
			code:      fmt.Sprintf("http_%d", resp.StatusCode),
			status:    resp.StatusCode,
			message:   friendlyHTTPMessage(resp.StatusCode),
			body:      respBody,
			requestID: captureRequestID(resp.Header),
			exit:      exitForStatus(resp.StatusCode),
		}
	}
	return nil
}

// captureRequestID returns the first non-empty value from any of the
// common request-id headers, so users have something to paste into a
// support ticket.
func captureRequestID(h http.Header) string {
	for _, key := range []string{"X-Request-Id", "X-Trace-Id", "X-Correlation-Id", "Request-Id"} {
		if v := h.Get(key); v != "" {
			return v
		}
	}
	return ""
}

// friendlyHTTPMessage maps HTTP failures to user-actionable hints. Falls back
// to a bare "HTTP NNN" so the body (already on stdout) carries the detail.
func friendlyHTTPMessage(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "HTTP 401: API key rejected. Run `twoctl auth login` or check the active context with `twoctl auth whoami`."
	case http.StatusForbidden:
		return "HTTP 403: the active API key does not have access to this operation."
	case http.StatusNotFound:
		return fmt.Sprintf("HTTP 404: not found.")
	case http.StatusTooManyRequests:
		return "HTTP 429: rate limited. Retry after a short backoff."
	}
	if status >= 500 {
		return fmt.Sprintf("HTTP %d: server error. Try again or contact Two support if it persists.", status)
	}
	return fmt.Sprintf("HTTP %d", status)
}

// originalParamName returns the spec's original parameter name for a flag,
// since header names are case-sensitive-ish and shouldn't be kebab-mangled
// when sent on the wire.
func originalParamName(op *openapi3.Operation, flag string) string {
	for _, ref := range op.Parameters {
		if ref.Value != nil && toKebab(ref.Value.Name) == flag {
			return ref.Value.Name
		}
	}
	return flag
}

func readBody(file, data string) ([]byte, error) {
	switch {
	case file == "-":
		return io.ReadAll(os.Stdin)
	case file != "":
		return os.ReadFile(file)
	case data != "":
		return []byte(data), nil
	}
	return nil, nil
}

func printResponse(cmd *cobra.Command, status int, body []byte) error {
	if len(body) == 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "HTTP %d (no body)\n", status)
		return nil
	}
	format, err := resolveOutputFormat(cmd)
	if err != nil {
		return err
	}
	return output.Render(cmd.OutOrStdout(), format, body)
}

// resolveOutputFormat reads --output, falling back to table when attached
// to a TTY and json when piped, matching the kubectl/gh convention.
func resolveOutputFormat(cmd *cobra.Command) (output.Format, error) {
	raw := flagOutput
	if raw == "" || raw == "auto" {
		if term.IsTerminal(int(os.Stdout.Fd())) {
			return output.FormatTable, nil
		}
		return output.FormatJSON, nil
	}
	return output.Parse(raw)
}

func firstLine(candidates ...string) string {
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if i := strings.IndexByte(c, '\n'); i >= 0 {
			return c[:i]
		}
		return c
	}
	return ""
}

// printDescribe emits a JSON document describing the operation. Designed for
// agents that need the request/response schema without making a network call.
func printDescribe(cmd *cobra.Command, method, path string, op *openapi3.Operation) error {
	doc := describeDoc{
		OperationID: op.OperationID,
		Method:      strings.ToUpper(method),
		Path:        path,
		Summary:     op.Summary,
		Description: op.Description,
		Parameters:  describeParams(op),
		RequestBody: describeRequestBody(op),
		Responses:   describeResponses(op),
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

type describeParam struct {
	Name        string `json:"name"`
	In          string `json:"in"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
	Schema      any    `json:"schema,omitempty"`
}

type describeDoc struct {
	OperationID string          `json:"operation_id,omitempty"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	Summary     string          `json:"summary,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  []describeParam `json:"parameters,omitempty"`
	RequestBody any             `json:"request_body,omitempty"`
	Responses   map[string]any  `json:"responses,omitempty"`
}

func describeParams(op *openapi3.Operation) []describeParam {
	var out []describeParam
	for _, ref := range op.Parameters {
		p := ref.Value
		if p == nil {
			continue
		}
		entry := describeParam{
			Name: p.Name, In: p.In, Required: p.Required, Description: p.Description,
		}
		if s := schemaOf(p); s != nil {
			entry.Schema = schemaSummary(s)
		}
		out = append(out, entry)
	}
	return out
}

func describeRequestBody(op *openapi3.Operation) any {
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		return nil
	}
	body := op.RequestBody.Value
	mt := body.Content.Get("application/json")
	if mt == nil || mt.Schema == nil || mt.Schema.Value == nil {
		return nil
	}
	return map[string]any{
		"required":     body.Required,
		"content_type": "application/json",
		"schema":       schemaSummary(mt.Schema.Value),
	}
}

func describeResponses(op *openapi3.Operation) map[string]any {
	if op.Responses == nil {
		return nil
	}
	out := map[string]any{}
	for status, ref := range op.Responses.Map() {
		if ref.Value == nil {
			continue
		}
		entry := map[string]any{"description": ref.Value.Description}
		if mt := ref.Value.Content.Get("application/json"); mt != nil && mt.Schema != nil && mt.Schema.Value != nil {
			entry["schema"] = schemaSummary(mt.Schema.Value)
		}
		out[status] = entry
	}
	return out
}

// schemaSummary produces a compact, machine-friendly view of a schema. We
// emit the immediate fields rather than walking the entire ref graph -
// callers can drill via `twoctl describe-schema` if we ever add it.
func schemaSummary(s *openapi3.Schema) map[string]any {
	out := map[string]any{}
	if s.Type != nil {
		out["type"] = s.Type.Slice()
	}
	if s.Format != "" {
		out["format"] = s.Format
	}
	if len(s.Enum) > 0 {
		out["enum"] = s.Enum
	}
	if s.Description != "" {
		out["description"] = s.Description
	}
	if s.Example != nil {
		out["example"] = s.Example
	}
	if len(s.Required) > 0 {
		out["required"] = s.Required
	}
	if len(s.Properties) > 0 {
		out["properties"] = schemaProperties(s)
	}
	if s.Items != nil && s.Items.Value != nil {
		out["items"] = schemaSummary(s.Items.Value)
	}
	return out
}

func schemaProperties(s *openapi3.Schema) map[string]any {
	props := map[string]any{}
	for k, ref := range s.Properties {
		if ref.Value == nil {
			continue
		}
		leaf := map[string]any{}
		if ref.Value.Type != nil {
			leaf["type"] = ref.Value.Type.Slice()
		}
		if ref.Value.Description != "" {
			leaf["description"] = ref.Value.Description
		}
		if ref.Value.Example != nil {
			leaf["example"] = ref.Value.Example
		}
		props[k] = leaf
	}
	return props
}

func buildOpLong(method, path string, op *openapi3.Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", strings.ToUpper(method), path)
	if op.Summary != "" {
		fmt.Fprintf(&b, "\n%s\n", op.Summary)
	}
	if op.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", op.Description)
	}
	return b.String()
}

