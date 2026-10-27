package commands

import (
	"cmp"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
)

// addOutputFlag registers -o/--output, checked with checkOutput before use.
func addOutputFlag(cmd *cobra.Command, output *string) {
	cmd.Flags().StringVarP(output, "output", "o", "table", `output format: "table" or "json"`)
}

func checkOutput(output string) error {
	if output != "table" && output != "json" {
		return fmt.Errorf(`--output must be "table" or "json", got %q`, output)
	}
	return nil
}

// printJSON writes m as indented JSON with the field names from anvil.proto.
func printJSON(w io.Writer, m proto.Message) error {
	b, err := protojson.MarshalOptions{Multiline: true, UseProtoNames: true}.Marshal(m)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// stateLabel is the instance state with a capital first letter, e.g. "Running".
func stateLabel(s anvilv1.State) string {
	l := format.State(s)
	return strings.ToUpper(l[:1]) + l[1:]
}

// printInstanceTable prints instances as a tab-aligned table to w.
func printInstanceTable(w io.Writer, instances []*anvilv1.Instance) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tENGINE\tSTATE\tIP\tIMAGE")
	for _, inst := range instances {
		image := ""
		engine := "-"
		if inst.GetVm() != nil {
			image = inst.GetVm().GetImageRef()
			engine = format.Hypervisor(inst.GetVm().GetHypervisor())
		} else if c := inst.GetContainer(); c != nil {
			image = c.GetImageRef()
			engine = format.Engine(c.GetEngine())
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", inst.GetName(), format.Kind(inst.GetKind()), engine, stateLabel(inst.GetState()), cmp.Or(format.InstanceIP(inst), "-"), image)
	}
	tw.Flush()
}
