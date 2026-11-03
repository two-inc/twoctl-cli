package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/two-inc/twoctl-cli/internal/output"
)

// Kept injectable so CI can exercise terminal policy without a real terminal.
var paginationTerminal = func(stream any) bool {
	fd, ok := stream.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(fd.Fd()))
}

func shouldPromptPagination(c *cobra.Command, b *opBindings) bool {
	if flagBool(c, "no-paginate", "twoctl-no-paginate") || flagBool(c, "all", "twoctl-all") {
		return false
	}
	// Never consume request-body stdin as an answer, even if it is a TTY.
	if b.bodyFile != nil && *b.bodyFile == "-" {
		return false
	}
	format, err := resolveOutputFormat(c)
	return err == nil && format == output.FormatTable &&
		paginationTerminal(c.InOrStdin()) &&
		paginationTerminal(c.OutOrStdout()) &&
		paginationTerminal(c.ErrOrStderr())
}

func runInteractivePages(c *cobra.Command, client *http.Client, baseURL, method, path string, op *openapi3.Operation, b *opBindings, body []byte, cursor *string) error {
	input := bufio.NewReader(c.InOrStdin())
	_, err := fetchPages(c, client, baseURL, method, path, op, b, body, cursor, func(raw []byte, next string) (bool, error) {
		// Preserve the complete page (including metadata and numeric precision)
		// using the existing table renderer's JSON fallback for envelopes.
		if err := output.Render(c.OutOrStdout(), output.FormatTable, raw); err != nil {
			return false, err
		}
		if next == "" {
			return false, nil
		}
		return askNextPage(c.Context(), input, c.ErrOrStderr())
	})
	return err
}

func askNextPage(ctx context.Context, input *bufio.Reader, stderr io.Writer) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := fmt.Fprint(stderr, "next page? [Y/n] "); err != nil {
		return false, err
	}
	type reply struct {
		answer string
		err    error
	}
	replies := make(chan reply, 1)
	go func() {
		answer, err := input.ReadString('\n')
		replies <- reply{answer, err}
	}()
	var answer string
	var err error
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case r := <-replies:
		answer, err = r.answer, r.err
	}
	if err != nil {
		// EOF is a decline, not the empty-line default: do not drain pages
		// when a terminal closes or the command has no more input.
		if err == io.EOF {
			return false, nil
		}
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "" || answer == "y" || answer == "yes", nil
}
