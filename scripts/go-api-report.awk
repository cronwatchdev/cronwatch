# The core's release version is expected to change. Types, removals and all
# other constants still participate in the compatibility check.
package == "cronwatch.dev/go" && /^- Version: value changed from "[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?" to "[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?"$/ { next }
{ print }
