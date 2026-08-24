#!/bin/sh
# Prove the Kubernetes manifests' TOPOLOGY without a cluster.
#
# kubeconform (the `kubeconform` job in .github/workflows/ci.yml) already
# validates every manifest under deploy/k8s against the Kubernetes OpenAPI
# schemas, recursively, in strict mode. That answers "is this a well-formed
# Service" and cannot answer "is this Service publicly routable", because both
# answers are perfectly valid YAML. Its own documentation says so plainly:
# server-side validation is out of scope, and so is every property that is
# about what the manifest set MEANS rather than what shape it has.
#
# This file carries those properties, and it is to deploy/k8s what
# deploy/hetzner/validate.sh is to the compose stacks. Nothing here contacts a
# cluster, a registry or a network.
#
# The properties, and why each one is worth a check rather than a review:
#
#   1. Nothing but the frontend is publicly routable. The contract topology
#      has said so since the first deployment; with a ledger in the set it
#      stops being a matter of taste, because an internet-reachable ledger API
#      is an internet-reachable way to move members' money.
#   2. The cashback directory is a genuine opt-in. If the api ever referenced
#      its ConfigMap as required, a cluster that never wanted a ledger would
#      have every api pod stuck on a missing ConfigMap.
#   3. The addresses the api is given actually resolve to Services in this
#      repository. A typo there is invisible until a member's money does not
#      move, and nothing else in CI would notice.
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

FAILS=0

fail() {
    echo "FAIL: $1"
    FAILS=1
}

# Every manifest, including the subdirectories `kubectl apply -f` does not
# recurse into. They are still part of the set this file reasons about: the
# whole point of the cashback directory is that it can be applied, and the
# whole point of examples/ is that it cannot be applied by accident.
MANIFESTS=$(find "$HERE" -name '*.yaml' | sort)

# An EMPTY list exits here rather than carrying on, and that is not tidiness.
# `fail` only records a failure and returns, so every `grep ... $MANIFESTS`
# below would then run with no file arguments - and a grep with no file reads
# STDIN and blocks forever. This script is a required gate, so that is not a
# red X, it is a job sitting on the runner until the timeout kills it, which
# reads as an infrastructure problem and gets retried rather than fixed.
if [ -z "$MANIFESTS" ]; then
    echo "FAIL: no manifests found under $HERE"
    echo "k8s topology: FAILURES"
    exit 1
fi

# ---------------------------------------------------------------------------
# 1. Nothing but the frontend is publicly routable.
# ---------------------------------------------------------------------------
# shellcheck disable=SC2086 # deliberate word splitting: a list of file paths
if grep -l -E '^[[:space:]]*type:[[:space:]]*(NodePort|LoadBalancer)' $MANIFESTS </dev/null >/dev/null 2>&1; then
    # shellcheck disable=SC2086 # deliberate word splitting: a list of file paths
    fail "a Service is typed NodePort or LoadBalancer: $(grep -l -E '^[[:space:]]*type:[[:space:]]*(NodePort|LoadBalancer)' $MANIFESTS </dev/null | tr '\n' ' ')"
else
    echo "ok: every Service is ClusterIP"
fi

# shellcheck disable=SC2086 # deliberate word splitting: a list of file paths
if grep -l -E '^[[:space:]]*(hostPort|hostNetwork):' $MANIFESTS </dev/null >/dev/null 2>&1; then
    # shellcheck disable=SC2086 # deliberate word splitting: a list of file paths
    fail "a pod binds the node's network: $(grep -l -E '^[[:space:]]*(hostPort|hostNetwork):' $MANIFESTS </dev/null | tr '\n' ' ')"
else
    echo "ok: no pod publishes a host port or joins the host network"
fi

# Exactly one Ingress, and it may name only the frontend. An Ingress that
# reached the api would expose the editorial endpoints; one that reached the
# ledger would expose the money.
# shellcheck disable=SC2086 # deliberate word splitting: a list of file paths
ingresses=$(grep -l '^kind: Ingress$' $MANIFESTS </dev/null || true)
ingress_count=$(printf '%s\n' "$ingresses" | grep -c . || true)
if [ "$ingress_count" != 1 ]; then
    fail "expected exactly one Ingress in deploy/k8s, found $ingress_count"
