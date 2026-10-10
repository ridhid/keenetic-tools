// Package markers holds the strings that identify files and crontab lines
// installed by keenetic-tools. Changing them orphans existing installations.
package markers

const (
	// Monitor marks the awg-monitor config (line 1) and its crontab line.
	Monitor = "# keenetic-tools: awg-monitor"
	// Restart is the suffix of the restart job's crontab line.
	Restart = "# keenetic-tools: awg-cron"
	// Binary marks the Go binary itself.
	Binary = "# keenetic-tools: keenetic-tools"
)

// Embedded is kept in the binary as a separate line, so an installed file
// can be recognised as ours before it is replaced or removed.
var Embedded = "\n" + Binary + "\n"
