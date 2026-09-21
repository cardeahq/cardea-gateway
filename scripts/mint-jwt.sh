#!/usr/bin/env bash
# Mints an HS256 JWT for manually testing agentkeeper's identification
# stage (specs/0002). Signs with the raw bytes of the named env var, the
# same way internal/identity reads jwt_secret_env.
set -euo pipefail

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") <agent_id> <secret_env_var> [ttl_seconds]

  agent_id        value for the "sub" claim (must match an agent_id in
                   agentkeeper's config with a jwt_secret_env set)
  secret_env_var  name of the env var holding the signing secret
                   (>=32 bytes, matching that agent's jwt_secret_env)
  ttl_seconds     seconds from now until "exp" (default 3600; use a
                   negative value to mint an already-expired token)

Prints the token to stdout.

Examples:
  export AGENT_BETA_JWT_SECRET=...
  $(basename "$0") agent-beta AGENT_BETA_JWT_SECRET        # valid for 1h
  $(basename "$0") agent-beta AGENT_BETA_JWT_SECRET -3600  # expired 1h ago
EOF
  exit 1
}

[ $# -ge 2 ] || usage

agent_id=$1
secret_var=$2
ttl=${3:-3600}
secret=${!secret_var:-}

if [ -z "$secret" ]; then
  echo "error: \$$secret_var is not set or empty" >&2
  exit 1
fi

b64url() { base64 | tr -d '=\n' | tr '+/' '-_'; }

header=$(printf '%s' '{"alg":"HS256","typ":"JWT"}' | b64url)
payload=$(printf '%s' "{\"sub\":\"${agent_id}\",\"exp\":$(( $(date +%s) + ttl ))}" | b64url)
signing_input="${header}.${payload}"
sig=$(printf '%s' "$signing_input" | openssl dgst -sha256 -hmac "$secret" -binary | b64url)

echo "${signing_input}.${sig}"