else
    echo "ok: there is exactly one Ingress"
    backends=$(sed -n 's/^[[:space:]]*name:[[:space:]]*\([a-z0-9-]*\)[[:space:]]*$/\1/p' "$ingresses" |
        sort -u | tr '\n' ' ')
    for name in api blnk blnk-worker redis; do
        case " $backends " in
        *" $name "*)
            fail "the Ingress names '$name'; only the frontend may be publicly routable, and a publicly reachable ledger or api is exactly what the contract topology forbids"
            ;;
        esac
    done
    echo "ok: the Ingress names neither the api nor the ledger"
fi

# ---------------------------------------------------------------------------
# 2. The cashback directory is an opt-in, not a dependency.
# ---------------------------------------------------------------------------
if grep -A2 'name: apivo-cashback-config' "$HERE/api-deployment.yaml" </dev/null | grep -q 'optional: true'; then
    echo "ok: the api treats the cashback ConfigMap as optional"
else
    fail "api-deployment.yaml does not reference apivo-cashback-config with 'optional: true'; a cluster that never applied deploy/k8s/cashback/ would have every api pod blocked on a ConfigMap it does not want"
fi

# And the base set must not reference anything the subdirectory defines.
# `kubectl apply -f deploy/k8s/` does not recurse, so a base manifest that
# needed blnk-config or the Blnk Secret key would be a base manifest that
# cannot be applied on its own.
base=$(find "$HERE" -maxdepth 1 -name '*.yaml' | sort)
# shellcheck disable=SC2086 # deliberate word splitting: a list of file paths
if grep -l -E 'name: (blnk-config|blnk|redis)$' $base </dev/null >/dev/null 2>&1; then
    # shellcheck disable=SC2086 # deliberate word splitting: a list of file paths
    fail "a base manifest references a cashback resource: $(grep -l -E 'name: (blnk-config|blnk|redis)$' $base </dev/null | tr '\n' ' ')"
else
    echo "ok: the base manifest set stands on its own"
fi

# ---------------------------------------------------------------------------
# 3. The addresses actually resolve to Services in this repository.
#
# Lifted out of the ConfigMap rather than restated here: restating them would
# only ever prove that a copy matches a copy.
# ---------------------------------------------------------------------------
CASHBACK="$HERE/cashback"

check_address() {
    # check_address <key> <configmap-file> <service-file>
    _key="$1"
    _cm="$2"
    _svc="$3"

    _value=$(sed -n "s/^[[:space:]]*$_key:[[:space:]]*\"\(.*\)\"[[:space:]]*$/\1/p" "$_cm")
    if [ -z "$_value" ]; then
        fail "$_key is not set in $_cm"
        return
    fi

    # scheme://host:port -> host and port.
    _host=$(printf '%s' "$_value" | sed -e 's#^[a-z][a-z0-9+.-]*://##' -e 's#[:/].*$##')
    _port=$(printf '%s' "$_value" | sed -e 's#^.*:##' -e 's#[^0-9].*$##')

    if [ ! -r "$_svc" ]; then
        fail "$_key names '$_host' but $_svc does not exist"
        return
    fi
    grep -q "^  name: $_host$" "$_svc" </dev/null ||
        fail "$_key resolves to '$_host', which is not the Service name in $_svc; the api would post to a name that does not resolve and nothing else in CI would notice"
    grep -q "^      port: $_port$" "$_svc" </dev/null ||
        fail "$_key uses port '$_port', which $_svc does not expose"
    echo "ok: $_key resolves to the $_host Service on port $_port"
}

check_address BLNK_URL "$CASHBACK/cashback-configmap.yaml" "$CASHBACK/blnk-service.yaml"
check_address REDIS_URL "$CASHBACK/cashback-configmap.yaml" "$CASHBACK/redis-service.yaml"
check_address BLNK_REDIS_DNS "$CASHBACK/blnk-configmap.yaml" "$CASHBACK/redis-service.yaml"

