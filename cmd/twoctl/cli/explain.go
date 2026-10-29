package cli

// kubectl-flavored top-level helpers: `explain`, `api-resources`.

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/spf13/cobra"
)

func init() {

	register(&cobra.Command{
		Use:   "explain <resource> <action>",
		Short: "Print an operation's OpenAPI schema",
		Long: `Print the request and response schema for an operation as JSON.
Equivalent to running the operation with --describe.

  twoctl explain order get               # GET order schema
  twoctl explain order create            # POST order schema
  twoctl explain order cancel            # cancel-order schema
  twoctl explain company search          # company search schema`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve the resource subtree + leaf action directly.
			// Re-entering Root().Execute() would re-fire PersistentPreRunE
			// (auto-upgrade check) and reuse the global flag state.
			return explainOperation(cmd, args[0], args[1])
		},
	})

	apiResources := &cobra.Command{
		Use:   "api-resources",
		Short: "List every operation as JSON (for agent discovery)",
		Long: `Emit a JSON catalog of every operation twoctl knows about. Each entry
includes the resource, action, command path, HTTP method, URL template,
flags (with kind/type/required), and body status. Designed to be piped to
an LLM planner or an agent's prompt.`,
		RunE: runAPIResources,
	}
	apiResources.Flags().StringP("resource", "r", "", "limit to a single resource (e.g. order, company)")
	apiResources.Flags().StringP("action", "a", "", "limit to a single action (e.g. get, create, cancel)")
	register(apiResources)
}

type catalogFlag struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Desc     string `json:"description,omitempty"`
}

type catalogOp struct {
	Command      string        `json:"command"` // e.g. "twoctl order get"
	Resource     string        `json:"resource"`
	Action       string        `json:"action"`
	OperationID  string        `json:"operation_id,omitempty"`
	Method       string        `json:"method"`
	Path         string        `json:"path"`
	Summary      string        `json:"summary,omitempty"`
	Flags        []catalogFlag `json:"flags,omitempty"`
	HasBody      bool          `json:"has_body"`
	BodyRequired bool          `json:"body_required,omitempty"`
}

// explainOperation walks the embedded specs, finds the operation matching
// (resource, action) and prints its describe doc. Reuses the same
// classification path verbs.go uses so the lookup stays consistent.
func explainOperation(cmd *cobra.Command, resource, action string) error {
	for _, m := range apiMeta {
		doc, err := loadEmbeddedSpec(m.file)
		if err != nil {
			continue
		}
		for _, e := range walkOperations(doc) {
			r, a := classifyOperation(e.op, e.method, e.path)
			if r == resource && a == action {
				return printDescribe(cmd, e.method, e.path, e.op)
			}
		}
	}
	return fmt.Errorf("no operation %q on resource %q", action, resource)
}

func runAPIResources(cmd *cobra.Command, args []string) error {
	wantResource, _ := cmd.Flags().GetString("resource")
	wantAction, _ := cmd.Flags().GetString("action")
	out, err := collectCatalog(wantResource, wantAction)
	if err != nil {
		return err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Command < out[j].Command })
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func collectCatalog(wantResource, wantAction string) ([]catalogOp, error) {
	var out []catalogOp
	taken := map[string]map[string]bool{}
	for _, m := range apiMeta {
		doc, err := loadEmbeddedSpec(m.file)
		if err != nil {
			return nil, err
		}
		for _, e := range walkOperations(doc) {
			resource, action := classifyOperation(e.op, e.method, e.path)
			if resource == "" || action == "" {
				continue
			}
			if wantResource != "" && wantResource != resource {
				continue
			}
			if wantAction != "" && wantAction != action {
				continue
			}
			if taken[resource] == nil {
				taken[resource] = map[string]bool{}
			}
			leaf := action
			for n := 2; taken[resource][leaf]; n++ {
				leaf = fmt.Sprintf("%s-%d", action, n)
			}
			taken[resource][leaf] = true
			entry := catalogEntryFor(resource, leaf, e)
			entry.Action = action // keep the bare action for filtering
			out = append(out, entry)
		}
	}
	return out, nil
}

func loadEmbeddedSpec(file string) (*openapi3.T, error) {
	raw, err := specFS.ReadFile("specs/" + file)
	if err != nil {
		return nil, err
	}
	return openapi3.NewLoader().LoadFromData(raw)
}

func catalogEntryFor(resource, action string, e opEntry) catalogOp {
	entry := catalogOp{
		Command:     "twoctl " + resource + " " + action,
		Resource:    resource,
		Action:      action,
		OperationID: e.op.OperationID,
		Method:      e.method,
		Path:        e.path,
		Summary:     e.op.Summary,
		Flags:       catalogFlagsFor(e.op),
	}
	if e.op.RequestBody != nil && e.op.RequestBody.Value != nil {
		entry.HasBody = true
		entry.BodyRequired = e.op.RequestBody.Value.Required
	}
	return entry
}

func catalogFlagsFor(op *openapi3.Operation) []catalogFlag {
	var flags []catalogFlag
	for _, ref := range op.Parameters {
		p := ref.Value
		if p == nil {
			continue
		}
		flags = append(flags, catalogFlag{
			Name:     toKebab(p.Name),
			In:       p.In,
			Type:     flagTypeOf(p),
			Required: p.Required,
			Desc:     p.Description,
		})
	}
	return flags
}

func flagTypeOf(p *openapi3.Parameter) string {
	sch := schemaOf(p)
	if sch == nil || sch.Type == nil {
		return "string"
	}
	if s := sch.Type.Slice(); len(s) > 0 {
		return s[0]
	}
	return "string"
}
