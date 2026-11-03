package gui

import "strings"

// GuestReaper is anvil-guest in the VM tools share; its `reap` mode waits for every process
// a command starts, even ones that detach from it.
const GuestReaper = GuestToolsDir + "/anvil-guest"

// waitScript runs "$@" through reaper when the guest has it. Without it, it runs "$@" with
// a marker in its environment and waits while any process still carries the marker.
func waitScript(reaper string) string {
	return `r='` + reaper + `'
if [ -x "$r" ]; then exec "$r" reap -- "$@"; fi
tok="app$$."
ANVIL_APP_RUN="$tok" "$@"
rc=$?
while grep -qsF "ANVIL_APP_RUN=$tok" /proc/[0-9]*/environ; do sleep 1; done
exit $rc`
}

// WaitArgv runs command in the guest and returns only when every process it started has
// exited, so a launcher that exits right away doesn't end the app session.
func WaitArgv(command []string) []string {
	return waitArgv(GuestReaper, command)
}

func waitArgv(reaper string, command []string) []string {
	return append([]string{"sh", "-c", waitScript(reaper), "sh"}, command...)
}

// WaitShellCommand is WaitArgv as one shell command line, for ssh to run.
func WaitShellCommand(command []string) string {
	words := WaitArgv(command)
	for i, w := range words {
		words[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(words, " ")
}
