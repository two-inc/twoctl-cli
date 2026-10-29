package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/spf13/cobra"
)

func interactiveTestCommand(t *testing.T, input string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	resetGlobals(t)
	old := paginationTerminal
	paginationTerminal = func(any) bool { return true }
	t.Cleanup(func() { paginationTerminal = old })
	flagOutput = "table"
	op := &openapi3.Operation{Parameters: openapi3.Parameters{
		{Value: &openapi3.Parameter{Name: cursorParamName, In: "query"}},
	}}
	cmd := buildOperationCommand("list", "/items", "GET", op)
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetIn(strings.NewReader(input))
	cmd.SetContext(context.Background())
	return cmd, &out, &stderr
}

func TestInteractivePaginationAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		pages       int
	}{
		{"default yes then no", "\nn\n", 2},
		{"yes uppercase", "YES\n", 2},
		{"no", "n\n", 1},
		{"other", "maybe\n", 1},
		{"eof", "", 1},
		{"unfinished yes", "y", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, out, stderr := interactiveTestCommand(t, tc.input)
			pages := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages++
				if pages == 2 && r.URL.Query().Get(cursorParamName) != "second" {
					t.Errorf("second page cursor: %s", r.URL.RawQuery)
				}
				next := "second"
				if pages == 2 {
					next = ""
				}
				fmt.Fprintf(w, `{"items":[{"id":9007199254740993,"page":%d}],"next_page_cursor":%q}`, pages, next)
			}))
			defer srv.Close()
			flagURL, flagAPIKey = srv.URL, "secret_test_x"
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if pages != tc.pages {
				t.Fatalf("pages = %d, want %d", pages, tc.pages)
			}
			if !strings.Contains(out.String(), "9007199254740993") {
				t.Fatal("numeric precision lost")
			}
			if strings.Contains(out.String(), "next page?") {
				t.Fatal("prompt leaked to stdout")
			}
			if stderr.String() != "next page? [Y/n] " {
				t.Fatalf("stderr: %q", stderr.String())
			}
		})
	}
}

func TestInteractivePaginationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		args         []string
		terminal     bool
		pages        int
	}{
		{"json", "json", nil, true, 1},
		{"yaml", "yaml", nil, true, 1},
		{"pipe", "table", nil, false, 1},
		{"single page", "table", []string{"--no-paginate"}, true, 1},
		{"all json", "json", []string{"--all"}, true, 2},
		{"all table", "table", []string{"--all"}, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, stderr := interactiveTestCommand(t, "y\n")
			flagOutput = tc.format
			paginationTerminal = func(any) bool { return tc.terminal }
			pages := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages++
				next := "second"
				if pages == 2 {
					next = ""
				}
				fmt.Fprintf(w, `{"items":[{"id":%d}],"next_page_cursor":%q}`, pages, next)
			}))
			defer srv.Close()
			flagURL, flagAPIKey = srv.URL, "secret_test_x"
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if pages != tc.pages || stderr.Len() != 0 {
				t.Fatalf("pages=%d stderr=%q", pages, stderr.String())
			}
		})
	}
}

func TestInteractivePaginationStreamAndBodyPolicy(t *testing.T) {
	cmd, _, _ := interactiveTestCommand(t, "")
	b := &opBindings{}
	for _, stream := range []any{cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()} {
		paginationTerminal = func(candidate any) bool { return candidate != stream }
		if shouldPromptPagination(cmd, b) {
			t.Fatal("redirected stream allowed prompting")
		}
	}
	paginationTerminal = func(any) bool { return true }
	stdin := "-"
	b.bodyFile = &stdin
	if shouldPromptPagination(cmd, b) {
		t.Fatal("request-body stdin allowed prompting")
	}
}

func TestPaginationFlagsConflictAndDryRun(t *testing.T) {
	cmd, _, stderr := interactiveTestCommand(t, "y\n")
	cmd.SetArgs([]string{"--all", "--no-paginate"})
	var ae *apiError
	if err := cmd.Execute(); !errors.As(err, &ae) || ae.exit != ExitUsage {
		t.Fatalf("want usage error, got %v", err)
	}
	cmd, out, stderr := interactiveTestCommand(t, "y\n")
	cmd.SetArgs([]string{"--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 || !strings.Contains(out.String(), "has_body") {
		t.Fatalf("dry run out=%q stderr=%q", out.String(), stderr.String())
	}
}

func TestInteractivePaginationLoopAndCursorRestore(t *testing.T) {
	cmd, _, _ := interactiveTestCommand(t, "\n\n\n")
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		fmt.Fprint(w, `{"items":[{"id":1}],"next_page_cursor":"same"}`)
	}))
	defer srv.Close()
	flagURL, flagAPIKey = srv.URL, "secret_test_x"
	cmd.SetArgs([]string{"--page-cursor", "original"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "loop detected") {
		t.Fatalf("want loop error, got %v", err)
	}
	cursor, _ := cmd.Flags().GetString("page-cursor")
	if pages != 2 || cursor != "original" {
		t.Fatalf("pages=%d cursor=%q", pages, cursor)
	}
}

func TestNextPageCancellation(t *testing.T) {
	in := &paginationBlockedReader{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(in.release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := askNextPage(ctx, bufio.NewReader(in), io.Discard)
		done <- err
	}()
	select {
	case <-in.entered:
	case <-time.After(time.Second):
		t.Fatal("prompt never entered input read")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt ignored cancellation")
	}
}

type paginationBlockedReader struct {
	entered chan struct{}
	release chan struct{}
}

func (r *paginationBlockedReader) Read([]byte) (int, error) {
	close(r.entered)
	<-r.release
	return 0, io.EOF
}
