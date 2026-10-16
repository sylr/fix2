#!/bin/sh
# The QuickFIX/J agent, as make build installs it in bin/: the JVM of
# JAVA_HOME, or of PATH, with its default options and those of
# FIX2_BENCH_JAVA_OPTS. exec, so that the driver's signals reach the JVM.
# The JVM's own output, warnings included, goes to stderr: stdout is the
# driver's protocol. Options of FIX2_BENCH_JAVA_OPTS logging must log there
# too (-Xlog:gc:stderr).
dir=$(dirname "$0")
exec "${JAVA_HOME:+$JAVA_HOME/bin/}java" -Xlog:all=warning:stderr -XX:+DisplayVMOutputToStderr \
	$FIX2_BENCH_JAVA_OPTS -jar "$dir/quickfixj-agent.jar" "$@"
