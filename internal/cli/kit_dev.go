//go:build dev

package cli

import (
	"fmt"

	"github.com/starknet-innovation/troupe/internal/help"
	"github.com/starknet-innovation/troupe/internal/published"
)

var kitHelp = help.Render(help.Command{
	Summary:  "Check a kit's source folder and print the label its published image must carry.",
	Usage:    []string{"kit label DIR"},
	Commands: []help.Row{{Name: "label DIR", Desc: "Checks the kit folder DIR as create would, and prints its label."}},
	Notes: []string{
		"Reads DIR/troupe/ (defaults.json, services.json and, for a kit with settings, settings.json), DIR/templates/env and DIR/slack-app-manifest.json.",
		"Prints one line of JSON: the value of the image label " + published.LabelName + ", which is all create reads of them.",
		"Exit 1, with the reason, for a kit create would refuse or a field troupe does not know: nothing is printed.",
		"Run it before every publication, with the troupe whose format the kit is meant for.",
	},
})

func kitCommand(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, kitHelp)
		return 0
	}
	if len(args) == 0 || args[0] != "label" {
		return usageError(e, kitHelp, "kit needs the label subcommand")
	}
	rest, code, ok := subcommand(e, "kit label", kitHelp, args[1:], nil)
	if !ok {
		return code
	}
	if len(rest) != 1 {
		return usageError(e, kitHelp, "kit label needs one kit folder")
	}
	kit, err := published.ReadSource(rest[0])
	if err != nil {
		return fail(e, err)
	}
	label, err := kit.Encode()
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprintln(e.out, label)
	return 0
}