# The ledger's credential is a different Postgres role from the api's. Same
# key in both would put Blnk's migrations in `public`, which is the one thing
# spike S1 exists to prove does not happen.
if grep -q 'key: BLNK_DATA_SOURCE_DNS' "$CASHBACK/blnk-deployment.yaml" </dev/null &&
    grep -q 'key: BLNK_DATA_SOURCE_DNS' "$CASHBACK/blnk-worker-deployment.yaml" </dev/null; then
    echo "ok: both ledger Deployments take their data source from the Secret"
else
    fail "a ledger Deployment does not map BLNK_DATA_SOURCE_DNS from the Secret; the ledger's role must not be the api's"
fi

# ---------------------------------------------------------------------------
# The founder's split posture (2026-08-24): migrations as the database owner,
# the server and the worker as blnk_app, which owns nothing.
#
# Asserted rather than reviewed because a regression is invisible. A
# single-role deployment migrates, serves, and reports every pod Ready; the
# only difference is that the process exposed every second of every day can
# reshape the tables holding members' balances. Nothing fails until something
# already has.
# ---------------------------------------------------------------------------
if grep -q 'name: migrate' "$CASHBACK/blnk-deployment.yaml" </dev/null &&
    grep -q 'key: BLNK_MIGRATE_DSN' "$CASHBACK/blnk-deployment.yaml" </dev/null; then
    echo "ok: the ledger migrates in its own container, as the database owner"
else
    fail "blnk-deployment.yaml has no migrate initContainer taking BLNK_MIGRATE_DSN; the ledger would migrate with whatever role it serves with, which needs CREATE on the database permanently"
fi

# The owner's key must be ABSENT from the long-lived containers, not merely
# unused by them. A server that could read BLNK_MIGRATE_DSN out of its own
# environment makes the split buy nothing.
_leak=$(awk '
    /^        - name: / { in_init = ($0 ~ /name: migrate$/) }
    /^      containers:/ { in_init = 0 }
    # Only real secretKeyRef lines, not prose: a comment naming the key in
    # the file header is documentation, and failing on it would train the
    # next person to work around this check rather than read it.
    !in_init && /key: BLNK_MIGRATE_DSN/ { print "LEAK" }
' "$CASHBACK/blnk-deployment.yaml")
if [ -n "$_leak" ]; then
    fail "BLNK_MIGRATE_DSN reaches a long-lived container in blnk-deployment.yaml; the owner's credential belongs only to the migrate initContainer"
else
    echo "ok: the owner credential is confined to the migrate initContainer"
fi

if grep -q 'BLNK_MIGRATE_DSN' "$CASHBACK/blnk-worker-deployment.yaml" </dev/null; then
    fail "the worker maps BLNK_MIGRATE_DSN; draining a queue is DML and needs no DDL rights, and a second long-lived holder of the owner credential is a second thing to compromise"
else
    echo "ok: the worker never sees the owner credential"
fi

# `blnk start` does not migrate, so nothing may chain a migration into a
# long-lived container's command - that would run DDL as blnk_app and fail,
# or worse, be "fixed" by widening the role.
if grep -rq 'blnk migrate up &&' "$CASHBACK" </dev/null; then
    fail "a ledger container still chains 'blnk migrate up' into its command; migrations belong to the migrate initContainer, as the owner"
else
    echo "ok: migrations run only in the initContainer"
fi

# Both halves of the ledger credential, which are useless apart: one makes the
# ledger demand a credential, the other makes the api present it.
if grep -q 'key: BLNK_SERVER_SECRET_KEY' "$CASHBACK/blnk-deployment.yaml" </dev/null &&
    grep -q 'key: BLNK_SECRET_KEY' "$HERE/api-deployment.yaml" </dev/null; then
    echo "ok: the ledger demands a credential and the api presents one"
else
    fail "BLNK_SERVER_SECRET_KEY or the api's BLNK_SECRET_KEY is unmapped; a cluster runs APP_ENV=prod, where the api refuses to start with cashback enabled and no ledger credential"
fi

# The ledger is probed with a real request rather than a port knock. A
# listening socket says nothing about whether Blnk reached its database, and a
# ledger that answers TCP while failing every query is the worst of both
# states: it passes readiness and takes traffic.
if grep -q 'path: /health' "$CASHBACK/blnk-deployment.yaml" </dev/null; then
    echo "ok: the ledger's probes ask /health rather than knocking on the port"
else
    fail "blnk-deployment.yaml does not probe /health; a TCP probe passes on a ledger that answers its socket and fails every query, which is the state that takes traffic"
fi

# The WORKER too, on its own monitoring listener. The server answering says
# nothing about the process that drains the queue: a worker dead on its first
# Redis call sits Running while transactions pile up behind it.
if grep -q 'path: /health' "$CASHBACK/blnk-worker-deployment.yaml" </dev/null; then
    echo "ok: the worker is probed on its own /health as well"
else
    fail "blnk-worker-deployment.yaml does not probe /health; blnk workers serves its own health route on 5004 (cmd/workers.go startMonitoringServer), and without a probe a dead worker sits Running while transactions queue up behind it"
fi

# Every ledger image carries a digest. A tag is mutable, and this is the one
# place in the repository where that matters most: a silent retag would change
# the binary that moves members' money, with no diff and no review. The base
# manifests are exempt because their placeholder references are replaced by
# the publish pipeline, which pins digests itself.
for _f in "$CASHBACK"/blnk-deployment.yaml "$CASHBACK"/blnk-worker-deployment.yaml "$CASHBACK"/redis-deployment.yaml; do
    _img=$(sed -n 's/^ *image: *//p' "$_f")
    case "$_img" in
    *@sha256:*)
        echo "ok: $(basename "$_f") pins its image by digest"
        ;;
    "")
        fail "$_f declares no image at all"
        ;;
    *)
        fail "$_f pins '$_img' by tag only; a retag would replace the ledger binary with no diff and no review"
        ;;
    esac
