package commands

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newImageCatalogCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Show or update the built-in VM image catalog",
		Long: "anvild starts with the image catalog built into it and, when the host is online, " +
			"checks the official catalog once a day for new distros, images and Firecracker kernels. " +
			"ANVIL_CATALOG_UPDATE=off in anvild's environment stops the automatic checks; " +
			"ANVIL_CATALOG_URL points them at another copy of the catalog.",
	}
	cmd.AddCommand(newImageCatalogStatusCommand(flags), newImageCatalogUpdateCommand(flags))
	return cmd
}

func newImageCatalogStatusCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show where the image catalog in use comes from and the last update check",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Image.CatalogStatus(cmd.Context(), &anvilv1.CatalogStatusRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(cmd.OutOrStdout(), reply)
				}
				printCatalogStatus(cmd.OutOrStdout(), reply)
				return nil
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newImageCatalogUpdateCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Check the official image catalog now and switch to it if it changed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Image.UpdateCatalog(cmd.Context(), &anvilv1.UpdateCatalogRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(cmd.OutOrStdout(), reply)
				}
				if reply.GetChanged() {
					fmt.Fprintf(cmd.OutOrStdout(), "catalog updated (%s)\n", format.CatalogSource(reply.GetStatus(), time.DateTime))
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "catalog already up to date")
				}
				return nil
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func printCatalogStatus(w io.Writer, s *anvilv1.CatalogStatusReply) {
	auto := "off"
	if s.GetAutoUpdate() {
		auto = "every " + (time.Duration(s.GetIntervalSeconds()) * time.Second).String()
	}
	lastCheck := "never"
	if s.GetLastCheckUnix() > 0 {
		lastCheck = time.Unix(s.GetLastCheckUnix(), 0).Format(time.DateTime)
	}
	fmt.Fprintf(w, "Source:      %s\n", format.CatalogSource(s, time.DateTime))
	fmt.Fprintf(w, "URL:         %s\n", s.GetUrl())
	fmt.Fprintf(w, "Auto update: %s\n", auto)
	fmt.Fprintf(w, "Last check:  %s\n", lastCheck)
	if s.GetLastError() != "" {
		fmt.Fprintf(w, "Last error:  %s\n", s.GetLastError())
	}
}
