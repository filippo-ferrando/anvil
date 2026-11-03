package guestexec

// BinaryPath is where anvil-guest is installed in a guest.
const BinaryPath = "/usr/local/bin/anvil-guest"

// ReapArgv runs command through `anvil-guest reap`, see Reap.
func ReapArgv(command []string) []string {
	return append([]string{BinaryPath, "reap", "--"}, command...)
}
