#!/bin/sh
# kindling-sandbox se mudó a kindling/ext/sandbox en kindling v0.13.0: este instalador
# solo delega en el de kindling, que instala el núcleo y la extensión juntos.
set -eu
echo "kindling-sandbox moved to kindling/ext/sandbox (kindling v0.13.0); using kindling's installer" >&2
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh \
  | sh -s -- --with sandbox "$@"
