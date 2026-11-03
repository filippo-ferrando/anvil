package gui

import "strings"

// appWaitScript runs "$@" with a marker in its environment, then waits while any process
// still carries that marker, e.g. an editor its launcher started in the background.
const appWaitScript = `tok="app$$."
ANVIL_APP_RUN="$tok" "$@"
rc=$?
while grep -qsF "ANVIL_APP_RUN=$tok" /proc/[0-9]*/environ; do sleep 1; done
exit $rc`

// WaitArgv runs command in the guest and returns only when every process it started has
// exited, so a launcher that exits right away doesn't end the app session.
func WaitArgv(command []string) []string {
	return append([]string{"sh", "-c", appWaitScript, "sh"}, command...)
}

// WaitShellCommand is WaitArgv as one shell command line, for ssh to run.
func WaitShellCommand(command []string) string {
	words := WaitArgv(command)
	for i, w := range words {
		words[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(words, " ")
}
