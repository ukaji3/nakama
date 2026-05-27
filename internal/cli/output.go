package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// renderOutput writes the given object to w according to the
// --output format setting (table | json | yaml). For "table" the
// caller may pass a textual representation that is already laid out.
func renderOutput(w io.Writer, format string, v any, fallbackText string) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case "yaml":
		enc := yaml.NewEncoder(w)
		enc.SetIndent(2)
		defer func() { _ = enc.Close() }()
		return enc.Encode(v)
	default:
		_, err := fmt.Fprintln(w, fallbackText)
		return err
	}
}

// withCancel returns the command's context, falling back to
// context.Background() if cobra has not propagated one.
func withCancel(cmd *cobra.Command) context.Context {
	if c := cmd.Context(); c != nil {
		return c
	}
	return context.Background()
}
