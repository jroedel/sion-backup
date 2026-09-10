# A machine for the self-update harness.
#
# Deliberately plain: the point is to vary one thing at a time from the outside
# (where the binary is installed, who owns it, who runs it, whether the mount
# is writable) rather than to model a particular client's laptop. The
# dimensions that need a real machine — systemd with lingering, a Windows
# scheduled task, launchd and Full Disk Access — are not here and cannot be;
# see docs/selfupdate-testing.md for what runs where.
FROM debian:stable-slim

# ca-certificates for the trust store the harness adds its CA to; the rest is
# what the assertions need. No curl, no wget: nothing in here should be able to
# fetch a release except the binary under test.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates procps \
 && rm -rf /var/lib/apt/lists/*

# The person whose files get backed up. Not root, because the interesting
# permission cases are all about a program installed by an administrator and
# run by somebody else.
RUN useradd --create-home --shell /bin/bash sion

COPY run-case /harness/run-case
COPY supervise /harness/supervise
RUN chmod 0755 /harness/run-case /harness/supervise

ENTRYPOINT ["/harness/run-case"]