done

# The ledger actually CHECKS credentials.
#
# BLNK_SERVER_SECRET_KEY does nothing on its own: in blnk v0.15.2 the auth
# middleware short-circuits before it looks at a key
# (api/middleware/auth.go: `if !conf.Server.Secure { c.Next(); return }`), and
# Server.Secure is never defaulted. A manifest set carrying only the secret
# ships a ledger that accepts anything reaching its Service while every
# document says otherwise, which is precisely the drift this file exists to
# catch.
if grep -q 'BLNK_SERVER_SECURE: *"\{0,1\}true' "$CASHBACK/blnk-configmap.yaml" </dev/null; then
    echo "ok: the ledger runs with authentication actually enabled"
else
    fail "BLNK_SERVER_SECURE is not true in blnk-configmap.yaml, so Blnk skips authentication entirely and the ledger accepts any request that reaches it - a secret key alone gates nothing"
fi

# The wait probe can write where it needs to.
#
# `readOnlyRootFilesystem: true` with no writable mount is how a probe that
# works everywhere else fails in a cluster: GNU wget writes an HSTS database
# under $HOME on exit. If that write fails the worker never starts, and the
# queue backs up behind an initContainer that was watching a perfectly healthy
# ledger.
if awk '
    # The range MUST close, and an earlier version of this did not: it set
    # inside=2 at the resources key, which is still truthy, so scanning ran on
    # into the MAIN container and its /tmp mount satisfied the check. Removing
    # the initContainer mount entirely still passed. Closing on the containers
    # key is what makes this about the initContainer at all.
    /^        - name: wait-for-ledger$/ { inside = 1; next }
    /^      containers:/ { inside = 0 }
    inside && /mountPath: \/tmp/ { found = 1 }
    END { exit(found ? 0 : 1) }
' "$CASHBACK/blnk-worker-deployment.yaml"; then
    echo "ok: the wait probe has somewhere writable"
else
    fail "the wait-for-ledger initContainer has no writable /tmp; with readOnlyRootFilesystem, wget's HSTS write fails and the worker never starts"
fi

if grep -q 'DATABASE_URL' "$CASHBACK/blnk-deployment.yaml" </dev/null; then
    fail "the ledger Deployment references DATABASE_URL — that is the api's role, and using it would let Blnk's migrations touch the public schema"
else
    echo "ok: the ledger never sees the api's database role"
fi

if [ "$FAILS" -ne 0 ]; then
    echo "k8s topology: FAILURES"
    exit 1
fi
echo "k8s topology: all checks passed"
